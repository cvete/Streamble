package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "streamble.yml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEmptyFileKeepsDefaults(t *testing.T) {
	cfg, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RTMP.Listen != ":1935" || cfg.HLS.Primary != VariantLLHLS {
		t.Fatalf("defaults lost: %+v", cfg)
	}
}

func TestLoadOverridesAndEnv(t *testing.T) {
	t.Setenv("TV1_KEY", "s3cret")
	cfg, err := Load(write(t, `
http:
  listen: ":9000"
hls:
  variants: [mpegts]
  segment_count: 5
  segment_duration: 4s
apps:
  - name: tv
    stream_keys:
      tv1: ${TV1_KEY}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != ":9000" || cfg.HLS.SegmentDuration != 4*time.Second {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.HLS.Primary != VariantMPEGTS {
		t.Fatalf("primary should fall back to first variant, got %q", cfg.HLS.Primary)
	}
	if cfg.App("tv").StreamKeys["tv1"] != "s3cret" {
		t.Fatal("env var not expanded")
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":   "nope: 1\n",
		"unknown variant": "hls:\n  variants: [dash]\n",
		"bad app name":    "apps:\n  - name: 'a/b'\n",
		"short srt pass":  "apps:\n  - name: a\n    srt_passphrase: short\n",
		"ll segments":     "hls:\n  segment_count: 3\n",
		"no ingest":       "rtmp:\n  enabled: false\nsrt:\n  enabled: false\n",
	}
	for name, content := range cases {
		if _, err := Load(write(t, content)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestUnsetEnvVarIsLeftVisible(t *testing.T) {
	cfg, err := Load(write(t, "apps:\n  - name: a\n    publish_key: ${DEFINITELY_NOT_SET_123}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.App("a").PublishKey, "DEFINITELY_NOT_SET_123") {
		t.Fatal("unset variable should stay literal, not become an empty (open) key")
	}
}
