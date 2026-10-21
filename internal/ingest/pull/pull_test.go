package pull

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
)

// mp3Frame builds a valid MPEG-1 Layer III frame: 128 kbit/s, 44.1 kHz,
// no padding (417 bytes). fill marks the payload.
func mp3Frame(fill byte) []byte {
	f := make([]byte, 417)
	f[0], f[1], f[2], f[3] = 0xFF, 0xFB, 0x90, 0x44
	for i := 4; i < len(f); i++ {
		f[i] = fill
	}
	return f
}

// id3Tag builds an ID3v2.4 tag with a payload of n bytes.
func id3Tag(n int) []byte {
	t := []byte{'I', 'D', '3', 4, 0, 0,
		byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	// Put a fake sync pattern inside the tag: it must be skipped, not parsed.
	payload := bytes.Repeat([]byte{0xFF, 0xFB, 0x90, 0x44}, n/4+1)[:n]
	return append(t, payload...)
}

func TestMP3ParserID3AndResync(t *testing.T) {
	var in []byte
	in = append(in, id3Tag(1000)...)
	in = append(in, mp3Frame(1)...)
	in = append(in, mp3Frame(2)...)
	// Garbage with false sync words.
	in = append(in, 0x00, 0xFF, 0xFB, 0x12, 0xFF, 0xE0, 0x00, 0x01, 0x02)
	in = append(in, mp3Frame(3)...)
	in = append(in, mp3Frame(4)[:100]...) // truncated: cut off at EOF
	in2 := append([]byte{}, in...)

	p := newMP3Parser(bytes.NewReader(in2))
	var got []byte
	for {
		f, h, err := p.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}
			break
		}
		if len(f) != 417 || h.SampleRate != 44100 || h.SampleCount() != 1152 {
			t.Fatalf("bad frame: len=%d rate=%d", len(f), h.SampleRate)
		}
		got = append(got, f[4])
	}
	if !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("frames %v, want [1 2 3]", got)
	}
}

func TestMP3ParserMPEG2(t *testing.T) {
	// MPEG-2 Layer III, 64 kbit/s, 24 kHz: 72*64000/24000 = 192 bytes.
	f := make([]byte, 192)
	f[0], f[1], f[2], f[3] = 0xFF, 0xF3, 0x84, 0xC4
	in := append(append([]byte{}, f...), f...)
	p := newMP3Parser(bytes.NewReader(in))
	for range 2 {
		got, h, err := p.Next()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 192 || h.SampleCount() != 576 || h.SampleRate != 24000 {
			t.Fatalf("len=%d samples=%d rate=%d", len(got), h.SampleCount(), h.SampleRate)
		}
	}
}

func TestMP3ParserGivesUpOnNoise(t *testing.T) {
	p := newMP3Parser(bytes.NewReader(make([]byte, maxResync+10000)))
	if _, _, err := p.Next(); !errors.Is(err, errNoSync) {
		t.Fatalf("expected errNoSync, got %v", err)
	}
}

func adtsFrame1(auLen int, fill byte, crc bool) []byte {
	pkts := mpeg4audio.ADTSPackets{{
		Type: mpeg4audio.ObjectTypeAACLC, SampleRate: 48000, ChannelConfig: 2,
		AU: bytes.Repeat([]byte{fill}, auLen),
	}}
	b, err := pkts.Marshal()
	if err != nil {
		panic(err)
	}
	if !crc {
		return b
	}
	// Turn into a protected frame: clear protection_absent, insert CRC.
	total := len(b) + 2
	b[1] &^= 0x01
	b[3] = b[3]&^0x03 | byte(total>>11&0x03)
	b[4] = byte(total >> 3)
	b[5] = b[5]&0x1F | byte(total&0x07)<<5
	out := append([]byte{}, b[:7]...)
	out = append(out, 0xAB, 0xCD)
	return append(out, b[7:]...)
}

func TestADTSParser(t *testing.T) {
	var in []byte
	in = append(in, id3Tag(50)...)
	in = append(in, adtsFrame1(100, 1, false)...)
	in = append(in, 0xFF, 0xF1, 0x00, 0x13, 0x37) // garbage with a fake sync
	in = append(in, adtsFrame1(200, 2, false)...)
	in = append(in, adtsFrame1(150, 3, true)...)
	in = append(in, adtsFrame1(120, 4, false)...)

	p := newADTSParser(bytes.NewReader(in))
	wantLen := []int{100, 200, 150, 120}
	for i, wl := range wantLen {
		f, err := p.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(f.AU) != wl || f.AU[0] != byte(i+1) || f.AU[len(f.AU)-1] != byte(i+1) {
			t.Fatalf("frame %d: len=%d first=%d", i, len(f.AU), f.AU[0])
		}
		if f.Config.SampleRate != 48000 || f.Config.ChannelConfig != 2 || f.Config.Type != mpeg4audio.ObjectTypeAACLC {
			t.Fatalf("frame %d: config %+v", i, f.Config)
		}
		if _, err := f.Config.Marshal(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestSampleClock(t *testing.T) {
	var c sampleClock
	if c.Advance(1152, 44100) != 0 {
		t.Fatal("first pts")
	}
	if got := c.Advance(1152, 44100); got != 1152*time.Second/44100 {
		t.Fatalf("pts %v", got)
	}
	base := 2 * 1152 * time.Second / 44100
	if got := c.Advance(1024, 48000); got != base {
		t.Fatalf("pts after rate change %v, want %v", got, base)
	}
	if got := c.Advance(1024, 48000); got != base+1024*time.Second/48000 {
		t.Fatalf("pts %v", got)
	}
}

func TestBackoff(t *testing.T) {
	var b backoff
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("step %d: %v, want %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if b.Next() != time.Second {
		t.Fatal("reset failed")
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"rtmp://user:secret@host/app/stream?key=abc":         "rtmp://host/app/stream?key=REDACTED",
		"srt://h:9000?streamid=live/x?key=k&passphrase=pppp": "srt://h:9000?streamid=REDACTED&passphrase=REDACTED",
		"http://radio.example/live.mp3":                      "http://radio.example/live.mp3",
		"udp://@:5000":                                       "udp://:5000",
		"https://h/x.m3u8?token=t#frag":                      "https://h/x.m3u8?token=REDACTED#REDACTED",
		"http://h/a?flag":                                    "http://h/a?flag",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeError(t *testing.T) {
	raw := "rtmp://bob:hunter22@host/app/s?key=s3cr3t"
	err := errors.New(`dial "rtmp://bob:hunter22@host/app/s?key=s3cr3t": key s3cr3t rejected for hunter22`)
	msg := sanitizeError(err, raw)
	for _, s := range []string{"hunter22", "s3cr3t", "bob"} {
		if strings.Contains(msg, s) {
			t.Fatalf("%q leaks %q", msg, s)
		}
	}
}

func TestDetectFormat(t *testing.T) {
	cases := []struct{ ct, path, want string }{
		{"video/MP2T", "/x", formatTS},
		{"application/octet-stream", "/s.ts", formatTS},
		{"", "/s.ts", formatTS},
		{"audio/mpeg", "/stream", formatMP3},
		{"", "/live.mp3", formatMP3},
		{"audio/aacp", "/x", formatAAC},
		{"audio/x-aac", "/x", formatAAC},
		{"", "/x.aac", formatAAC},
		{"application/vnd.apple.mpegurl", "/x", formatHLS},
		{"application/x-mpegURL; charset=utf-8", "/x", formatHLS},
		{"text/plain", "/index.m3u8", formatHLS},
		{"text/html", "/", formatUnknown},
	}
	for _, c := range cases {
		if got := detectFormat(c.ct, c.path); got != c.want {
			t.Errorf("detectFormat(%q, %q) = %q, want %q", c.ct, c.path, got, c.want)
		}
	}
}

func TestRTMPClientURL(t *testing.T) {
	u, _ := url.Parse("rtmp://h:1935/live/cam?key=abc")
	got := rtmpClientURL(u)
	if got.Path != "/live" || got.Fragment != "cam?key=abc" || got.RawQuery != "" {
		t.Fatalf("got %+v", got)
	}
	u, _ = url.Parse("rtmp://h/app")
	if got := rtmpClientURL(u); got.Path != "/app" || got.Fragment != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestUDPListenAddr(t *testing.T) {
	for in, want := range map[string]string{
		"udp://@:5000":           ":5000",
		"udp://0.0.0.0:5000":     "0.0.0.0:5000",
		"udp://127.0.0.1:5000":   "127.0.0.1:5000",
		"udp://@239.1.1.1:5000":  "239.1.1.1:5000",
		"udp://239.1.1.1:5000/x": "239.1.1.1:5000",
	} {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		a, err := udpListenAddr(u)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if a.String() != want {
			t.Errorf("%s: %s, want %s", in, a, want)
		}
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestManagerMP3OverHTTP runs the manager against an in-process Icecast-like
// server, checks units flow, the status, and that Close unpublishes promptly
// while the HTTP body is blocked.
func TestManagerMP3OverHTTP(t *testing.T) {
	var gotIcy []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIcy = append(gotIcy, r.Header.Get("Icy-MetaData"))
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(id3Tag(64)) //nolint:errcheck
		for i := range 40 {
			w.Write(mp3Frame(byte(i))) //nolint:errcheck
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done() // then stay silent until the client goes away
	}))
	defer srv.Close()

	hub := core.NewHub(quietLog())
	m := &Manager{
		Sources: []config.Source{{Path: "radio/one", URL: srv.URL + "/live?token=xyz"}},
		Hub:     hub,
		Log:     quietLog(),
	}
	m.Start()

	s := waitStream(t, hub, "radio/one")
	if _, ok := s.Tracks[0].Codec.(*core.MPEGAudio); !ok || len(s.Tracks) != 1 {
		t.Fatalf("unexpected tracks %v", s.Tracks)
	}
	if s.Source.Protocol != "pull" || strings.Contains(s.Source.RemoteAddr, "xyz") {
		t.Fatalf("source info %+v", s.Source)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.UnitsIn() < 40 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.UnitsIn() != 40 {
		t.Fatalf("got %d units", s.UnitsIn())
	}
	st := m.Status()
	if len(st) != 1 || st[0].State != StateLive || strings.Contains(st[0].URL, "xyz") {
		t.Fatalf("status %+v", st)
	}

	start := time.Now()
	m.Close()
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close took too long")
	}
	if hub.Lookup("radio/one") != nil {
		t.Fatal("stream still published after Close")
	}
	if len(gotIcy) == 0 || gotIcy[0] != "" {
		t.Fatalf("Icy-MetaData header sent: %v", gotIcy)
	}
}

func TestManagerRetriesBusyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		for i := 0; r.Context().Err() == nil; i++ {
			if _, err := w.Write(mp3Frame(byte(i))); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	hub := core.NewHub(quietLog())
	other, err := hub.Publish("radio/busy", []*core.Track{core.NewTrack(&core.MPEGAudio{})}, core.SourceInfo{Protocol: "rtmp"})
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Sources: []config.Source{{Path: "radio/busy", URL: srv.URL + "/a.mp3"}}, Hub: hub, Log: quietLog()}
	m.Start()
	defer m.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status()[0]
		if st.State == StateRetrying && strings.Contains(st.LastError, "already") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := m.Status()[0]; st.State != StateRetrying {
		t.Fatalf("status %+v", st)
	}
	hub.Unpublish(other)
	s := waitStream(t, hub, "radio/busy")
	if s.Source.Protocol != "pull" {
		t.Fatal("pull did not take over the path")
	}
	if m.Status()[0].Reconnects < 1 {
		t.Fatal("reconnects not counted")
	}
}

func TestCloseInterruptsUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()

	hub := core.NewHub(quietLog())
	m := &Manager{Sources: []config.Source{{Path: "live/u", URL: "udp://" + addr}}, Hub: hub, Log: quietLog()}
	m.Start()
	time.Sleep(200 * time.Millisecond)
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a UDP read")
	}
}

func TestCloseWithoutStart(_ *testing.T) {
	(&Manager{}).Close()
}

func waitStream(t *testing.T, hub *core.Hub, path string) *core.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if s := hub.Lookup(path); s != nil {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stream %s not published", path)
	return nil
}
