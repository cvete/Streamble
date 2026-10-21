// Package e2e runs the real ingest servers and HLS output against FFmpeg.
// Tests are skipped when ffmpeg is not installed.
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/api"
	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/ingest/srt"
	"github.com/cvete/streamble/internal/output/hls"
)

type env struct {
	hub      *core.Hub
	rtmpAddr string
	srtAddr  string
	http     *httptest.Server
}

func start(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	cfg := config.Default()
	cfg.HLS.Variants = []string{config.VariantLLHLS, config.VariantFMP4, config.VariantMPEGTS}
	cfg.HLS.SegmentDuration = config.Duration(time.Second)
	cfg.Apps = []config.App{
		{Name: "live", PublishKey: "k3y"},
		{Name: "secure", SRTPassphrase: "passphrase123"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := core.NewHub(log)
	authz := auth.New(cfg)
	hm := hls.NewManager(cfg.HLS, "*", log)
	hub.AddHook(hm)

	rs := &rtmp.Server{Addr: "127.0.0.1:0", Hub: hub, Auth: authz, Log: log}
	if err := rs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)

	ss := &srt.Server{Addr: "127.0.0.1:" + freeUDPPort(t), Hub: hub, Auth: authz, Log: log}
	if err := ss.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ss.Close)

	a := api.New(hub, hm, "", cfg.HLS.Primary)
	t.Cleanup(a.Close)
	mux := http.NewServeMux()
	mux.Handle("/api/", a)
	mux.Handle("/", hm)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)

	return &env{hub: hub, rtmpAddr: rs.ListenAddr().String(), srtAddr: ss.ListenAddr(), http: hs}
}

func freeUDPPort(t *testing.T) string {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, port, _ := net.SplitHostPort(c.LocalAddr().String())
	return port
}

// publish runs an FFmpeg test source in the background for the test's lifetime.
func publish(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	base := []string{"-hide_banner", "-loglevel", "error", "-re"}
	cmd := exec.CommandContext(ctx, "ffmpeg", append(base, args...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		cmd.Wait() //nolint:errcheck
	})
}

// Test sources. FFmpeg options apply to the next -i or output, so every
// input comes first, then the encoder options, then the output URL.
var (
	inVideo  = []string{"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25"}
	inAudio  = []string{"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000"}
	encH264  = []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-pix_fmt", "yuv420p"}
	encAAC   = []string{"-c:a", "aac"}
	video264 = join(inVideo, encH264)
	av264AAC = join(inVideo, inAudio, encH264, encAAC)
	audioAAC = join(inAudio, encAAC)
)

func join(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func waitForStream(t *testing.T, e *env, path string) *core.Stream {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if s := e.hub.Lookup(path); s != nil {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("stream %s was not published", path)
	return nil
}

// play decodes two seconds of an HLS URL with FFmpeg and fails on errors.
func play(t *testing.T, url string) {
	t.Helper()
	var lastErr string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
			"-i", url, "-t", "2", "-f", "null", "-").CombinedOutput()
		cancel()
		if err == nil && len(strings.TrimSpace(string(out))) == 0 {
			return
		}
		lastErr = fmt.Sprintf("%v: %s", err, out)
		time.Sleep(500 * time.Millisecond) // muxer may still be producing the first segments
	}
	t.Fatalf("cannot play %s: %s", url, lastErr)
}

func TestRTMPToAllHLSVariants(t *testing.T) {
	e := start(t)
	publish(t, join(av264AAC, []string{"-f", "flv", "rtmp://" + e.rtmpAddr + "/live/cam?key=k3y"})...)
	s := waitForStream(t, e, "live/cam")
	if len(s.Tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(s.Tracks))
	}
	for _, v := range []string{"", "llhls/", "fmp4/", "mpegts/"} {
		play(t, e.http.URL+"/live/cam/"+v+"index.m3u8")
	}
}

func TestSRTPublishWithKey(t *testing.T) {
	e := start(t)
	publish(t, join(av264AAC, []string{"-f", "mpegts",
		"srt://" + e.srtAddr + "?streamid=live/srt?key=k3y&pkt_size=1316"})...)
	waitForStream(t, e, "live/srt")
	play(t, e.http.URL+"/live/srt/mpegts/index.m3u8")
}

func TestSRTEncryptedHEVC(t *testing.T) {
	if out, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output(); !strings.Contains(string(out), "libx265") {
		t.Skip("ffmpeg has no libx265")
	}
	e := start(t)
	publish(t, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "keyint=25:log-level=error",
		"-pix_fmt", "yuv420p", "-f", "mpegts",
		"srt://"+e.srtAddr+"?streamid=#!::r=secure/hevc,m=publish&passphrase=passphrase123&pkt_size=1316")
	waitForStream(t, e, "secure/hevc")
	play(t, e.http.URL+"/secure/hevc/llhls/index.m3u8")

	// HEVC is fMP4-only in HLS: the MPEG-TS variant must not exist.
	resp, err := http.Get(e.http.URL + "/secure/hevc/mpegts/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for HEVC over MPEG-TS, got %d", resp.StatusCode)
	}
}

func TestAudioOnlyRadio(t *testing.T) {
	e := start(t)
	publish(t, join(audioAAC, []string{"-f", "flv", "rtmp://" + e.rtmpAddr + "/live/radio?key=k3y"})...)
	waitForStream(t, e, "live/radio")
	play(t, e.http.URL+"/live/radio/index.m3u8")
}

func TestRejectedPublishers(t *testing.T) {
	e := start(t)
	publish(t, join(video264, []string{"-f", "flv", "rtmp://" + e.rtmpAddr + "/live/bad?key=wrong"})...)
	publish(t, join(video264, []string{"-f", "flv", "rtmp://" + e.rtmpAddr + "/unknown/x"})...)
	publish(t, join(video264, []string{"-f", "mpegts", "srt://" + e.srtAddr + "?streamid=secure/plain&pkt_size=1316"})...)
	time.Sleep(3 * time.Second)
	if n := len(e.hub.Streams()); n != 0 {
		t.Fatalf("expected no streams, got %d", n)
	}
}
