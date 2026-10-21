package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/ingest/srt"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/output/httpstream"
)

// outEnv is a server set with the non-HLS playback outputs.
type outEnv struct {
	*env
	streams *httpstream.Handler
}

// outStart starts RTMP, SRT and HTTP servers with every playback output
// enabled (or all disabled). App "live" needs publish key "k3y" and no play
// key; app "priv" is open for publishing and needs play key "pk".
func outStart(t *testing.T, playback bool) *outEnv {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	cfg := config.Default()
	cfg.HLS.Variants = []string{config.VariantMPEGTS}
	cfg.HLS.Primary = config.VariantMPEGTS
	cfg.HLS.SegmentDuration = config.Duration(time.Second)
	cfg.Playback = config.Playback{HTTPTS: playback, HTTPAudio: playback, RTMP: playback, SRT: playback}
	cfg.Apps = []config.App{
		{Name: "live", PublishKey: "k3y"},
		{Name: "priv", PlayKey: "pk"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := core.NewHub(log)
	authz := auth.New(cfg)
	hm := hls.NewManager(cfg.HLS, "*", log)
	hub.AddHook(hm)

	rs := &rtmp.Server{Addr: "127.0.0.1:0", Hub: hub, Auth: authz, Log: log, AllowPlay: cfg.Playback.RTMP}
	if err := rs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)

	ss := &srt.Server{Addr: "127.0.0.1:" + freeUDPPort(t), Hub: hub, Auth: authz, Log: log, AllowRead: cfg.Playback.SRT}
	if err := ss.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ss.Close)

	hs := &httpstream.Handler{Hub: hub, Auth: authz, Playback: cfg.Playback, AllowOrigin: "*", Log: log}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hs.Match(r.URL.Path) {
			hs.ServeHTTP(w, r)
			return
		}
		hm.ServeHTTP(w, r)
	}))
	// Close active stream clients first, or Close waits for them.
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})

	return &outEnv{
		env:     &env{hub: hub, rtmpAddr: rs.ListenAddr().String(), srtAddr: ss.ListenAddr(), http: srv},
		streams: hs,
	}
}

func outRequire(t *testing.T, what, name string) {
	t.Helper()
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-"+what).Output()
	if !strings.Contains(string(out), " "+name) {
		t.Skipf("ffmpeg has no %s %s", strings.TrimSuffix(what, "s"), name)
	}
}

// outDecode decodes two seconds of url and returns which media types
// FFmpeg found. It retries while the stream warms up.
func outDecode(t *testing.T, url string, extra ...string) (video, audio bool) {
	t.Helper()
	var lastErr string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		args := append([]string{"-hide_banner", "-loglevel", "info", "-nostats"}, extra...)
		args = append(args, "-i", url, "-t", "2", "-f", "null", "-")
		out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
		cancel()
		s := string(out)
		if err == nil && strings.Contains(s, "Output #0") {
			return strings.Contains(s, "Video:"), strings.Contains(s, "Audio:")
		}
		lastErr = fmt.Sprintf("%v: %s", err, s)
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("cannot decode %s: %s", url, lastErr)
	return false, false
}

// outRejected checks that FFmpeg cannot read url.
func outRejected(t *testing.T, url string, extra ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := append([]string{"-hide_banner", "-loglevel", "error"}, extra...)
	args = append(args, "-i", url, "-t", "1", "-f", "null", "-")
	out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	if err == nil {
		t.Fatalf("expected %s to be rejected, FFmpeg succeeded: %s", url, out)
	}
	if ctx.Err() != nil {
		t.Fatalf("expected %s to be rejected, FFmpeg hung instead", url)
	}
}

// outHead returns the status and headers of a GET, without reading the body.
func outHead(t *testing.T, url string) (int, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

func outPublishRTMP(t *testing.T, e *outEnv, path string, enc []string) {
	t.Helper()
	publish(t, join(enc, []string{"-f", "flv", "rtmp://" + e.rtmpAddr + "/" + path})...)
}

func outPublishSRT(t *testing.T, e *outEnv, path string, enc []string) {
	t.Helper()
	publish(t, join(enc, []string{"-f", "mpegts", "srt://" + e.srtAddr + "?streamid=" + path + "&pkt_size=1316"})...)
}

func outSRTURL(e *outEnv, sid string) string {
	return "srt://" + e.srtAddr + "?streamid=" + sid + "&mode=caller"
}

func TestOutHTTPTS(t *testing.T) {
	e := outStart(t, true)
	outPublishRTMP(t, e, "live/tv?key=k3y", av264AAC)
	s := waitForStream(t, e.env, "live/tv")

	code, hd := outHead(t, e.http.URL+"/live/tv.ts")
	if code != http.StatusOK || hd.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("got %d %v", code, hd)
	}
	if v, a := outDecode(t, e.http.URL+"/live/tv.ts"); !v || !a {
		t.Fatalf("expected video and audio, got video=%v audio=%v", v, a)
	}
	if s.BytesOut.Load() == 0 {
		t.Fatal("BytesOut not counted")
	}
	// HLS still works next to the continuous stream.
	play(t, e.http.URL+"/live/tv/index.m3u8")
}

func TestOutHTTPMP3(t *testing.T) {
	outRequire(t, "encoders", "libmp3lame")
	e := outStart(t, true)
	outPublishSRT(t, e, "live/radio?key=k3y", join(inAudio, []string{"-c:a", "libmp3lame", "-b:a", "128k"}))
	waitForStream(t, e.env, "live/radio")

	code, hd := outHead(t, e.http.URL+"/live/radio.mp3")
	if code != http.StatusOK || hd.Get("Content-Type") != "audio/mpeg" || hd.Get("icy-name") != "live/radio" ||
		hd.Get("icy-pub") != "0" || hd.Get("Cache-Control") != "no-cache, no-store" || hd.Get("Content-Length") != "" {
		t.Fatalf("got %d %v", code, hd)
	}
	if _, a := outDecode(t, e.http.URL+"/live/radio.mp3"); !a {
		t.Fatal("no audio decoded")
	}
	// The same stream has no AAC.
	if code, _ := outHead(t, e.http.URL+"/live/radio.aac"); code != http.StatusNotFound {
		t.Fatalf(".aac of an MP3 stream: got %d", code)
	}
	// MPEG-TS carries MP3 too.
	if _, a := outDecode(t, e.http.URL+"/live/radio.ts"); !a {
		t.Fatal("no audio decoded from .ts")
	}
}

func TestOutHTTPAAC(t *testing.T) {
	e := outStart(t, true)
	outPublishRTMP(t, e, "live/aac?key=k3y", audioAAC)
	waitForStream(t, e.env, "live/aac")

	code, hd := outHead(t, e.http.URL+"/live/aac.aac")
	if code != http.StatusOK || hd.Get("Content-Type") != "audio/aac" || hd.Get("icy-name") != "live/aac" {
		t.Fatalf("got %d %v", code, hd)
	}
	if _, a := outDecode(t, e.http.URL+"/live/aac.aac"); !a {
		t.Fatal("no audio decoded")
	}
	code, _ = outHead(t, e.http.URL+"/live/aac.mp3")
	if code != http.StatusNotFound {
		t.Fatalf(".mp3 of an AAC stream: got %d", code)
	}
}

func TestOutRTMPPlay(t *testing.T) {
	e := outStart(t, true)
	outPublishRTMP(t, e, "live/fromrtmp?key=k3y", av264AAC)
	outPublishSRT(t, e, "live/fromsrt?key=k3y", av264AAC)
	waitForStream(t, e.env, "live/fromrtmp")
	s := waitForStream(t, e.env, "live/fromsrt")

	if v, a := outDecode(t, "rtmp://"+e.rtmpAddr+"/live/fromrtmp"); !v || !a {
		t.Fatalf("RTMP->RTMP: video=%v audio=%v", v, a)
	}
	// SRT sources carry SPS/PPS only in-band: rtmpconv takes them from the
	// first keyframe.
	if v, a := outDecode(t, "rtmp://"+e.rtmpAddr+"/live/fromsrt"); !v || !a {
		t.Fatalf("SRT->RTMP: video=%v audio=%v", v, a)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.BytesOut.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if s.BytesOut.Load() == 0 {
		t.Fatal("BytesOut not counted for RTMP player")
	}
	outRejected(t, "rtmp://"+e.rtmpAddr+"/live/missing")
}

func TestOutSRTRead(t *testing.T) {
	e := outStart(t, true)
	outPublishRTMP(t, e, "live/x?key=k3y", av264AAC)
	s := waitForStream(t, e.env, "live/x")

	if v, a := outDecode(t, outSRTURL(e, "read:live/x"), "-f", "mpegts"); !v || !a {
		t.Fatalf("SRT read: video=%v audio=%v", v, a)
	}
	if v, _ := outDecode(t, outSRTURL(e, "#!::r=live/x,m=request"), "-f", "mpegts"); !v {
		t.Fatal("SRT read (access control syntax): no video")
	}
	if s.BytesOut.Load() == 0 {
		t.Fatal("BytesOut not counted for SRT reader")
	}
	outRejected(t, outSRTURL(e, "read:live/missing"), "-f", "mpegts")
}

func TestOutPlayKey(t *testing.T) {
	e := outStart(t, true)
	outPublishRTMP(t, e, "priv/tv", av264AAC)
	waitForStream(t, e.env, "priv/tv")
	base := e.http.URL + "/priv/tv"

	for _, ext := range []string{".ts", ".aac"} {
		if code, _ := outHead(t, base+ext); code != http.StatusUnauthorized {
			t.Errorf("%s without key: got %d", ext, code)
		}
		if code, _ := outHead(t, base+ext+"?key=wrong"); code != http.StatusForbidden {
			t.Errorf("%s with wrong key: got %d", ext, code)
		}
	}
	outRejected(t, "rtmp://"+e.rtmpAddr+"/priv/tv")
	outRejected(t, "rtmp://"+e.rtmpAddr+"/priv/tv?key=wrong")
	outRejected(t, outSRTURL(e, "read:priv/tv"), "-f", "mpegts")
	outRejected(t, outSRTURL(e, "read:priv/tv?key=wrong"), "-f", "mpegts")

	outDecode(t, base+".ts?key=pk")
	outDecode(t, base+".aac?key=pk")
	outDecode(t, "rtmp://"+e.rtmpAddr+"/priv/tv?key=pk")
	outDecode(t, outSRTURL(e, "read:priv/tv?key=pk"), "-f", "mpegts")
	outDecode(t, outSRTURL(e, "#!::r=priv/tv,m=request,key=pk"), "-f", "mpegts")
}

func TestOutPlaybackDisabled(t *testing.T) {
	e := outStart(t, false)
	outPublishRTMP(t, e, "live/tv?key=k3y", av264AAC)
	waitForStream(t, e.env, "live/tv")

	for _, ext := range []string{".ts", ".aac", ".mp3"} {
		if code, _ := outHead(t, e.http.URL+"/live/tv"+ext); code != http.StatusNotFound {
			t.Errorf("%s disabled: got %d", ext, code)
		}
	}
	outRejected(t, "rtmp://"+e.rtmpAddr+"/live/tv")
	outRejected(t, outSRTURL(e, "read:live/tv"), "-f", "mpegts")
	// Publishing and HLS are unaffected.
	play(t, e.http.URL+"/live/tv/index.m3u8")
}
