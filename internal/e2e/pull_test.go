package e2e

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/ingest/pull"
)

// pullStart runs a pull manager for one source on the test hub.
func pullStart(t *testing.T, e *env, path, url string) *pull.Manager {
	t.Helper()
	m := &pull.Manager{
		Sources: []config.Source{{Path: path, URL: url}},
		Hub:     e.hub,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	m.Start()
	t.Cleanup(m.Close)
	return m
}

func pullRequire(t *testing.T, list, what string) {
	t.Helper()
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-"+list).Output()
	if !strings.Contains(string(out), what) {
		t.Skipf("ffmpeg has no %s", what)
	}
}

func pullFreeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// pullCheckTracks fails unless the stream has exactly the given codecs.
func pullCheckTracks(t *testing.T, s *core.Stream, codecs ...string) {
	t.Helper()
	var got []string
	for _, tr := range s.Tracks {
		got = append(got, tr.Codec.Name())
	}
	if strings.Join(got, ",") != strings.Join(codecs, ",") {
		t.Fatalf("tracks %v, want %v", got, codecs)
	}
}

// pullCountUnits subscribes and waits for n units of every track.
func pullCountUnits(t *testing.T, s *core.Stream, n int) {
	t.Helper()
	sub := s.Subscribe("pull-test", 4096)
	if sub == nil {
		t.Fatal("stream ended")
	}
	defer sub.Close()
	counts := map[*core.Track]int{}
	timeout := time.After(15 * time.Second)
	for {
		done := true
		for _, tr := range s.Tracks {
			if counts[tr] < n {
				done = false
			}
		}
		if done {
			return
		}
		select {
		case u, ok := <-sub.C():
			if !ok {
				t.Fatalf("stream ended early, counts %v", counts)
			}
			counts[u.Track]++
		case <-timeout:
			t.Fatalf("not enough units: %v", counts)
		}
	}
}

// pullFFmpegHandler serves FFmpeg's stdout (one process per request) like
// an Icecast server. prefix is written before the audio; stopAfter > 0
// ends the first response after that duration (a dropped source).
func pullFFmpegHandler(t *testing.T, contentType string, prefix []byte, stopAfter time.Duration,
	args ...string,
) http.Handler {
	var requests atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if requests.Add(1) == 1 && stopAfter > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, stopAfter)
			defer cancel()
		}
		base := []string{"-hide_banner", "-loglevel", "error", "-re"}
		cmd := exec.CommandContext(ctx, "ffmpeg", append(base, args...)...)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Error(err)
			return
		}
		if err := cmd.Start(); err != nil {
			t.Error(err)
			return
		}
		defer cmd.Wait() //nolint:errcheck
		w.Header().Set("Content-Type", contentType)
		w.Write(prefix) //nolint:errcheck
		buf := make([]byte, 4096)
		for {
			n, err := out.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
			if err != nil {
				return
			}
		}
	})
}

// pullID3 is a small ID3v2.4 tag with a fake frame sync inside.
var pullID3 = append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 20},
	0xFF, 0xFB, 0x90, 0x44, 0xFF, 0xFB, 0x90, 0x44, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)

func TestPullHTTPMPEGTS(t *testing.T) {
	e := start(t)
	port := pullFreeTCPPort(t)
	publish(t, join(av264AAC, []string{"-f", "mpegts", "-listen", "1", "http://127.0.0.1:" + port + "/s.ts"})...)
	pullStart(t, e, "live/httpts", "http://127.0.0.1:"+port+"/s.ts")
	s := waitForStream(t, e, "live/httpts")
	pullCheckTracks(t, s, "H264", "AAC")
	pullCountUnits(t, s, 25)
	play(t, e.http.URL+"/live/httpts/index.m3u8")
}

func TestPullHTTPMP3Radio(t *testing.T) {
	pullRequire(t, "encoders", "libmp3lame")
	e := start(t)
	srv := httptest.NewServer(pullFFmpegHandler(t, "audio/mpeg", pullID3, 0,
		join(inAudio, []string{"-c:a", "libmp3lame", "-b:a", "128k", "-f", "mp3", "-id3v2_version", "0", "pipe:1"})...))
	t.Cleanup(srv.Close)
	pullStart(t, e, "live/mp3", srv.URL+"/stream?listener=secret")
	s := waitForStream(t, e, "live/mp3")
	pullCheckTracks(t, s, "MP3")
	if strings.Contains(s.Source.RemoteAddr, "secret") {
		t.Fatalf("remote address leaks the query: %s", s.Source.RemoteAddr)
	}
	pullCountUnits(t, s, 40)
}

func TestPullHTTPAAC(t *testing.T) {
	e := start(t)
	srv := httptest.NewServer(pullFFmpegHandler(t, "audio/aacp", nil, 0,
		join(inAudio, encAAC, []string{"-f", "adts", "pipe:1"})...))
	t.Cleanup(srv.Close)
	pullStart(t, e, "live/aac", srv.URL+"/radio")
	s := waitForStream(t, e, "live/aac")
	pullCheckTracks(t, s, "AAC")
	pullCountUnits(t, s, 40)
	play(t, e.http.URL+"/live/aac/index.m3u8")
}

func TestPullHLS(t *testing.T) {
	e := start(t)
	dir := t.TempDir()
	publish(t, join(av264AAC, []string{"-f", "hls", "-hls_time", "1", "-hls_list_size", "6",
		"-hls_flags", "delete_segments", filepath.Join(dir, "index.m3u8")})...)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)

	// Wait for a playlist with a few segments before pulling.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(dir, "index.m3u8"))
		if strings.Count(string(b), "#EXTINF") >= 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	pullStart(t, e, "live/hls", srv.URL+"/index.m3u8")
	s := waitForStream(t, e, "live/hls")
	pullCheckTracks(t, s, "H264", "AAC")
	pullCountUnits(t, s, 25)
	play(t, e.http.URL+"/live/hls/index.m3u8")
}

func TestPullUDPUnicast(t *testing.T) {
	e := start(t)
	port := freeUDPPort(t)
	pullStart(t, e, "live/udp", "udp://127.0.0.1:"+port)
	time.Sleep(300 * time.Millisecond) // let the puller bind first
	publish(t, join(av264AAC, []string{"-f", "mpegts", "udp://127.0.0.1:" + port + "?pkt_size=1316"})...)
	s := waitForStream(t, e, "live/udp")
	pullCheckTracks(t, s, "H264", "AAC")
	pullCountUnits(t, s, 25)
	play(t, e.http.URL+"/live/udp/index.m3u8")
}

func TestPullSRTCaller(t *testing.T) {
	pullRequire(t, "protocols", "srt")
	e := start(t)
	port := freeUDPPort(t)
	publish(t, join(av264AAC, []string{"-f", "mpegts",
		"srt://127.0.0.1:" + port + "?mode=listener&pkt_size=1316&passphrase=pullsecret1&latency=200000"})...)
	m := pullStart(t, e, "live/srtpull", "srt://127.0.0.1:"+port+"?passphrase=pullsecret1&latency=200")
	s := waitForStream(t, e, "live/srtpull")
	pullCheckTracks(t, s, "H264", "AAC")
	pullCountUnits(t, s, 25)
	if st := m.Status(); strings.Contains(st[0].URL, "pullsecret1") {
		t.Fatalf("status leaks the passphrase: %s", st[0].URL)
	}
}

func TestPullRTMP(t *testing.T) {
	// FFmpeg as an RTMP server that plays to one client. (Pulling from
	// Streamble's own RTMP ingest needs RTMP playback there.)
	e := start(t)
	port := pullFreeTCPPort(t)
	publish(t, join(av264AAC, []string{"-f", "flv", "-listen", "1", "rtmp://127.0.0.1:" + port + "/app/src"})...)
	pullStart(t, e, "live/rtmppull", "rtmp://127.0.0.1:"+port+"/app/src")
	s := waitForStream(t, e, "live/rtmppull")
	pullCheckTracks(t, s, "H264", "AAC")
	pullCountUnits(t, s, 25)
	play(t, e.http.URL+"/live/rtmppull/index.m3u8")
}

func TestPullReconnect(t *testing.T) {
	e := start(t)
	// The first connection is cut after 3 seconds; later ones stay up.
	srv := httptest.NewServer(pullFFmpegHandler(t, "audio/aac", nil, 3*time.Second,
		join(inAudio, encAAC, []string{"-f", "adts", "pipe:1"})...))
	t.Cleanup(srv.Close)
	m := pullStart(t, e, "live/flaky", srv.URL+"/radio.aac")
	first := waitForStream(t, e, "live/flaky")

	deadline := time.Now().Add(20 * time.Second)
	var second *core.Stream
	for time.Now().Before(deadline) {
		if s := e.hub.Lookup("live/flaky"); s != nil && s != first {
			second = s
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second == nil {
		t.Fatal("stream was not republished after the source came back")
	}
	pullCountUnits(t, second, 40)
	st := m.Status()[0]
	if st.Reconnects < 1 || st.State != pull.StateLive {
		t.Fatalf("status %+v", st)
	}
}
