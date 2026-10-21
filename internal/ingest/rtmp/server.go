// Package rtmp implements RTMP publishing (ingest) into the stream hub.
//
// Encoders connect to rtmp://host:1935/<app> and publish with the stream key
// "<stream>" or "<stream>?key=<publish key>".
package rtmp

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/codecs"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/core"
)

const (
	handshakeTimeout = 10 * time.Second
	readTimeout      = 10 * time.Second
)

// Server accepts RTMP publishers.
type Server struct {
	Addr string
	Hub  *core.Hub
	Auth *auth.Authorizer
	Log  *slog.Logger

	ln    net.Listener
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// Start begins listening. It returns once the listener is open.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.conns = make(map[net.Conn]struct{})
	s.Log.Info("RTMP ingest listening", "addr", ln.Addr().String())

	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// ListenAddr returns the bound address (useful when Addr uses port 0).
func (s *Server) ListenAddr() net.Addr { return s.ln.Addr() }

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
		conn, err := s.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.Log.Error("RTMP accept failed", "err", err)
			}
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				conn.Close()
			}()
			err := s.handleConn(conn)
			s.Log.Debug("RTMP connection closed", "remote", conn.RemoteAddr().String(), "err", err)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) error {
	remote := conn.RemoteAddr().String()
	conn.SetReadDeadline(time.Now().Add(handshakeTimeout)) //nolint:errcheck

	sc := &gortmplib.ServerConn{RW: conn}
	if err := sc.Initialize(); err != nil {
		return err
	}
	if err := sc.AcceptConn(); err != nil {
		return err
	}

	if !sc.Publish {
		sc.RejectAction() //nolint:errcheck
		s.Log.Warn("RTMP playback is not supported yet, use HLS", "remote", remote, "path", sc.URL.Path)
		return errors.New("playback not supported")
	}

	path, _, err := s.Auth.AuthorizePublish(sc.URL.Path, sc.URL.Query().Get("key"))
	if err != nil {
		sc.RejectAction() //nolint:errcheck
		s.Log.Warn("RTMP publish rejected", "remote", remote, "path", sc.URL.Path, "err", err)
		return err
	}

	r := &gortmplib.Reader{Conn: sc}
	if err := r.Initialize(); err != nil {
		return err
	}

	var stream *core.Stream
	tracks := AttachTracks(r, &stream, s.Log)

	stream, err = s.Hub.Publish(path, tracks, core.SourceInfo{Protocol: "rtmp", RemoteAddr: remote})
	if err != nil {
		s.Log.Warn("RTMP publish failed", "remote", remote, "path", path, "err", err)
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

// AttachTracks converts the tracks of an initialized RTMP reader into hub
// tracks and registers data callbacks that write into *stream. Set *stream
// (by publishing the returned tracks) before calling r.Read(): callbacks
// only fire from Read. Unsupported codecs are logged and skipped.
//
// It is used for RTMP publishers and for pulling rtmp:// sources.
func AttachTracks(r *gortmplib.Reader, stream **core.Stream, log *slog.Logger) []*core.Track {
	var tracks []*core.Track
	for _, rt := range r.Tracks() {
		if t := attachTrack(r, rt, stream); t != nil {
			tracks = append(tracks, t)
		} else {
			log.Warn("RTMP track ignored: codec not supported yet", "codec", fmt.Sprintf("%T", rt.Codec))
		}
	}
	return tracks
}

func attachTrack(r *gortmplib.Reader, rt *gortmplib.Track, stream **core.Stream) *core.Track {
	switch c := rt.Codec.(type) {
	case *codecs.H264:
		t := core.NewTrack(&core.H264{SPS: c.SPS, PPS: c.PPS})
		keeper := core.NewParamKeeper(t.Codec)
		r.OnDataH264(rt, func(pts, dts time.Duration, au [][]byte) {
			writeVideo(*stream, t, keeper, pts, dts, au)
		})
		return t

	case *codecs.H265:
		t := core.NewTrack(&core.H265{VPS: c.VPS, SPS: c.SPS, PPS: c.PPS})
		keeper := core.NewParamKeeper(t.Codec)
		r.OnDataH265(rt, func(pts, dts time.Duration, au [][]byte) {
			writeVideo(*stream, t, keeper, pts, dts, au)
		})
		return t

	case *codecs.MPEG4Audio:
		t := core.NewTrack(&core.AAC{Config: c.Config})
		r.OnDataMPEG4Audio(rt, func(pts time.Duration, au []byte) {
			(*stream).WriteUnit(&core.Unit{Track: t, NTP: time.Now(), PTS: pts, Payload: [][]byte{au}})
		})
		return t

	case *codecs.Opus:
		ch := 2
		if c.IDHeader != nil {
			ch = int(c.IDHeader.ChannelCount)
		}
		t := core.NewTrack(&core.Opus{ChannelCount: ch})
		r.OnDataOpus(rt, func(pts time.Duration, pkt []byte) {
			(*stream).WriteUnit(&core.Unit{Track: t, NTP: time.Now(), PTS: pts, Payload: [][]byte{pkt}})
		})
		return t

	case *codecs.MPEG1Audio:
		t := core.NewTrack(&core.MPEGAudio{})
		r.OnDataMPEG1Audio(rt, func(pts time.Duration, frame []byte) {
			(*stream).WriteUnit(&core.Unit{Track: t, NTP: time.Now(), PTS: pts, Payload: [][]byte{frame}})
		})
		return t

	case *codecs.AC3:
		t := core.NewTrack(&core.AC3{SampleRate: c.SampleRate, ChannelCount: c.ChannelCount})
		r.OnDataAC3(rt, func(pts time.Duration, frame []byte) {
			(*stream).WriteUnit(&core.Unit{Track: t, NTP: time.Now(), PTS: pts, Payload: [][]byte{frame}})
		})
		return t
	}

	return nil
}

func writeVideo(stream *core.Stream, t *core.Track, k *core.ParamKeeper, pts, dts time.Duration, au [][]byte) {
	au, key := k.Process(au)
	if au == nil {
		return
	}
	stream.WriteUnit(&core.Unit{
		Track:        t,
		NTP:          time.Now(),
		PTS:          pts,
		DTS:          dts,
		Payload:      au,
		RandomAccess: key,
	})
}
