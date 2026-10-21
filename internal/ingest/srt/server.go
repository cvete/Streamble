// Package srt implements SRT publishing (ingest) into the stream hub.
//
// Encoders connect in caller mode to srt://host:8890 with a stream ID such as
// "live/tv1?key=secret" and send MPEG-TS.
package srt

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/tsconv"
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
