package httpstream

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
)

func newHandler(t *testing.T, pb config.Playback) (*Handler, *core.Hub) {
	t.Helper()
	cfg := config.Default()
	cfg.Apps = []config.App{{Name: "live"}, {Name: "priv", PlayKey: "pk"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := core.NewHub(log)
	return &Handler{Hub: hub, Auth: auth.New(cfg), Playback: pb, AllowOrigin: "*", Log: log}, hub
}

var allOn = config.Playback{HTTPTS: true, HTTPAudio: true}

func aacTrack() *core.Track {
	return core.NewTrack(&core.AAC{Config: &mpeg4audio.AudioSpecificConfig{
		Type: mpeg4audio.ObjectTypeAACLC, SampleRate: 48000, ChannelConfig: 2, ChannelCount: 2,
	}})
}

func TestMatch(t *testing.T) {
	h := &Handler{}
	for p, want := range map[string]bool{
		"/live/tv1.ts":          true,
		"/live/radio.mp3":       true,
		"/live/radio.aac":       true,
		"/live/a.b.ts":          true,
		"/live/.ts":             false,
		"/live/tv1":             false,
		"/live/tv1/index.m3u8":  false,
		"/live/tv1/seg1.ts":     false,
		"/live/tv1/mpegts/x.ts": false,
		"/tv1.ts":               false,
		"//tv1.ts":              false,
		"/live/tv1.mp4":         false,
		"/":                     false,
	} {
		if got := h.Match(p); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
}

func get(t *testing.T, h http.Handler, method, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, url, nil))
	return rec
}

func TestRejections(t *testing.T) {
	h, hub := newHandler(t, allOn)
	for _, p := range []string{"live/tv", "priv/tv"} {
		if _, err := hub.Publish(p, []*core.Track{core.NewTrack(&core.H264{}), aacTrack()},
			core.SourceInfo{}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		url    string
		status int
		body   string
	}{
		{"/priv/tv.ts", http.StatusUnauthorized, "play key required"},
		{"/priv/tv.ts?key=bad", http.StatusForbidden, "invalid play key"},
		{"/priv/tv.aac?key=bad", http.StatusForbidden, ""},
		{"/nope/tv.ts", http.StatusNotFound, ""},
		{"/live/missing.ts", http.StatusNotFound, "stream not found"},
		{"/live/tv.mp3", http.StatusNotFound, "audio_codec: mp3"},
		{"/priv/tv.mp3?key=pk", http.StatusNotFound, "audio_codec: mp3"},
	}
	for _, c := range cases {
		rec := get(t, h, http.MethodHead, c.url)
		if c.body != "" {
			rec = get(t, h, http.MethodGet, c.url)
		}
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: got %d %q, want %d containing %q", c.url, rec.Code, rec.Body.String(), c.status, c.body)
		}
	}

	if rec := get(t, h, http.MethodPost, "/live/tv.ts"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d", rec.Code)
	}
	if rec := get(t, h, http.MethodOptions, "/live/tv.ts"); rec.Code != http.StatusNoContent ||
		rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("OPTIONS: got %d %v", rec.Code, rec.Header())
	}
}

func TestNoAACMessage(t *testing.T) {
	h, hub := newHandler(t, allOn)
	if _, err := hub.Publish("live/mp3", []*core.Track{core.NewTrack(&core.MPEGAudio{})}, core.SourceInfo{}); err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, http.MethodGet, "/live/mp3.aac")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no AAC audio") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestDisabled(t *testing.T) {
	h, hub := newHandler(t, config.Playback{HTTPTS: false, HTTPAudio: true})
	if _, err := hub.Publish("live/tv", []*core.Track{aacTrack()}, core.SourceInfo{}); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, h, http.MethodHead, "/live/tv.ts"); rec.Code != http.StatusNotFound {
		t.Errorf(".ts with http_ts off: got %d", rec.Code)
	}
	if rec := get(t, h, http.MethodHead, "/live/tv.aac"); rec.Code != http.StatusOK {
		t.Errorf(".aac with http_audio on: got %d", rec.Code)
	}

	h.Playback = config.Playback{HTTPTS: true}
	if rec := get(t, h, http.MethodHead, "/live/tv.aac"); rec.Code != http.StatusNotFound {
		t.Errorf(".aac with http_audio off: got %d", rec.Code)
	}
	if rec := get(t, h, http.MethodHead, "/live/tv.ts"); rec.Code != http.StatusOK {
		t.Errorf(".ts with http_ts on: got %d", rec.Code)
	}
}

func TestHeadHeaders(t *testing.T) {
	h, hub := newHandler(t, allOn)
	if _, err := hub.Publish("priv/radio", []*core.Track{core.NewTrack(&core.MPEGAudio{}), aacTrack()},
		core.SourceInfo{}); err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, http.MethodHead, "/priv/radio.mp3?key=pk")
	hd := rec.Header()
	if rec.Code != http.StatusOK || hd.Get("Content-Type") != "audio/mpeg" ||
		hd.Get("icy-name") != "priv/radio" || hd.Get("icy-pub") != "0" ||
		hd.Get("Cache-Control") != "no-cache, no-store" || hd.Get("Content-Length") != "" ||
		hd.Get("Access-Control-Allow-Origin") != "*" || rec.Body.Len() != 0 {
		t.Fatalf("got %d %v body=%d", rec.Code, hd, rec.Body.Len())
	}
	if rec := get(t, h, http.MethodHead, "/priv/radio.aac?key=pk"); rec.Header().Get("Content-Type") != "audio/aac" {
		t.Fatalf("aac content type: %v", rec.Header())
	}
	if rec := get(t, h, http.MethodHead, "/priv/radio.ts?key=pk"); rec.Header().Get("Content-Type") != "video/mp2t" ||
		rec.Header().Get("icy-name") != "" {
		t.Fatalf("ts headers: %v", rec.Header())
	}
	if n := hub.Lookup("priv/radio").SubscriberCount(); n != 0 {
		t.Fatalf("HEAD left %d subscribers", n)
	}
}

func TestWrapADTS(t *testing.T) {
	cfg := aacTrack().Codec.(*core.AAC).Config
	au := []byte{1, 2, 3, 4, 5}
	b, err := wrapADTS(adtsTemplate(cfg), au)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 7+len(au) || b[0] != 0xFF || b[1] != 0xF1 {
		t.Fatalf("bad header % x", b)
	}
	var pkts mpeg4audio.ADTSPackets
	if err := pkts.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	p := pkts[0]
	if len(pkts) != 1 || p.Type != mpeg4audio.ObjectTypeAACLC || p.SampleRate != 48000 ||
		p.ChannelConfig != 2 || !bytes.Equal(p.AU, au) {
		t.Fatalf("round trip: %+v", p)
	}

	if _, err := wrapADTS(adtsTemplate(cfg), make([]byte, mpeg4audio.MaxAccessUnitSize+1)); err == nil {
		t.Fatal("expected error for oversized AU")
	}
}

// TestStreamAAC checks a full GET: ADTS frames arrive, bytes and listeners
// are counted, and the response ends with the stream.
func TestStreamAAC(t *testing.T) {
	h, hub := newHandler(t, allOn)
	tr := aacTrack()
	s, err := hub.Publish("live/radio", []*core.Track{tr}, core.SourceInfo{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/live/radio.aac", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/aac" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}

	// The subscription starts when headers are sent; feed until data arrives.
	stop := make(chan struct{})
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			s.WriteUnit(&core.Unit{Track: tr, PTS: time.Duration(i) * 20 * time.Millisecond,
				Payload: [][]byte{{0xAA, 0xBB}, {0xCC}}})
		}
	}()

	buf := make([]byte, 9+8)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-fed
	var pkts mpeg4audio.ADTSPackets
	if err := pkts.Unmarshal(buf); err != nil || len(pkts) != 2 ||
		!bytes.Equal(pkts[0].AU, []byte{0xAA, 0xBB}) || !bytes.Equal(pkts[1].AU, []byte{0xCC}) {
		t.Fatalf("unexpected ADTS stream % x (%v)", buf, err)
	}
	if h.Listeners("live/radio") != 1 {
		t.Fatalf("listeners = %d", h.Listeners("live/radio"))
	}
	if s.BytesOut.Load() < uint64(len(buf)) {
		t.Fatalf("BytesOut = %d", s.BytesOut.Load())
	}

	hub.Unpublish(s)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.Listeners("live/radio") != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := h.Listeners("live/radio"); n != 0 {
		t.Fatalf("listeners after end = %d", n)
	}
}
