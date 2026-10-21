// Package srt implements SRT publishing (ingest) into the stream hub.
//
// Encoders connect in caller mode to srt://host:8890 with a stream ID such as
// "live/tv1?key=secret" and send MPEG-TS.
package srt

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/core"
)

const readTimeout = 10 * time.Second

// Server accepts SRT publishers.
type Server struct {
	Addr    string
	Latency time.Duration
	Hub     *core.Hub
	Auth    *auth.Authorizer
	Log     *slog.Logger

	ln    gosrt.Listener
	mu    sync.Mutex
	conns map[gosrt.Conn]struct{}
	wg    sync.WaitGroup
}

// Start begins listening. It returns once the listener is open.
func (s *Server) Start() error {
	cfg := gosrt.DefaultConfig()
	if s.Latency > 0 {
		cfg.ReceiverLatency = s.Latency
		cfg.PeerLatency = s.Latency
	}
	ln, err := gosrt.Listen("srt", s.Addr, cfg)
	if err != nil {
		return err
	}
	s.ln = ln
	s.conns = make(map[gosrt.Conn]struct{})
	s.Log.Info("SRT ingest listening", "addr", ln.Addr().String())

	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// ListenAddr returns the bound address.
func (s *Server) ListenAddr() string { return s.ln.Addr().String() }

// Close stops the listener and disconnects every publisher.
func (s *Server) Close() {
	if s.ln == nil {
		return
	}
	s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		req, err := s.ln.Accept2()
		if err != nil {
			if !errors.Is(err, gosrt.ErrListenerClosed) {
				s.Log.Error("SRT accept failed", "err", err)
			}
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleRequest(req)
		}()
	}
}

func (s *Server) handleRequest(req gosrt.ConnRequest) {
	remote := req.RemoteAddr().String()
	log := s.Log.With("remote", remote, "streamid", redact(req.StreamId()))

	sid, err := ParseStreamID(req.StreamId())
	if err != nil {
		log.Warn("SRT connection rejected", "err", err)
		req.Reject(gosrt.REJ_PEER)
		return
	}
	if sid.Mode != ModePublish {
		log.Warn("SRT playback is not supported yet, use HLS")
		req.Reject(gosrt.REJ_PEER)
		return
	}

	path, app, err := s.Auth.AuthorizePublish(sid.Path, sid.Key)
	if err != nil {
		log.Warn("SRT publish rejected", "err", err)
		req.Reject(gosrt.REJ_PEER)
		return
	}

	switch {
	case app.SRTPassphrase != "":
		if !req.IsEncrypted() {
			log.Warn("SRT publish rejected: encryption required")
			req.Reject(gosrt.REJ_UNSECURE)
			return
		}
		if err := req.SetPassphrase(app.SRTPassphrase); err != nil {
			log.Warn("SRT publish rejected: wrong passphrase")
			req.Reject(gosrt.REJ_BADSECRET)
			return
		}
	case req.IsEncrypted():
		log.Warn("SRT publish rejected: caller uses a passphrase but the app has none")
		req.Reject(gosrt.REJ_UNSECURE)
		return
	}

	conn, err := req.Accept()
	if err != nil {
		log.Warn("SRT accept failed", "err", err)
		return
	}
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		conn.Close()
	}()

	err = s.publish(conn, path, remote)
	log.Debug("SRT connection closed", "err", err)
}

func (s *Server) publish(conn gosrt.Conn, path, remote string) error {
	conn.SetReadDeadline(time.Now().Add(readTimeout)) //nolint:errcheck

	r := &mpegts.Reader{R: mpegts.NewBufferedReader(conn)}
	if err := r.Initialize(); err != nil {
		return fmt.Errorf("reading MPEG-TS: %w", err)
	}
	r.OnDecodeError(func(err error) {
		s.Log.Debug("MPEG-TS decode error", "path", path, "err", err)
	})

	var stream *core.Stream
	var tracks []*core.Track
	td := &mpegts.TimeDecoder{}
	td.Initialize()

	for _, mt := range r.Tracks() {
		if t := s.attachTrack(r, mt, td, &stream); t != nil {
			tracks = append(tracks, t)
		}
	}

	stream, err := s.Hub.Publish(path, tracks, core.SourceInfo{Protocol: "srt", RemoteAddr: remote})
	if err != nil {
		s.Log.Warn("SRT publish failed", "path", path, "err", err)
		return err
	}
	defer s.Hub.Unpublish(stream)

	for {
		conn.SetReadDeadline(time.Now().Add(readTimeout)) //nolint:errcheck
		if err := r.Read(); err != nil {
			return err
		}
	}
}

func ticksToDuration(t int64) time.Duration {
	return time.Duration(t) * time.Second / 90000
}

// attachTrack converts an MPEG-TS track into a hub track and registers a
// data callback. It returns nil for unsupported codecs.
func (s *Server) attachTrack(r *mpegts.Reader, mt *mpegts.Track, td *mpegts.TimeDecoder, stream **core.Stream) *core.Track {
	switch c := mt.Codec.(type) {
	case *mpegts.CodecH264:
		t := core.NewTrack(&core.H264{})
		keeper := core.NewParamKeeper(t.Codec)
		r.OnDataH264(mt, func(pts, dts int64, au [][]byte) error {
			writeVideo(*stream, t, keeper, td.Decode(pts), td.Decode(dts), au)
			return nil
		})
		return t

	case *mpegts.CodecH265:
		t := core.NewTrack(&core.H265{})
		keeper := core.NewParamKeeper(t.Codec)
		r.OnDataH265(mt, func(pts, dts int64, au [][]byte) error {
			writeVideo(*stream, t, keeper, td.Decode(pts), td.Decode(dts), au)
			return nil
		})
		return t

	case *mpegts.CodecMPEG4Audio:
		cfg := c.Config
		t := core.NewTrack(&core.AAC{Config: &cfg})
		r.OnDataMPEG4Audio(mt, func(pts int64, aus [][]byte) error {
			(*stream).WriteUnit(&core.Unit{
				Track: t, NTP: time.Now(), PTS: ticksToDuration(td.Decode(pts)), Payload: aus,
			})
			return nil
		})
		return t

	case *mpegts.CodecOpus:
		ch := 2
		if c.Desc != nil && c.Desc.ExplicitChannelCount > 0 {
			ch = c.Desc.ExplicitChannelCount
		} else if c.ChannelCount > 0 { //nolint:staticcheck
			ch = c.ChannelCount //nolint:staticcheck
		}
		t := core.NewTrack(&core.Opus{ChannelCount: ch})
		r.OnDataOpus(mt, func(pts int64, packets [][]byte) error {
			(*stream).WriteUnit(&core.Unit{
				Track: t, NTP: time.Now(), PTS: ticksToDuration(td.Decode(pts)), Payload: packets,
			})
			return nil
		})
		return t
	}

	s.Log.Warn("SRT track ignored: codec not supported yet", "codec", fmt.Sprintf("%T", mt.Codec))
	return nil
}

func writeVideo(stream *core.Stream, t *core.Track, k *core.ParamKeeper, pts, dts int64, au [][]byte) {
	au, key := k.Process(au)
	if au == nil {
		return
	}
	stream.WriteUnit(&core.Unit{
		Track:        t,
		NTP:          time.Now(),
		PTS:          ticksToDuration(pts),
		DTS:          ticksToDuration(dts),
		Payload:      au,
		RandomAccess: key,
	})
}

// redact hides publish keys in logged stream IDs.
func redact(sid string) string {
	parsed, err := ParseStreamID(sid)
	if err != nil || parsed.Key == "" {
		return sid
	}
	mode := "publish"
	if parsed.Mode == ModeRead {
		mode = "read"
	}
	return mode + ":" + parsed.Path + "?key=REDACTED"
}
