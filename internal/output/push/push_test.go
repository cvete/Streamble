package push

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
)

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Apps = []config.App{
		{Name: "live", Push: []config.Push{
			{Name: "all", URL: "rtmp://a.example/live2/KEY"},
			{Name: "only-tv1", URL: "srt://b.example:9000?streamid={app}/{stream}", Streams: []string{"tv1"}},
			{Name: "rendition", URL: "rtmp://c.example/app/{stream}", Streams: []string{"tv1_720p"}},
		}},
		{Name: "radio"},
	}
	return cfg
}

func names(ps []config.Push) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return strings.Join(out, ",")
}

func newStream(t *testing.T, hub *core.Hub, path, protocol string, tracks ...*core.Track) *core.Stream {
	t.Helper()
	if len(tracks) == 0 {
		tracks = []*core.Track{core.NewTrack(&core.H264{})}
	}
	s, err := hub.Publish(path, tracks, core.SourceInfo{Protocol: protocol})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSelectPushes(t *testing.T) {
	cfg := testConfig()
	hub := core.NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		path, protocol, want string
	}{
		{"live/tv1", "rtmp", "all,only-tv1"},
		{"live/tv2", "srt", "all"},
		{"live/tv1_720p", "transcode", "rendition"}, // only explicit
		{"live/tv1_480p", "transcode", ""},          // not listed anywhere
		{"live/tv1_720p", "rtmp", "all,rendition"},  // a real publisher with that name
		{"radio/fm", "rtmp", ""},                    // app without targets
		{"other/x", "rtmp", ""},                     // unknown app
	}
	for _, c := range cases {
		s := newStream(t, hub, c.path, c.protocol)
		_, _, got := selectPushes(cfg, s)
		if names(got) != c.want {
			t.Errorf("%s (%s): got %q, want %q", c.path, c.protocol, names(got), c.want)
		}
		hub.Unpublish(s)
	}
}

func TestExpand(t *testing.T) {
	got := expand("srt://h:1?streamid=publish:{app}/{stream}&x={stream}", "live", "tv1")
	if got != "srt://h:1?streamid=publish:live/tv1&x=tv1" {
		t.Fatal(got)
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"rtmp://a.rtmp.youtube.com/live2/abcd-efgh-ijkl":             "rtmp://a.rtmp.youtube.com/live2/REDACTED",
		"rtmps://live-api-s.facebook.com:443/rtmp/FB-123?s_bl=1&a=b": "rtmps://live-api-s.facebook.com:443/rtmp/REDACTED?s_bl=REDACTED&a=REDACTED",
		"rtmp://host:1935/live/tv1?key=secret":                       "rtmp://host:1935/live/REDACTED?key=REDACTED",
		"rtmp://user:pw@host/app/key#frag":                           "rtmp://host/app/REDACTED#REDACTED",
		"rtmp://host/app":                                            "rtmp://host/app",
		"srt://host:9000?streamid=live/tv1&passphrase=supersecret":   "srt://host:9000?streamid=REDACTED&passphrase=REDACTED",
		"icecast://source:hackme@radio.example:8000/live.mp3":        "icecast://radio.example:8000/live.mp3",
		"icecasts://radio.example/stream":                            "icecasts://radio.example/stream",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := redactURL("rtmp://bad host/%zz"); strings.Contains(got, "bad host") {
		t.Errorf("unparsable URL leaked: %q", got)
	}
}

func TestScrub(t *testing.T) {
	raw := "rtmp://u:pw123@host/live2/MYKEY?token=t0k3n"
	msg := scrub("dial rtmp://u:pw123@host/live2/MYKEY?token=t0k3n failed (pw123, MYKEY, t0k3n)", secretsOf(raw))
	for _, s := range []string{"pw123", "MYKEY", "t0k3n"} {
		if strings.Contains(msg, s) {
			t.Fatalf("%q leaked in %q", s, msg)
		}
	}
}

func TestTargetName(t *testing.T) {
	if n := targetName(config.Push{Name: "YouTube"}, "rtmp://x/y/z"); n != "YouTube" {
		t.Fatal(n)
	}
	if n := targetName(config.Push{}, "rtmp://user:pw@a.rtmp.youtube.com:1935/live2/KEY"); n != "rtmp://a.rtmp.youtube.com:1935" {
		t.Fatal(n)
	}
}

func TestRTMPURLSplit(t *testing.T) {
	cases := []struct{ in, path, key string }{
		{"rtmp://a.rtmp.youtube.com/live2/KEY", "/live2", "KEY"},
		{"rtmp://host:1935/live/tv1?key=secret", "/live", "tv1?key=secret"},
		{"rtmps://live-api-s.facebook.com:443/rtmp/FB-1?s_bl=1", "/rtmp", "FB-1?s_bl=1"},
		{"rtmp://host/a/b/KEY/", "/a/b", "KEY"},
		{"rtmp://host/app/sub#key", "/app/sub", "key"},
	}
	for _, c := range cases {
		u, err := rtmpURL(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if u.Path != c.path || u.Fragment != c.key || u.RawQuery != "" {
			t.Errorf("%s: path %q key %q query %q", c.in, u.Path, u.Fragment, u.RawQuery)
		}
	}
	for _, bad := range []string{"rtmp://host", "rtmp://host/app", "rtmp:///app/key"} {
		if _, err := rtmpURL(bad); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	d := minBackoff
	var seq []time.Duration
	for range 7 {
		seq = append(seq, d)
		d = nextBackoff(d)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i := range want {
		if seq[i] != want[i]*time.Second {
			t.Fatalf("backoff sequence %v", seq)
		}
	}
}

func TestIcecastTrackChoice(t *testing.T) {
	aac := core.NewTrack(&core.AAC{Config: &mpeg4audio.AudioSpecificConfig{Type: 2, SampleRate: 48000, ChannelCount: 2}})
	mp3 := core.NewTrack(&core.MPEGAudio{})
	video := core.NewTrack(&core.H264{})

	hub := core.NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := newStream(t, hub, "live/a", "rtmp", video, aac, mp3)
	if tr, ct := icecastTrack(s); tr != mp3 || ct != "audio/mpeg" {
		t.Fatal("MP3 should be preferred")
	}
	s2 := newStream(t, hub, "live/b", "rtmp", video, aac)
	if tr, ct := icecastTrack(s2); tr != aac || ct != "audio/aac" {
		t.Fatal("AAC expected")
	}
	s3 := newStream(t, hub, "live/c", "rtmp", video)
	if _, err := dialIcecast(context.Background(), "icecast://h:8000/m", s3); !errors.As(err, new(*permanentError)) ||
		!strings.Contains(err.Error(), "audio_codec mp3 or aac") {
		t.Fatalf("expected permanent error, got %v", err)
	}
}

func TestADTSHeader(t *testing.T) {
	cfg := &mpeg4audio.AudioSpecificConfig{Type: 2, SampleRate: 44100, ChannelCount: 2}
	h, err := adtsTemplate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	au := make([]byte, 300)
	setADTSLength(h, len(au))
	var pkts mpeg4audio.ADTSPackets
	if err := pkts.Unmarshal(append(append([]byte(nil), h...), au...)); err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 1 || len(pkts[0].AU) != 300 || pkts[0].SampleRate != 44100 || pkts[0].ChannelConfig != 2 {
		t.Fatalf("bad ADTS packet %+v", pkts[0])
	}
}

func TestIcecastHeader(t *testing.T) {
	tg, err := parseIcecastURL("icecast://dj:pa55@radio.example/live.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if tg.host != "radio.example:8000" || tg.user != "dj" || tg.pass != "pa55" || tg.path != "/live.mp3" {
		t.Fatalf("%+v", tg)
	}
	h := tg.header("audio/mpeg", "live/fm")
	for _, want := range []string{"PUT /live.mp3 HTTP/1.1\r\n", "Authorization: Basic ZGo6cGE1NQ==\r\n",
		"Content-Type: audio/mpeg\r\n", "Ice-Name: live/fm\r\n", "Ice-Public: 0\r\n"} {
		if !strings.Contains(h, want) {
			t.Errorf("header lacks %q", want)
		}
	}
	tg, _ = parseIcecastURL("icecasts://radio.example/x")
	if tg.host != "radio.example:443" || tg.user != "source" || !tg.tls {
		t.Fatalf("%+v", tg)
	}
	if _, err := parseIcecastURL("icecast://radio.example:8000"); err == nil {
		t.Fatal("mount point should be required")
	}
}

// fakeSession sends until its subscription ends or fail is closed.
type fakeSession struct {
	fail   chan struct{}
	once   sync.Once
	closed atomic.Bool
}

func (f *fakeSession) send(sub *core.Subscription) error {
	for {
		select {
		case _, ok := <-sub.C():
			if !ok {
				return io.EOF
			}
		case <-f.fail:
			return errors.New("peer went away")
		}
	}
}

func (f *fakeSession) close() {
	f.closed.Store(true)
	f.once.Do(func() { close(f.fail) })
}
func (f *fakeSession) bytesSent() uint64 { return 42 }

func TestReconnectAndStop(t *testing.T) {
	oldMin, oldMax := minBackoff, maxBackoff
	minBackoff, maxBackoff = 10*time.Millisecond, 40*time.Millisecond
	defer func() { minBackoff, maxBackoff = oldMin, oldMax }()

	hub := core.NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	vt := core.NewTrack(&core.H264{})
	s := newStream(t, hub, "live/tv1", "rtmp", vt)

	var attempts atomic.Int32
	sessions := make(chan *fakeSession, 10)
	tg := &target{
		stream: s,
		rawURL: "rtmp://h/a/b",
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		status: Status{Stream: s.Path, State: StateConnecting},
		connector: func(ctx context.Context, _ string, _ *core.Stream) (session, error) {
			if attempts.Add(1) == 2 {
				return nil, errors.New("connection refused")
			}
			fs := &fakeSession{fail: make(chan struct{})}
			sessions <- fs
			return fs, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { tg.run(ctx); close(done) }()

	first := <-sessions
	waitState(t, tg, StateLive)
	first.close() // connection drops
	second := <-sessions
	waitState(t, tg, StateLive)
	st := tg.snapshot()
	if st.Reconnects != 2 || st.LastError != "connection refused" || st.BytesSent != 84 {
		t.Fatalf("unexpected status %+v", st)
	}

	// Stream end stops the target without a retry.
	hub.Unpublish(s)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("target did not stop at stream end")
	}
	if !second.closed.Load() {
		t.Fatal("session not closed")
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("expected 3 attempts, got %d", n)
	}
}

func waitState(t *testing.T, tg *target, state string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tg.snapshot().State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state %q not reached, have %+v", state, tg.snapshot())
}

func TestManagerLifecycle(t *testing.T) {
	cfg := config.Default()
	cfg.Apps = []config.App{{Name: "live", Push: []config.Push{
		{URL: "ftp://example.com/{stream}"},                              // unsupported: fails permanently
		{Name: "radio", URL: "icecast://source:pw@127.0.0.1:1/{stream}"}, // no audio: fails permanently
	}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(config.Static(cfg), log)
	hub := core.NewHub(log)
	hub.AddHook(m)

	s := newStream(t, hub, "live/tv1", "rtmp")
	deadline := time.Now().Add(2 * time.Second)
	var st []Status
	for time.Now().Before(deadline) {
		st = m.Status()
		if len(st) == 2 && st[0].State == StateFailed && st[1].State == StateFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(st) != 2 || st[0].Name != "ftp://example.com" || st[1].Name != "radio" {
		t.Fatalf("unexpected status %+v", st)
	}
	if st[0].State != StateFailed || !strings.Contains(st[0].LastError, "unsupported scheme") {
		t.Fatalf("ftp target: %+v", st[0])
	}
	if st[1].State != StateFailed || strings.Contains(st[1].URL, "pw") {
		t.Fatalf("icecast target: %+v", st[1])
	}

	hub.Unpublish(s)
	if n := len(m.Status()); n != 0 {
		t.Fatalf("status not cleared: %d", n)
	}
	m.Close()
	// After Close, new streams are ignored.
	newStream(t, hub, "live/tv2", "rtmp")
	if n := len(m.Status()); n != 0 {
		t.Fatal("closed manager started targets")
	}
}
