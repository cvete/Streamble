// Package rtmp implements RTMP publishing (ingest) into the stream hub, and
// RTMP playback of hub streams when enabled.
//
// Encoders connect to rtmp://host:1935/<app> and publish with the stream key
// "<stream>" or "<stream>?key=<publish key>". Players open
// rtmp://host:1935/<app>/<stream>, adding "?key=<play key>" when the app
// has one.
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
	"github.com/cvete/streamble/internal/rtmpconv"
)

const (
	handshakeTimeout = 10 * time.Second
	readTimeout      = 10 * time.Second
	writeTimeout     = 10 * time.Second
	// bytesOutInterval is how often bytes sent to players are added to the
	// stream's outgoing counter.
	bytesOutInterval = 2 * time.Second
)

// Server accepts RTMP publishers and, when AllowPlay is set, players.
type Server struct {
	Addr string
	Hub  *core.Hub
	Auth *auth.Authorizer
	Log  *slog.Logger
	// AllowPlay enables RTMP playback of live streams.
	AllowPlay bool
	// AllowPlayFunc, when set, is consulted instead of AllowPlay on every
	// play request, so the setting can change at runtime.
	AllowPlayFunc func() bool

	ln      net.Listener
	closing chan struct{}
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	wg      sync.WaitGroup
}

// Start begins listening. It returns once the listener is open.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.conns = make(map[net.Conn]struct{})
	s.closing = make(chan struct{})
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

	sc := &gortmplib.ServerConn{RW: &deadlineWriter{Conn: conn}}
	if err := sc.Initialize(); err != nil {
		return err
	}
	if err := sc.AcceptConn(); err != nil {
		return err
	}

	if !sc.Publish {
		return s.play(conn, sc, remote)
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

	stream, err = s.Hub.Publish(path, tracks, core.SourceInfo{
		Protocol: "rtmp", RemoteAddr: remote, Kick: func() { conn.Close() }, //nolint:errcheck
	})
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

func (s *Server) playAllowed() bool {
	if s.AllowPlayFunc != nil {
		return s.AllowPlayFunc()
	}
	return s.AllowPlay
}

// play sends a live stream to an RTMP player.
func (s *Server) play(conn net.Conn, sc *gortmplib.ServerConn, remote string) error {
	if !s.playAllowed() {
		sc.RejectAction() //nolint:errcheck
		s.Log.Warn("RTMP playback rejected: disabled", "remote", remote, "path", sc.URL.Path)
		return errors.New("playback disabled")
	}

	path, _, err := s.Auth.AuthorizePlay(sc.URL.Path, sc.URL.Query().Get("key"))
	if err != nil {
		sc.RejectAction() //nolint:errcheck
		s.Log.Warn("RTMP playback rejected", "remote", remote, "path", sc.URL.Path, "err", err)
		return err
	}

	var sub *core.Subscription
	stream := s.Hub.Lookup(path)
	if stream != nil {
		sub = stream.Subscribe("rtmp-play", 0)
	}
	if sub == nil {
		sc.RejectAction() //nolint:errcheck
		s.Log.Warn("RTMP playback rejected: stream not live", "remote", remote, "path", path)
		return errors.New("stream not live")
	}
	defer sub.Close()

	// Players send little after the play command (acknowledgements,
	// pings), so a read timeout would drop healthy players. Dead players
	// are detected by the per-write deadline instead.
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	done := make(chan struct{})
	counted := make(chan struct{})
	go func() {
		defer close(counted)
		s.countBytesOut(sc, stream, sub, done)
	}()

	s.Log.Info("RTMP player started", "remote", remote, "path", path)
	err = rtmpconv.Send(sc, stream, sub)
	close(done)
	<-counted
	s.Log.Info("RTMP player stopped", "remote", remote, "path", path)
	return err
}

// countBytesOut adds the bytes sent to a player to the stream's outgoing
// counter until done is closed. On server shutdown it ends the
// subscription, so a player waiting for media does not block Close.
func (s *Server) countBytesOut(sc *gortmplib.ServerConn, stream *core.Stream, sub *core.Subscription,
	done <-chan struct{},
) {
	last := sc.BytesSent()
	update := func() {
		n := sc.BytesSent()
		stream.BytesOut.Add(n - last)
		last = n
	}
	t := time.NewTicker(bytesOutInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			update()
		case <-s.closing:
			sub.Close()
			<-done
			update()
			return
		case <-done:
			update()
			return
		}
	}
}

// deadlineWriter sets a write deadline before every write, so a peer that
// stops reading is disconnected instead of blocking a goroutine forever.
type deadlineWriter struct {
	net.Conn
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	d.Conn.SetWriteDeadline(time.Now().Add(writeTimeout)) //nolint:errcheck
	return d.Conn.Write(p)
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
