package e2e

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/transcode"
)

type transEnv struct {
	hub      *core.Hub
	tm       *transcode.Manager
	rtmpAddr string
	http     *httptest.Server
}

func transStart(t *testing.T, ffmpegPath string, p config.Profile) *transEnv {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if out, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output(); !strings.Contains(string(out), "libmp3lame") {
		t.Skip("ffmpeg has no libmp3lame")
	}

	cfg := config.Default()
	cfg.HLS.SegmentDuration = config.Duration(time.Second)
	cfg.Transcode.FFmpeg = ffmpegPath
	cfg.Transcode.Profiles = []config.Profile{p}
	cfg.Apps = []config.App{{Name: "live", Transcode: p.Name}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("STREAMBLE_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	hub := core.NewHub(log)
	hm := hls.NewManager(cfg.HLS, "*", log)
	tm := transcode.NewManager(config.Static(cfg), hub, log)
	hm.ABR = func(path string) []string {
		var out []string
		for _, r := range tm.Renditions(path) {
			if !r.AudioOnly { // same rule as cmd/streamble
				out = append(out, r.Path)
			}
		}
		return out
	}
	hub.AddHook(hm)
	hub.AddHook(tm)
	t.Cleanup(tm.Close)

	rs := &rtmp.Server{Addr: "127.0.0.1:0", Hub: hub, Auth: auth.New(cfg), Log: log}
	if err := rs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)

	hs := httptest.NewServer(hm)
	t.Cleanup(hs.Close)
	return &transEnv{hub: hub, tm: tm, rtmpAddr: rs.ListenAddr().String(), http: hs}
}

func (e *transEnv) wait(t *testing.T, path string) *core.Stream {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s := e.hub.Lookup(path); s != nil {
			return s
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("stream %s did not appear; transcoder status: %+v", path, e.tm.Status())
	return nil
}

func transCodecs(s *core.Stream) string {
	var names []string
	for _, tr := range s.Tracks {
		names = append(names, tr.Codec.Name())
	}
	return strings.Join(names, "+")
}

// An H.264 + MP3 source (common from radio-TV encoders) becomes an
// HLS-playable H.264 + AAC rendition and an MP3 radio rendition.
func TestTranscodeMP3SourceToLadder(t *testing.T) {
	e := transStart(t, "ffmpeg", config.Profile{
		Name: "ladder",
		Renditions: []config.Rendition{
			{Name: "240p", Height: 240, VideoBitrate: "300k", AudioBitrate: "64k"},
			{Name: "radio", AudioOnly: true, AudioCodec: "mp3", AudioBitrate: "64k"},
		},
	})

	publish(t, join(inVideo, inAudio, encH264,
		[]string{"-c:a", "libmp3lame", "-b:a", "128k", "-f", "flv", "rtmp://" + e.rtmpAddr + "/live/cam"})...)

	src := e.wait(t, "live/cam")
	if got := transCodecs(src); got != "H264+MP2/MP3" {
		t.Fatalf("source tracks %s, want H264+MP2/MP3", got)
	}

	video := e.wait(t, "live/cam_240p")
	if got := transCodecs(video); got != "H264+AAC" {
		t.Fatalf("240p tracks %s, want H264+AAC", got)
	}
	radio := e.wait(t, "live/cam_radio")
	if got := transCodecs(radio); got != "MP2/MP3" {
		t.Fatalf("radio tracks %s, want MP3", got)
	}
	if video.Source.Protocol != transcode.ProtocolTranscode {
		t.Fatalf("rendition protocol %q", video.Source.Protocol)
	}

	// The rendition plays over HLS (the MP3 source alone could not).
	play(t, e.http.URL+"/live/cam_240p/index.m3u8")

	// The adaptive master playlist references the video rendition and plays.
	var master string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(e.http.URL + "/live/cam/abr.m3u8")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(b), "../cam_240p/llhls/") {
				master = string(b)
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if master == "" {
		t.Fatal("ABR master playlist never listed the 240p rendition")
	}
	if !strings.Contains(master, "#EXT-X-STREAM-INF") {
		t.Fatalf("not a multivariant playlist:\n%s", master)
	}
	play(t, e.http.URL+"/live/cam/abr.m3u8")
	play(t, e.http.URL+"/live/cam/mpegts/abr.m3u8")

	st := e.tm.Status()
	if len(st) != 1 || st[0].State != "running" || len(st[0].Renditions) != 2 {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestTranscodeStopsWithSource(t *testing.T) {
	e := transStart(t, "ffmpeg", config.Profile{
		Name:       "small",
		Renditions: []config.Rendition{{Name: "low", Height: 144, VideoBitrate: "150k"}},
	})
	ctx := publishFor(t, 15*time.Second, join(inVideo, inAudio, encH264, encAAC,
		[]string{"-f", "flv", "rtmp://" + e.rtmpAddr + "/live/short"})...)
	e.wait(t, "live/short_low")
	<-ctx

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if e.hub.Lookup("live/short_low") == nil && len(e.tm.Status()) == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("rendition or job still running after the source ended")
}

func TestTranscodeReportsMissingFFmpeg(t *testing.T) {
	e := transStart(t, "/nonexistent/ffmpeg", config.Profile{
		Name:       "broken",
		Renditions: []config.Rendition{{Name: "x", Height: 144}},
	})
	publish(t, join(inVideo, encH264, []string{"-f", "flv", "rtmp://" + e.rtmpAddr + "/live/nf"})...)
	e.wait(t, "live/nf")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := e.tm.Status(); len(st) == 1 && st[0].State == "restarting" &&
			strings.Contains(st[0].LastError, "ffmpeg not found") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("expected a restarting job with a clear error, got %+v", e.tm.Status())
}

// publishFor runs FFmpeg for a fixed time; the returned channel closes when
// it has exited.
func publishFor(t *testing.T, d time.Duration, args ...string) <-chan struct{} {
	t.Helper()
	base := []string{"-hide_banner", "-loglevel", "error", "-re"}
	full := append(base, args...)
	// Limit the duration as an output option, right before the output URL.
	url := full[len(full)-1]
	full = append(full[:len(full)-1:len(full)-1], "-t", strconv.FormatFloat(d.Seconds(), 'f', -1, 64), url)
	cmd := exec.Command("ffmpeg", full...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait() //nolint:errcheck
		close(done)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill() //nolint:errcheck
		<-done
	})
	return done
}
