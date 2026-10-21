// Package srt implements SRT publishing (ingest) into the stream hub, and
// SRT reading (playback) of hub streams when enabled.
//
// Encoders connect in caller mode to srt://host:8890 with a stream ID such as
// "live/tv1?key=secret" and send MPEG-TS. Readers use a stream ID such as
// "read:live/tv1?key=<play key>" or "#!::r=live/tv1,m=request".
package srt

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/tsconv"
)

const readTimeout = 10 * time.Second

// Server accepts SRT publishers and, when AllowRead is set, readers.
type Server struct {
	Addr    string
	Latency time.Duration
	Hub     *core.Hub
	Auth    *auth.Authorizer
	Log     *slog.Logger
	// AllowRead enables reading live streams (stream ID mode "read").
	AllowRead bool

	closing chan struct{}
	ln      gosrt.Listener
	mu      sync.Mutex
	conns   map[gosrt.Conn]struct{}
	wg      sync.WaitGroup
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
	s.closing = make(chan struct{})
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
	select {
	case <-s.closing:
	default:
		close(s.closing)
	}
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
	action := "publish"
	var (
		path   string
		app    *config.App
		stream *core.Stream
	)
	if sid.Mode == ModeRead {
		action = "read"
		if !s.AllowRead {
			log.Warn("SRT read rejected: playback disabled")
			req.Reject(gosrt.REJ_PEER)
			return
		}
		path, app, err = s.Auth.AuthorizePlay(sid.Path, sid.Key)
		if err == nil {
			if stream = s.Hub.Lookup(path); stream == nil {
				err = errors.New("stream not live")
			}
		}
	} else {
		path, app, err = s.Auth.AuthorizePublish(sid.Path, sid.Key)
	}
	if err != nil {
		log.Warn("SRT "+action+" rejected", "err", err)
		req.Reject(gosrt.REJ_PEER)
		return
	}

	switch {
	case app.SRTPassphrase != "":
		if !req.IsEncrypted() {
			log.Warn("SRT " + action + " rejected: encryption required")
			req.Reject(gosrt.REJ_UNSECURE)
			return
		}
		if err := req.SetPassphrase(app.SRTPassphrase); err != nil {
			log.Warn("SRT " + action + " rejected: wrong passphrase")
			req.Reject(gosrt.REJ_BADSECRET)
			return
		}
	case req.IsEncrypted():
		log.Warn("SRT " + action + " rejected: caller uses a passphrase but the app has none")
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

	if stream != nil {
		err = s.read(conn, stream, remote)
	} else {
		err = s.publish(conn, path, remote)
	}
	log.Debug("SRT connection closed", "err", err)
}

// read sends a live stream to an SRT caller as MPEG-TS until the stream
// ends, the caller disconnects or the server shuts down.
func (s *Server) read(conn gosrt.Conn, stream *core.Stream, remote string) error {
	sub := stream.Subscribe("srt-read", 0)
	if sub == nil {
		return errors.New("stream not live")
	}
	defer sub.Close()

	cw := &countingConn{conn: conn, stream: stream}
	mux, err := tsconv.NewMuxer(&tsconv.PacketWriter{W: cw}, stream.Tracks)
	if err != nil {
		return err
	}

	// The caller sends nothing but control packets, so a returning Read
	// means the connection is gone. gosrt has no deadlines: closing the
	// connection unblocks it.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		buf := make([]byte, 1500)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	s.Log.Info("SRT reader started", "remote", remote, "path", stream.Path)
	defer s.Log.Info("SRT reader stopped", "remote", remote, "path", stream.Path)
	for {
		select {
		case <-gone:
			return io.EOF
		case <-s.closing:
			return io.EOF
		case u, ok := <-sub.C():
			if !ok {
				return io.EOF
			}
			// gosrt's Write never blocks: it fails when the send queue is
			// full, which drops callers that stopped reading.
			if err := mux.WriteUnit(u); err != nil {
				return err
			}
		}
	}
}

// countingConn adds written bytes to the stream's outgoing counter.
type countingConn struct {
	conn   gosrt.Conn
	stream *core.Stream
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.conn.Write(p)
	c.stream.BytesOut.Add(uint64(n))
	return n, err
}

func (s *Server) publish(conn gosrt.Conn, path, remote string) error {
	r := &tsconv.TimeoutReader{Conn: conn, R: conn, Timeout: readTimeout}
	err := tsconv.Demux(r, s.Hub, path, core.SourceInfo{Protocol: "srt", RemoteAddr: remote}, s.Log)
	if errors.Is(err, core.ErrAlreadyPublishing) {
		s.Log.Warn("SRT publish failed", "path", path, "err", err)
	}
	return err
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
