package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg1audio"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/ingest/srt"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/output/push"
)

// pushServer is one in-process Streamble instance: hub, RTMP and SRT
// ingest, HLS output and the push manager.
type pushServer struct {
	hub      *core.Hub
	push     *push.Manager
	rtmp     *rtmp.Server
	authz    *auth.Authorizer
	log      *slog.Logger
	rtmpAddr string
	srtAddr  string
	http     *httptest.Server
}

func pushStartServer(t *testing.T, apps ...config.App) *pushServer {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	cfg := config.Default()
	cfg.HLS.Variants = []string{config.VariantMPEGTS}
	cfg.HLS.Primary = config.VariantMPEGTS
	cfg.HLS.SegmentDuration = time.Second
	cfg.Apps = apps
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(pushLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ps := &pushServer{hub: core.NewHub(log), authz: auth.New(cfg), log: log}
	hm := hls.NewManager(cfg.HLS, "*", log)
	ps.hub.AddHook(hm)
	ps.push = push.NewManager(cfg, log)
	ps.hub.AddHook(ps.push)
	t.Cleanup(ps.push.Close)

	ps.startRTMP(t, "127.0.0.1:0")

	ss := &srt.Server{Addr: "127.0.0.1:" + freeUDPPort(t), Hub: ps.hub, Auth: ps.authz, Log: log}
	if err := ss.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ss.Close)
	ps.srtAddr = ss.ListenAddr()

	ps.http = httptest.NewServer(hm)
	t.Cleanup(ps.http.Close)
	return ps
}

func (ps *pushServer) startRTMP(t *testing.T, addr string) {
	t.Helper()
	var err error
	for range 50 { // the port may take a moment to be free again after a restart
		rs := &rtmp.Server{Addr: addr, Hub: ps.hub, Auth: ps.authz, Log: ps.log}
		if err = rs.Start(); err == nil {
			ps.rtmp = rs
			ps.rtmpAddr = rs.ListenAddr().String()
			t.Cleanup(rs.Close)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(err)
}

// pushPublish runs FFmpeg in the background and returns a function that
// stops it.
func pushPublish(t *testing.T, args ...string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	base := []string{"-hide_banner", "-loglevel", "error", "-re"}
	cmd := exec.CommandContext(ctx, "ffmpeg", append(base, args...)...)
	cmd.Stderr = pushLogWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			cmd.Wait() //nolint:errcheck
		})
	}
	t.Cleanup(stop)
	return stop
}

// pushPublishKeep is pushPublish for tests that only need a stream to be
// there: FFmpeg is restarted if it exits early (an SRT publisher can be
// dropped when the machine is overloaded by -race and parallel packages).
func pushPublishKeep(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			base := []string{"-hide_banner", "-loglevel", "error", "-re"}
			cmd := exec.CommandContext(ctx, "ffmpeg", append(base, args...)...)
			cmd.Stderr = pushLogWriter{t}
			if cmd.Run() != nil && ctx.Err() == nil {
				time.Sleep(200 * time.Millisecond)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func pushWaitStream(t *testing.T, hub *core.Hub, path string) *core.Stream {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s := hub.Lookup(path); s != nil {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("stream %s did not arrive", path)
	return nil
}

func pushCodecs(s *core.Stream) string {
	var out []string
	for _, tr := range s.Tracks {
		out = append(out, tr.Codec.Name())
	}
	return strings.Join(out, ",")
}

// pushCountUnits subscribes to s for d and checks that video keyframes and
// audio both flow.
func pushCountUnits(t *testing.T, s *core.Stream, d time.Duration, wantVideo, wantAudio bool) {
	t.Helper()
	sub := s.Subscribe("test", 4096)
	if sub == nil {
		t.Fatal("stream ended")
	}
	defer sub.Close()
	var video, keys, audio int
	timeout := time.After(d)
loop:
	for {
		select {
		case u, ok := <-sub.C():
			if !ok {
				break loop
			}
			if u.Track.Codec.IsVideo() {
				video++
				if u.RandomAccess {
					keys++
				}
			} else {
				audio += len(u.Payload) // MPEG-TS sources batch audio frames
			}
		case <-timeout:
			break loop
		}
	}
	if wantVideo && (video < 25 || keys < 2) {
		t.Fatalf("too little video: %d units, %d keyframes", video, keys)
	}
	if wantAudio && audio < 20 {
		t.Fatalf("too little audio: %d frames", audio)
	}
}

// pushWaitStatus waits until the push manager reports a target of stream
// in the given state and returns it.
func pushWaitStatus(t *testing.T, m *push.Manager, stream, name, state string) push.Status {
	t.Helper()
	var last []push.Status
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		last = m.Status()
		for _, st := range last {
			if st.Stream == stream && st.Name == name && st.State == state {
				return st
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("target %s of %s never reached %q: %+v", name, stream, state, last)
	return push.Status{}
}

func TestPushRTMPToRTMP(t *testing.T) {
	dst := pushStartServer(t, config.App{Name: "live", PublishKey: "destkey"})
	src := pushStartServer(t, config.App{Name: "live", PublishKey: "srckey", Push: []config.Push{{
		Name: "other-server",
		URL:  "rtmp://" + dst.rtmpAddr + "/live/{stream}?key=destkey",
	}}})

	pushPublish(t, join(av264AAC, []string{"-f", "flv", "rtmp://" + src.rtmpAddr + "/live/tv1?key=srckey"})...)
	s := pushWaitStream(t, dst.hub, "live/tv1")
	if c := pushCodecs(s); c != "H264,AAC" {
		t.Fatalf("unexpected tracks at destination: %s", c)
	}
	pushCountUnits(t, s, 3*time.Second, true, true)
	play(t, dst.http.URL+"/live/tv1/index.m3u8")

	st := pushWaitStatus(t, src.push, "live/tv1", "other-server", push.StateLive)
	if st.BytesSent == 0 || strings.Contains(st.URL, "destkey") || !strings.Contains(st.URL, "REDACTED") {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestPushSRT(t *testing.T) {
	dst := pushStartServer(t,
		config.App{Name: "live", PublishKey: "destkey"},
		config.App{Name: "secure", SRTPassphrase: "passphrase123"},
	)
	src := pushStartServer(t, config.App{Name: "live", Push: []config.Push{
		// SRT with passphrase to an encrypted app.
		{Name: "srt", URL: "srt://" + dst.srtAddr + "?streamid=secure/{stream}&passphrase=passphrase123"},
		// SRT source (MPEG-TS, parameter sets only in-band) out over RTMP.
		{Name: "rtmp", URL: "rtmp://" + dst.rtmpAddr + "/live/{stream}-rtmp?key=destkey"},
	}})

	pushPublishKeep(t, join(av264AAC, []string{"-f", "mpegts",
		"srt://" + src.srtAddr + "?streamid=live/tv1&pkt_size=1316"})...)

	pushWaitStatus(t, src.push, "live/tv1", "srt", push.StateLive)
	viaSRT := pushWaitStream(t, dst.hub, "secure/tv1")
	if c := pushCodecs(viaSRT); c != "H264,AAC" {
		t.Fatalf("unexpected tracks over SRT: %s", c)
	}
	viaRTMP := pushWaitStream(t, dst.hub, "live/tv1-rtmp")
	if c := pushCodecs(viaRTMP); c != "H264,AAC" {
		t.Fatalf("unexpected tracks over RTMP: %s", c)
	}
	pushCountUnits(t, viaSRT, 3*time.Second, true, true)
	pushCountUnits(t, viaRTMP, 3*time.Second, true, true)

	st := pushWaitStatus(t, src.push, "live/tv1", "srt", push.StateLive)
	if strings.Contains(st.URL, "passphrase123") || st.BytesSent == 0 {
		t.Fatalf("unexpected status: %+v", st)
	}
}

// pushIcecastServer imitates the Icecast 2.4 source protocol: it checks the
// PUT request, answers 200 and records the raw body until the source
// disconnects.
type pushIcecastServer struct {
	*httptest.Server
	mu          sync.Mutex
	body        bytes.Buffer
	contentType string
	errs        []string
}

func pushStartIcecast(t *testing.T) *pushIcecastServer {
	ic := &pushIcecastServer{}
	ic.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("source:hackme"))
		switch {
		case r.Method != http.MethodPut || r.URL.Path != "/live.mp3":
			ic.fail("unexpected request " + r.Method + " " + r.URL.Path)
			http.Error(w, "bad mount", http.StatusNotFound)
			return
		case r.Header.Get("Authorization") != wantAuth:
			ic.fail("bad credentials")
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		case r.Header.Get("Ice-Public") != "0" || r.Header.Get("Ice-Name") == "":
			ic.fail("missing Ice headers")
		}
		ic.mu.Lock()
		ic.contentType = r.Header.Get("Content-Type")
		ic.mu.Unlock()

		// Icecast sources send the body until they disconnect, without
		// Content-Length or chunking, so read it from the raw connection.
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			ic.fail(err.Error())
			return
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
			return
		}
		buf := make([]byte, 4096)
		for {
			n, err := rw.Read(buf)
			ic.mu.Lock()
			ic.body.Write(buf[:n])
			ic.mu.Unlock()
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(ic.Close)
	return ic
}

func (ic *pushIcecastServer) fail(msg string) {
	ic.mu.Lock()
	ic.errs = append(ic.errs, msg)
	ic.mu.Unlock()
}

// waitBody waits until at least n bytes arrived and returns a copy.
func (ic *pushIcecastServer) waitBody(t *testing.T, n int) ([]byte, string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ic.mu.Lock()
		if len(ic.errs) > 0 {
			t.Fatalf("icecast server: %v", ic.errs)
		}
		if ic.body.Len() >= n {
			b := append([]byte(nil), ic.body.Bytes()...)
			ct := ic.contentType
			ic.mu.Unlock()
			return b, ct
		}
		ic.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("icecast server received too little data")
	return nil, ""
}

func pushIcecastURL(ic *pushIcecastServer) string {
	return "icecast://source:hackme@" + strings.TrimPrefix(ic.URL, "http://") + "/live.mp3"
}

func pushNeedEncoder(t *testing.T, name string) {
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if !strings.Contains(string(out), name) {
		t.Skip("ffmpeg has no " + name)
	}
}

func TestPushIcecastMP3(t *testing.T) {
	pushNeedEncoder(t, "libmp3lame")
	ic := pushStartIcecast(t)
	src := pushStartServer(t, config.App{Name: "radio", Push: []config.Push{{Name: "icecast", URL: pushIcecastURL(ic)}}})

	pushPublish(t, join(inAudio, []string{"-c:a", "libmp3lame", "-b:a", "128k", "-f", "mpegts",
		"srt://" + src.srtAddr + "?streamid=radio/fm&pkt_size=1316"})...)

	body, ct := ic.waitBody(t, 32*1024)
	if ct != "audio/mpeg" {
		t.Fatalf("content type %q", ct)
	}
	// The body must be a clean sequence of MP3 frames.
	frames := 0
	for pos := 0; pos+5 <= len(body); frames++ {
		var h mpeg1audio.FrameHeader
		if err := h.Unmarshal(body[pos:]); err != nil {
			t.Fatalf("invalid MP3 frame %d at %d: %v", frames, pos, err)
		}
		if h.Layer != 3 {
			t.Fatalf("frame %d is layer %d", frames, h.Layer)
		}
		pos += h.FrameLen()
	}
	if frames < 50 {
		t.Fatalf("only %d MP3 frames", frames)
	}
	st := pushWaitStatus(t, src.push, "radio/fm", "icecast", push.StateLive)
	if strings.Contains(st.URL, "hackme") || st.BytesSent == 0 {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestPushIcecastAAC(t *testing.T) {
	ic := pushStartIcecast(t)
	src := pushStartServer(t, config.App{Name: "radio", Push: []config.Push{{Name: "icecast", URL: pushIcecastURL(ic)}}})

	pushPublish(t, join(audioAAC, []string{"-f", "flv", "rtmp://" + src.rtmpAddr + "/radio/aac"})...)
	body, ct := ic.waitBody(t, 16*1024)
	if ct != "audio/aac" {
		t.Fatalf("content type %q", ct)
	}
	frames := 0
	for pos := 0; pos+7 <= len(body); frames++ {
		if body[pos] != 0xff || body[pos+1]&0xf6 != 0xf0 {
			t.Fatalf("no ADTS sync word at %d (frame %d)", pos, frames)
		}
		pos += int(body[pos+3]&0x03)<<11 | int(body[pos+4])<<3 | int(body[pos+5]>>5)
	}
	if frames < 50 {
		t.Fatalf("only %d ADTS frames", frames)
	}
}

func TestPushReconnectAndStreamEnd(t *testing.T) {
	dst := pushStartServer(t, config.App{Name: "live", PublishKey: "destkey"})
	src := pushStartServer(t, config.App{Name: "live", Push: []config.Push{{
		Name: "dst", URL: "rtmp://" + dst.rtmpAddr + "/live/{stream}?key=destkey",
	}}})

	stop := pushPublish(t, join(video264, []string{"-f", "flv", "rtmp://" + src.rtmpAddr + "/live/cam"})...)
	pushWaitStream(t, dst.hub, "live/cam")
	pushWaitStatus(t, src.push, "live/cam", "dst", push.StateLive)

	// The destination goes away...
	addr := dst.rtmpAddr
	dst.rtmp.Close()
	pushWaitStatus(t, src.push, "live/cam", "dst", push.StateRetrying)
	if dst.hub.Lookup("live/cam") != nil {
		t.Fatal("destination stream still live")
	}

	// ...and comes back on the same address.
	dst.startRTMP(t, addr)
	s := pushWaitStream(t, dst.hub, "live/cam")
	pushCountUnits(t, s, 2*time.Second, true, false)
	st := pushWaitStatus(t, src.push, "live/cam", "dst", push.StateLive)
	if st.Reconnects < 1 {
		t.Fatalf("reconnects not counted: %+v", st)
	}

	// The source stream ends: the push stops and its status disappears.
	stop()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(src.push.Status()) == 0 && dst.hub.Lookup("live/cam") == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("push still running after stream end: %+v", src.push.Status())
}

// pushLogWriter sends server logs and FFmpeg errors to the test log.
type pushLogWriter struct{ t *testing.T }

// Write logs through t.Log, which is safe from any goroutine; output is
// shown for failing tests only (or with -v).
func (w pushLogWriter) Write(b []byte) (int, error) {
	defer func() { _ = recover() }() // t.Log panics once the test has ended
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}
