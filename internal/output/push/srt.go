package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	gosrt "github.com/datarhei/gosrt"

	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/tsconv"
)

type srtSession struct {
	conn      gosrt.Conn
	w         *watchdogWriter
	stream    *core.Stream
	closeOnce sync.Once
}

func dialSRT(ctx context.Context, rawURL string, s *core.Stream) (session, error) {
	// Check the tracks first: no connection is needed to know that the
	// stream cannot be muxed.
	if _, err := tsconv.NewMuxer(io.Discard, s.Tracks); err != nil {
		return nil, permanentf("stream has no track that can be carried in MPEG-TS")
	}

	cfg := gosrt.DefaultConfig()
	addr, err := cfg.UnmarshalURL(rawURL)
	if err != nil {
		return nil, permanent(errors.New("invalid srt:// URL"))
	}
	cfg.TransmissionType = "live"
	if cfg.ConnectionTimeout > connectTimeout {
		cfg.ConnectionTimeout = connectTimeout
	}
	conn, err := gosrt.DialWithContext(ctx, "srt", addr, cfg)
	if err != nil {
		return nil, err
	}
	sess := &srtSession{conn: conn, stream: s}
	sess.w = &watchdogWriter{w: conn, closer: conn, timeout: writeTimeout}
	return sess, nil
}

func (r *srtSession) send(sub *core.Subscription) error {
	m, err := tsconv.NewMuxer(&tsconv.PacketWriter{W: r.w}, r.stream.Tracks)
	if err != nil {
		return err
	}
	for u := range sub.C() {
		if err := m.WriteUnit(u); err != nil {
			// gosrt reports a closed connection or a full send queue
			// (peer not keeping up) as io.EOF.
			return fmt.Errorf("SRT write failed: %w", err)
		}
	}
	return io.EOF
}

func (r *srtSession) close() { r.closeOnce.Do(func() { r.conn.Close() }) }

func (r *srtSession) bytesSent() uint64 { return r.w.written.Load() }
