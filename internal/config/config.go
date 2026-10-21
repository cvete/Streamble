// Package config loads the Streamble configuration file.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of streamble.yml.
type Config struct {
	LogLevel string `yaml:"log_level"`
	RTMP     RTMP   `yaml:"rtmp"`
	SRT      SRT    `yaml:"srt"`
	HTTP     HTTP   `yaml:"http"`
	HLS      HLS    `yaml:"hls"`
	Apps     []App  `yaml:"apps"`
}

// RTMP configures the RTMP ingest listener.
type RTMP struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// SRT configures the SRT ingest listener.
type SRT struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	// LatencyMS is the SRT receiver latency in milliseconds.
	LatencyMS int `yaml:"latency_ms"`
}

// HTTP configures the HTTP server (HLS playback, API, player page).
type HTTP struct {
	Listen string `yaml:"listen"`
	// AllowOrigin is sent as Access-Control-Allow-Origin on playback
	// responses so web players on other domains can load streams.
	AllowOrigin string `yaml:"allow_origin"`
	// APIToken, when set, is required as "Authorization: Bearer <token>"
	// on /api/ endpoints.
	APIToken string `yaml:"api_token"`
}

// HLS configures HLS output.
type HLS struct {
	// Variants to produce for every stream. Supported values:
	//   "llhls"  Apple Low-Latency HLS (fMP4 parts), lowest latency
	//   "fmp4"   standard HLS with fMP4 segments, CDN friendly
	//   "mpegts" classic HLS with MPEG-TS segments, widest device support
	Variants []string `yaml:"variants"`
	// Primary is the variant served at /<stream>/index.m3u8.
	// Every variant is also served at /<stream>/<variant>/index.m3u8.
	Primary         string        `yaml:"primary"`
	SegmentCount    int           `yaml:"segment_count"`
	SegmentDuration time.Duration `yaml:"segment_duration"`
	PartDuration    time.Duration `yaml:"part_duration"`
}

// App groups streams under a common prefix, like "live" in
// rtmp://host/live/mystream.
type App struct {
	Name string `yaml:"name"`
	// PublishKey, when set, must be supplied by publishers (see README).
	// When empty, anyone can publish to this app.
	PublishKey string `yaml:"publish_key"`
	// StreamKeys optionally assigns a dedicated key per stream name and
	// takes precedence over PublishKey.
	StreamKeys map[string]string `yaml:"stream_keys"`
	// SRTPassphrase, when set, requires SRT publishers to encrypt with it
	// (10 to 79 characters).
	SRTPassphrase string `yaml:"srt_passphrase"`
}

// Variant names.
const (
	VariantLLHLS  = "llhls"
	VariantFMP4   = "fmp4"
	VariantMPEGTS = "mpegts"
)

// Names may not start with '.', which rules out "." and "..".
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

// Default returns a configuration with sensible defaults.
func Default() *Config {
	return &Config{
		LogLevel: "info",
		RTMP:     RTMP{Enabled: true, Listen: ":1935"},
		SRT:      SRT{Enabled: true, Listen: ":8890", LatencyMS: 200},
		HTTP:     HTTP{Listen: ":8080", AllowOrigin: "*"},
		HLS: HLS{
			Variants:        []string{VariantLLHLS, VariantMPEGTS},
			Primary:         VariantLLHLS,
			SegmentCount:    7,
			SegmentDuration: 2 * time.Second,
			PartDuration:    200 * time.Millisecond,
		},
		Apps: []App{{Name: "live"}},
	}
}

// Load reads and validates a configuration file. Values not present in the
// file keep their defaults. Strings of the form ${VAR} are replaced with
// environment variables, so secrets can stay out of the file.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, cfg.Validate()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := os.Expand(string(raw), func(k string) string {
		v, ok := os.LookupEnv(k)
		if !ok {
			return "${" + k + "}"
		}
		return v
	})

	// Unless the file sets hls.primary explicitly, it falls back to the
	// first listed variant (see Validate), so overriding only hls.variants
	// never leaves an invalid primary behind.
	cfg.HLS.Primary = ""

	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, cfg.Validate()
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !c.RTMP.Enabled && !c.SRT.Enabled {
		add("at least one ingest (rtmp or srt) must be enabled")
	}
	if c.HTTP.Listen == "" {
		add("http.listen is required")
	}

	if len(c.HLS.Variants) == 0 {
		add("hls.variants must list at least one variant")
	}
	seen := map[string]bool{}
	for _, v := range c.HLS.Variants {
		switch v {
		case VariantLLHLS, VariantFMP4, VariantMPEGTS:
		default:
			add("hls.variants: unknown variant %q (use llhls, fmp4 or mpegts)", v)
		}
		if seen[v] {
			add("hls.variants: %q listed twice", v)
		}
		seen[v] = true
	}
	if c.HLS.Primary == "" && len(c.HLS.Variants) > 0 {
		c.HLS.Primary = c.HLS.Variants[0]
	}
	if !seen[c.HLS.Primary] {
		add("hls.primary %q must be one of hls.variants", c.HLS.Primary)
	}
	if seen[VariantLLHLS] && c.HLS.SegmentCount < 7 {
		add("hls.segment_count must be at least 7 when llhls is enabled")
	}
	if c.HLS.SegmentCount < 3 {
		add("hls.segment_count must be at least 3")
	}
	if c.HLS.SegmentDuration <= 0 || c.HLS.PartDuration <= 0 {
		add("hls.segment_duration and hls.part_duration must be positive")
	}

	if len(c.Apps) == 0 {
		add("at least one app must be configured")
	}
	apps := map[string]bool{}
	for _, a := range c.Apps {
		if !nameRe.MatchString(a.Name) {
			add("app name %q may only contain letters, digits, '_', '-' and '.'", a.Name)
		}
		if apps[a.Name] {
			add("app %q is defined twice", a.Name)
		}
		apps[a.Name] = true
		if p := a.SRTPassphrase; p != "" && (len(p) < 10 || len(p) > 79) {
			add("app %q: srt_passphrase must be 10 to 79 characters", a.Name)
		}
		for name := range a.StreamKeys {
			if !nameRe.MatchString(name) {
				add("app %q: invalid stream name %q in stream_keys", a.Name, name)
			}
		}
	}

	return errors.Join(errs...)
}

// App returns the app with the given name, or nil.
func (c *Config) App(name string) *App {
	for i := range c.Apps {
		if c.Apps[i].Name == name {
			return &c.Apps[i]
		}
	}
	return nil
}

// ValidStreamName reports whether s is an acceptable stream name.
func ValidStreamName(s string) bool { return nameRe.MatchString(s) }
