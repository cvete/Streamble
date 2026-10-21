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
		"unknown profile": "apps:\n  - name: a\n    transcode: nope\n",
		"bad push scheme": "apps:\n  - name: a\n    push:\n      - url: http://x\n",
		"push no url":     "apps:\n  - name: a\n    push:\n      - name: yt\n",
		"bad hwaccel":     "transcode:\n  profiles:\n    - name: p\n      hwaccel: cuda\n      renditions: [{name: r}]\n",
		"no renditions":   "transcode:\n  profiles:\n    - name: p\n",
		"dup rendition":   "transcode:\n  profiles:\n    - name: p\n      renditions: [{name: r}, {name: r}]\n",
		"bad audio codec": "transcode:\n  profiles:\n    - name: p\n      renditions: [{name: r, audio_codec: flac}]\n",
		"source bad path": "sources:\n  - path: nope\n    url: http://x/y.ts\n",
		"source no app":   "sources:\n  - path: other/x\n    url: http://x/y.ts\n",
		"source no url":   "sources:\n  - path: live/x\n    url: x.ts\n",
	}
	for name, content := range cases {
		if _, err := Load(write(t, content)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestMilestone2Config(t *testing.T) {
	cfg, err := Load(write(t, `
transcode:
  profiles:
    - name: abr
      hwaccel: nvenc
      gop: 2s
      renditions:
        - {name: 720p, height: 720, video_bitrate: 2500k}
        - {name: radio, audio_only: true, audio_codec: mp3, audio_bitrate: 128k}
apps:
  - name: live
    transcode: abr
    play_key: viewer
    push:
      - name: youtube
        url: rtmp://a.rtmp.youtube.com/live2/{stream}
        streams: [tv1]
      - url: icecast://source:pw@radio.example:8000/{stream}.mp3
sources:
  - path: live/relay
    url: udp://239.1.1.1:5000
`))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Profile("abr"); p == nil || len(p.Renditions) != 2 || p.GOP != 2*time.Second {
		t.Fatalf("profile not parsed: %+v", p)
	}
	if a := cfg.App("live"); len(a.Push) != 2 || a.PlayKey != "viewer" {
		t.Fatalf("app not parsed: %+v", a)
	}
	if !cfg.Playback.HTTPTS || cfg.Transcode.FFmpeg != "ffmpeg" {
		t.Fatal("playback/transcode defaults lost")
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
