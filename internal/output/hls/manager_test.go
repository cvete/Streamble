package hls

import (
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
)

func TestRoute(t *testing.T) {
	cfg := config.Default().HLS
	m := NewManager(cfg, "*", slog.Default())

	cases := []struct {
		in                  string
		path, variant, file string
		ok                  bool
	}{
		{"/live/tv1/index.m3u8", "live/tv1", "llhls", "index.m3u8", true},
		{"/live/tv1/mpegts/index.m3u8", "live/tv1", "mpegts", "index.m3u8", true},
		{"/live/tv1/fmp4/index.m3u8", "", "", "", false}, // fmp4 not enabled by default
		{"/live/index.m3u8", "", "", "", false},
		{"/a/b/c/d/e", "", "", "", false},
	}
	for _, c := range cases {
		p, v, f, ok := m.route(c.in)
		if ok != c.ok || p != c.path || v != c.variant || f != c.file {
			t.Errorf("%s: got (%q %q %q %v)", c.in, p, v, f, ok)
		}
	}
}

func TestDurationToTicksNoOverflow(t *testing.T) {
	if got := durationToTicks(1500*time.Millisecond, 90000); got != 135000 {
		t.Fatalf("got %d", got)
	}
	// 100 days of uptime must not overflow.
	d := 100 * 24 * time.Hour
	got := durationToTicks(d, 90000)
	want := int64(d/time.Second) * 90000
	if got != want || got <= 0 || got > math.MaxInt64/2 {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestCodecVariantSupport(t *testing.T) {
	hevc := core.NewTrack(&core.H265{})
	opus := core.NewTrack(&core.Opus{ChannelCount: 2})
	for _, tr := range []*core.Track{hevc, opus} {
		if toHLSTrack(tr, config.VariantMPEGTS) != nil {
			t.Errorf("%s must not be offered over MPEG-TS HLS", tr.Codec.Name())
		}
		if toHLSTrack(tr, config.VariantLLHLS) == nil {
			t.Errorf("%s must be offered over LL-HLS", tr.Codec.Name())
		}
	}
	if toHLSTrack(core.NewTrack(&core.AAC{}), config.VariantFMP4) != nil {
		t.Error("AAC without config must be skipped")
	}
}

func TestViewerTracker(t *testing.T) {
	v := newViewerTracker()
	v.touch("a")
	v.touch("b")
	v.touch("a")
	if v.count() != 2 {
		t.Fatalf("expected 2 viewers, got %d", v.count())
	}
	v.seen["a"] = time.Now().Add(-2 * viewerWindow)
	if v.count() != 1 {
		t.Fatal("stale viewer not pruned")
	}
}
