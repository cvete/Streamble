package e2e

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/stats"
	"github.com/cvete/streamble/internal/store"
)

// TestStatsRecorderWithHLSViewer publishes over RTMP, plays the HLS
// playlist and segments like a player, and checks that the recorder (with
// 200 ms "minutes") writes rows with viewers, bytes and countries.
func TestStatsRecorderWithHLSViewer(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	cfg := config.Default()
	cfg.HLS.Variants = []string{config.VariantMPEGTS}
	cfg.HLS.Primary = config.VariantMPEGTS
	cfg.HLS.SegmentDuration = config.Duration(time.Second)
	cfg.Apps = []config.App{{Name: "live", PublishKey: "k3y"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := core.NewHub(log)
	hm := hls.NewManager(cfg.HLS, "*", log)
	var geo *stats.GeoIP // no database: every viewer is "--"
	hm.Country = geo.Country
	hub.AddHook(hm)

	rs := &rtmp.Server{Addr: "127.0.0.1:0", Hub: hub, Auth: auth.New(cfg), Log: log}
	if err := rs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)
	hs := httptest.NewServer(hm)
	t.Cleanup(hs.Close)

	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	rec := &stats.Recorder{
		DB: db, Hub: hub, Viewers: hm.Viewers, Countries: hm.ViewersByCountry,
		Bitrates:      func(*core.Stream) (int64, int64) { return 1000, 500 },
		RetentionDays: 1, Log: log,
	}
	rec.SetIntervals(20*time.Millisecond, 200*time.Millisecond)
	rec.Start()
	t.Cleanup(rec.Close)

	publish(t, join(video264, []string{"-f", "flv", "rtmp://" + rs.ListenAddr().String() + "/live/stats?key=k3y"})...)
	waitForStream(t, &env{hub: hub}, "live/stats")

	// Poll the playlist and fetch every segment it lists, for a few
	// seconds, like a player.
	deadline := time.Now().Add(15 * time.Second)
	fetched := 0
	base := hs.URL + "/live/stats/"
	for time.Now().Before(deadline) && fetched < 3 {
		for _, uri := range statsURIs(statsGet(t, base+"index.m3u8")) {
			if !strings.Contains(uri, ".m3u8") { // a media playlist was served directly
				if statsGet(t, base+uri) != "" {
					fetched++
				}
				continue
			}
			for _, seg := range statsURIs(statsGet(t, base+uri)) {
				if statsGet(t, base+seg) != "" {
					fetched++
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if fetched == 0 {
		t.Fatal("no HLS segment could be fetched")
	}
	time.Sleep(500 * time.Millisecond) // let at least one more minute end
	rec.Close()

	ctx := context.Background()
	for _, path := range []string{"live/stats", stats.ServerPath} {
		var rows, viewers, bytesIn, bytesOut int64
		if err := db.QueryRow(ctx, `SELECT COUNT(*), MAX(viewers_max), SUM(bytes_in), SUM(bytes_out)
			FROM stats_minute WHERE path = ?`, path).Scan(&rows, &viewers, &bytesIn, &bytesOut); err != nil {
			t.Fatal(err)
		}
		if rows < 2 || viewers < 1 || bytesIn <= 0 || bytesOut <= 0 {
			t.Fatalf("path %q: rows=%d viewers_max=%d bytes_in=%d bytes_out=%d",
				path, rows, viewers, bytesIn, bytesOut)
		}
	}
	var n int64
	if err := db.QueryRow(ctx, `SELECT MAX(viewers) FROM stats_country WHERE path = ? AND country = ?`,
		"live/stats", stats.UnknownCountry).Scan(&n); err != nil || n < 1 {
		t.Fatalf("country rows: max viewers %d, err %v", n, err)
	}
}

// statsURIs returns the URI lines of a playlist.
func statsURIs(playlist string) []string {
	var out []string
	for _, line := range strings.Split(playlist, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func statsGet(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return string(b)
}
