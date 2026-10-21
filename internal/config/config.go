// Package config loads the Streamble configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of streamble.yml.
//
// It has two kinds of settings:
//   - server settings (listeners, HLS, FFmpeg path, storage, panel, stats,
//     GeoIP): read from the file at startup; changing them needs a restart.
//   - runtime settings (Playback, Transcode.Profiles, Apps, Sources,
//     Webhooks): stored in the database and editable live from the panel or
//     API. On the first start with an empty database they are imported from
//     the file; after that the database is the source of truth.
type Config struct {
	LogLevel  string    `yaml:"log_level" json:"log_level"`
	RTMP      RTMP      `yaml:"rtmp" json:"rtmp"`
	SRT       SRT       `yaml:"srt" json:"srt"`
	HTTP      HTTP      `yaml:"http" json:"http"`
	HLS       HLS       `yaml:"hls" json:"hls"`
	Storage   Storage   `yaml:"storage" json:"storage"`
	Panel     Panel     `yaml:"panel" json:"panel"`
	Stats     Stats     `yaml:"stats" json:"stats"`
	GeoIP     GeoIP     `yaml:"geoip" json:"geoip"`
	Playback  Playback  `yaml:"playback" json:"playback"`
	Transcode Transcode `yaml:"transcode" json:"transcode"`
	Apps      []App     `yaml:"apps" json:"apps"`
	Sources   []Source  `yaml:"sources" json:"sources"`
	Webhooks  []Webhook `yaml:"webhooks" json:"webhooks"`
}

// Storage selects the database holding runtime settings, users and stats.
type Storage struct {
	// Driver is "sqlite" (default, a single file, no setup) or "postgres".
	Driver string `yaml:"driver" json:"driver"`
	// DSN is a file path for sqlite, or a connection URL for postgres:
	// postgres://user:password@host:5432/dbname?sslmode=disable
	DSN string `yaml:"dsn" json:"-"`
}

// Panel configures the web control panel and its logins.
type Panel struct {
	// Enabled serves the control panel at /panel/.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// SecureCookies marks session cookies Secure. Enable when the panel is
	// reached over HTTPS (directly or through a reverse proxy).
	SecureCookies bool `yaml:"secure_cookies" json:"secure_cookies"`
	// SessionTTL is how long a login lasts.
	SessionTTL Duration `yaml:"session_ttl" json:"session_ttl"`
}

// Stats configures the statistics history.
type Stats struct {
	// RetentionDays is how long per-minute statistics are kept.
	RetentionDays int `yaml:"retention_days" json:"retention_days"`
}

// GeoIP enables viewer countries.
type GeoIP struct {
	// Database is the path to a MaxMind GeoLite2-Country (or compatible)
	// .mmdb file. Empty disables country statistics.
	Database string `yaml:"database" json:"database"`
}

// Playback enables the non-HLS ways to watch or listen to a stream.
type Playback struct {
	// HTTPTS serves /<app>/<stream>.ts as a continuous MPEG-TS stream
	// (IPTV players, VLC, set-top boxes, other servers).
	HTTPTS bool `yaml:"http_ts" json:"http_ts"`
	// HTTPAudio serves /<app>/<stream>.mp3 and /<app>/<stream>.aac as
	// Icecast-style radio streams when the stream has that audio codec.
	HTTPAudio bool `yaml:"http_audio" json:"http_audio"`
	// RTMP allows playing rtmp://host/<app>/<stream>.
	RTMP bool `yaml:"rtmp" json:"rtmp"`
	// SRT allows reading with streamid=read:<app>/<stream>.
	SRT bool `yaml:"srt" json:"srt"`
}

// Transcode configures FFmpeg-based transcoding.
type Transcode struct {
	// FFmpeg is the path to the ffmpeg binary (server setting).
	FFmpeg string `yaml:"ffmpeg" json:"ffmpeg"`
	// Profiles are runtime settings.
	Profiles []Profile `yaml:"profiles" json:"profiles"`
}

// Profile is a named transcoding recipe that apps refer to.
type Profile struct {
	Name string `yaml:"name" json:"name"`
	// HWAccel selects the video encoder: none (libx264), nvenc, qsv, vaapi.
	HWAccel string `yaml:"hwaccel" json:"hwaccel"`
	// VAAPIDevice is the render node for vaapi, e.g. /dev/dri/renderD128.
	VAAPIDevice string `yaml:"vaapi_device" json:"vaapi_device"`
	// Preset is the encoder speed preset (libx264: veryfast, nvenc: p4, ...).
	Preset string `yaml:"preset" json:"preset"`
	// GOP is the keyframe interval. Renditions share it so players can
	// switch between them at segment boundaries.
	GOP        Duration    `yaml:"gop" json:"gop"`
	Renditions []Rendition `yaml:"renditions" json:"renditions"`
}

// Rendition is one output of a transcoding profile. It is published as
// "<app>/<stream>_<name>".
type Rendition struct {
	Name string `yaml:"name" json:"name"`
	// Height in pixels; width follows the source aspect ratio.
	// 0 keeps the source size.
	Height int `yaml:"height" json:"height"`
	// FPS caps the frame rate; 0 keeps the source rate.
	FPS          int    `yaml:"fps" json:"fps"`
	VideoBitrate string `yaml:"video_bitrate" json:"video_bitrate"` // e.g. 2500k
	AudioBitrate string `yaml:"audio_bitrate" json:"audio_bitrate"` // e.g. 128k
	// AudioOnly drops video (radio, low-bandwidth listeners).
	AudioOnly bool `yaml:"audio_only" json:"audio_only"`
	// AudioCodec is aac (default, plays everywhere) or mp3.
	AudioCodec string `yaml:"audio_codec" json:"audio_codec"`
}

// Push is a restream target.
type Push struct {
	// Name identifies the target in logs and the API.
	Name string `yaml:"name" json:"name"`
	// URL of the target. Supported schemes:
	//   rtmp, rtmps  the last path segment (plus query) is the stream key,
	//                e.g. rtmp://a.rtmp.youtube.com/live2/<key>
	//   srt          caller mode; streamid and passphrase as query parameters
	//   icecast, icecasts (TLS)  icecast://source:<password>@host:8000/mount
	// "{app}" and "{stream}" are replaced with the stream's app and name.
	URL string `yaml:"url" json:"url"`
	// Streams limits the target to these stream names; empty means every
	// stream of the app.
	Streams []string `yaml:"streams" json:"streams"`
	// Disabled pauses the target without deleting it.
	Disabled bool `yaml:"disabled" json:"disabled"`
}

// Source is a stream pulled from elsewhere and published into the hub.
type Source struct {
	// Path to publish at, "<app>/<stream>". The app must exist.
	Path string `yaml:"path" json:"path"`
	// URL to pull. Supported: http(s) MPEG-TS (.ts), Icecast/Shoutcast MP3
	// or AAC, HLS (.m3u8), udp:// (unicast or multicast MPEG-TS),
	// srt:// (caller mode), rtmp(s)://.
	URL string `yaml:"url" json:"url"`
	// Disabled pauses the source without deleting it.
	Disabled bool `yaml:"disabled" json:"disabled"`
}

// Webhook sends signed JSON events to an HTTP endpoint.
type Webhook struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
	// Secret signs each request: X-Streamble-Signature: sha256=<hex HMAC of
	// the body>. Optional but recommended.
	Secret string `yaml:"secret" json:"secret"`
	// Events to send; empty means all. See the Event* constants.
	Events   []string `yaml:"events" json:"events"`
	Disabled bool     `yaml:"disabled" json:"disabled"`
}

// Webhook event names.
const (
	EventStreamPublished = "stream.published"
	EventStreamEnded     = "stream.ended"
	EventPushState       = "push.state"
	EventSourceState     = "source.state"
	EventTranscodeState  = "transcode.state"
)

// Events lists every webhook event name.
var Events = []string{EventStreamPublished, EventStreamEnded, EventPushState, EventSourceState, EventTranscodeState}

// RTMP configures the RTMP ingest listener.
type RTMP struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Listen  string `yaml:"listen" json:"listen"`
}

// SRT configures the SRT ingest listener.
type SRT struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Listen  string `yaml:"listen" json:"listen"`
	// LatencyMS is the SRT receiver latency in milliseconds.
	LatencyMS int `yaml:"latency_ms" json:"latency_ms"`
}

// HTTP configures the HTTP server (HLS playback, API, panel).
type HTTP struct {
	Listen string `yaml:"listen" json:"listen"`
	// AllowOrigin is sent as Access-Control-Allow-Origin on playback
	// responses so web players on other domains can load streams.
	AllowOrigin string `yaml:"allow_origin" json:"allow_origin"`
	// APIToken is a legacy static admin token, accepted as
	// "Authorization: Bearer <token>". Prefer per-user API tokens.
	APIToken string `yaml:"api_token" json:"-"`
}

// HLS configures HLS output.
type HLS struct {
	// Variants to produce for every stream. Supported values:
	//   "llhls"  Apple Low-Latency HLS (fMP4 parts), lowest latency
	//   "fmp4"   standard HLS with fMP4 segments, CDN friendly
	//   "mpegts" classic HLS with MPEG-TS segments, widest device support
	Variants []string `yaml:"variants" json:"variants"`
	// Primary is the variant served at /<stream>/index.m3u8.
	// Every variant is also served at /<stream>/<variant>/index.m3u8.
	Primary         string   `yaml:"primary" json:"primary"`
	SegmentCount    int      `yaml:"segment_count" json:"segment_count"`
	SegmentDuration Duration `yaml:"segment_duration" json:"segment_duration"`
	PartDuration    Duration `yaml:"part_duration" json:"part_duration"`
}

// App groups streams under a common prefix, like "live" in
// rtmp://host/live/mystream.
type App struct {
	Name string `yaml:"name" json:"name"`
	// Description is a free-form note shown in the panel.
	Description string `yaml:"description" json:"description"`
	// PublishKey, when set, must be supplied by publishers (see README).
	// When empty, anyone can publish to this app.
	PublishKey string `yaml:"publish_key" json:"publish_key"`
	// StreamKeys optionally assigns a dedicated key per stream name and
	// takes precedence over PublishKey.
	StreamKeys map[string]string `yaml:"stream_keys" json:"stream_keys"`
	// SRTPassphrase, when set, requires SRT publishers to encrypt with it
	// (10 to 79 characters).
	SRTPassphrase string `yaml:"srt_passphrase" json:"srt_passphrase"`
	// PlayKey, when set, is required to play over RTMP, SRT, HTTP MPEG-TS
	// and HTTP audio (as "?key=" or in the SRT stream ID). HLS is not
	// covered yet; signed HLS URLs are planned.
	PlayKey string `yaml:"play_key" json:"play_key"`
	// AuthURL, when set, is asked to allow every publish and play request
	// of this app (after key checks); see the README.
	AuthURL string `yaml:"auth_url" json:"auth_url"`
	// Transcode names a transcode profile applied to every stream
	// published in this app.
	Transcode string `yaml:"transcode" json:"transcode"`
	// Push lists restream targets for streams of this app.
	Push []Push `yaml:"push" json:"push"`
}

// Variant names.
const (
	VariantLLHLS  = "llhls"
	VariantFMP4   = "fmp4"
	VariantMPEGTS = "mpegts"
)

// Names may not start with '.', which rules out "." and "..".
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

// ReservedAppNames cannot be used as app names because the HTTP server
// uses them as path prefixes.
var ReservedAppNames = []string{"api", "panel"}

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
			SegmentDuration: Duration(2 * time.Second),
			PartDuration:    Duration(200 * time.Millisecond),
		},
		Storage:   Storage{Driver: "sqlite", DSN: "data/streamble.db"},
		Panel:     Panel{Enabled: true, SessionTTL: Duration(7 * 24 * time.Hour)},
		Stats:     Stats{RetentionDays: 30},
		Playback:  Playback{HTTPTS: true, HTTPAudio: true, RTMP: true, SRT: true},
		Transcode: Transcode{FFmpeg: "ffmpeg"},
		Apps:      []App{{Name: "live"}},
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

	switch c.Storage.Driver {
	case "sqlite", "postgres":
	default:
		add("storage.driver must be sqlite or postgres")
	}
	if c.Storage.DSN == "" {
		add("storage.dsn is required")
	}
	if c.Panel.SessionTTL <= 0 {
		add("panel.session_ttl must be positive")
	}
	if c.Stats.RetentionDays < 1 {
		add("stats.retention_days must be at least 1")
	}

	errs = append(errs, c.ValidateRuntime())
	return errors.Join(errs...)
}

// ValidateRuntime checks only the runtime settings (apps, profiles,
// sources, webhooks). The panel and API call it before applying a change.
func (c *Config) ValidateRuntime() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	apps := map[string]bool{}
	for _, a := range c.Apps {
		if !nameRe.MatchString(a.Name) {
			add("app name %q may only contain letters, digits, '_', '-' and '.'", a.Name)
		}
		for _, r := range ReservedAppNames {
			if strings.EqualFold(a.Name, r) {
				add("app name %q is reserved", a.Name)
			}
		}
		if a.AuthURL != "" && !strings.HasPrefix(a.AuthURL, "http://") && !strings.HasPrefix(a.AuthURL, "https://") {
			add("app %q: auth_url must be an http:// or https:// URL", a.Name)
		}
		pushNames := map[string]bool{}
		for _, p := range a.Push {
			if p.Name != "" {
				if pushNames[p.Name] {
					add("app %q: push target name %q used twice", a.Name, p.Name)
				}
				pushNames[p.Name] = true
			}
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
		if a.Transcode != "" && c.Profile(a.Transcode) == nil {
			add("app %q: unknown transcode profile %q", a.Name, a.Transcode)
		}
		for i, p := range a.Push {
			if p.URL == "" {
				add("app %q: push[%d] has no url", a.Name, i)
				continue
			}
			scheme, _, ok := strings.Cut(p.URL, "://")
			switch {
			case !ok:
				add("app %q: push[%d] url must start with a scheme like rtmp://", a.Name, i)
			case scheme != "rtmp" && scheme != "rtmps" && scheme != "srt" && scheme != "icecast" && scheme != "icecasts":
				add("app %q: push[%d] scheme %q not supported (rtmp, rtmps, srt, icecast, icecasts)", a.Name, i, scheme)
			}
		}
	}

	profiles := map[string]bool{}
	for _, p := range c.Transcode.Profiles {
		if !nameRe.MatchString(p.Name) {
			add("transcode profile name %q is invalid", p.Name)
		}
		if profiles[p.Name] {
			add("transcode profile %q is defined twice", p.Name)
		}
		profiles[p.Name] = true
		switch p.HWAccel {
		case "", "none", "nvenc", "qsv", "vaapi":
		default:
			add("transcode profile %q: hwaccel must be none, nvenc, qsv or vaapi", p.Name)
		}
		if len(p.Renditions) == 0 {
			add("transcode profile %q has no renditions", p.Name)
		}
		names := map[string]bool{}
		for _, r := range p.Renditions {
			if !nameRe.MatchString(r.Name) {
				add("transcode profile %q: invalid rendition name %q", p.Name, r.Name)
			}
			if names[r.Name] {
				add("transcode profile %q: rendition %q defined twice", p.Name, r.Name)
			}
			names[r.Name] = true
			if r.Height < 0 || r.FPS < 0 {
				add("transcode profile %q: rendition %q has a negative height or fps", p.Name, r.Name)
			}
			if r.AudioCodec != "" && r.AudioCodec != "aac" && r.AudioCodec != "mp3" {
				add("transcode profile %q: rendition %q audio_codec must be aac or mp3", p.Name, r.Name)
			}
		}
	}

	paths := map[string]bool{}
	for i, s := range c.Sources {
		parts := strings.Split(s.Path, "/")
		if len(parts) != 2 || !nameRe.MatchString(parts[0]) || !nameRe.MatchString(parts[1]) {
			add("sources[%d]: path %q must be <app>/<stream>", i, s.Path)
		} else if c.App(parts[0]) == nil {
			add("sources[%d]: app %q is not defined", i, parts[0])
		}
		if paths[s.Path] {
			add("sources[%d]: path %q is used twice", i, s.Path)
		}
		paths[s.Path] = true
		if !strings.Contains(s.URL, "://") {
			add("sources[%d]: url must start with a scheme like http://", i)
		}
	}

	hooks := map[string]bool{}
	for i, w := range c.Webhooks {
		if !nameRe.MatchString(w.Name) {
			add("webhooks[%d]: invalid name %q", i, w.Name)
		}
		if hooks[w.Name] {
			add("webhook %q is defined twice", w.Name)
		}
		hooks[w.Name] = true
		if !strings.HasPrefix(w.URL, "http://") && !strings.HasPrefix(w.URL, "https://") {
			add("webhook %q: url must be an http:// or https:// URL", w.Name)
		}
		for _, e := range w.Events {
			if !slices.Contains(Events, e) {
				add("webhook %q: unknown event %q", w.Name, e)
			}
		}
	}

	return errors.Join(errs...)
}

// Source returns the source with the given path, or nil.
func (c *Config) Source(path string) *Source {
	for i := range c.Sources {
		if c.Sources[i].Path == path {
			return &c.Sources[i]
		}
	}
	return nil
}

// Webhook returns the webhook with the given name, or nil.
func (c *Config) Webhook(name string) *Webhook {
	for i := range c.Webhooks {
		if c.Webhooks[i].Name == name {
			return &c.Webhooks[i]
		}
	}
	return nil
}

// Clone returns a deep copy, so a snapshot can be edited and validated
// without affecting readers of the original.
func (c *Config) Clone() *Config {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // all fields are plain data
	}
	out := &Config{}
	if err := json.Unmarshal(b, out); err != nil {
		panic(err)
	}
	// Fields hidden from JSON.
	out.Storage.DSN = c.Storage.DSN
	out.HTTP.APIToken = c.HTTP.APIToken
	return out
}

// Profile returns the transcode profile with the given name, or nil.
func (c *Config) Profile(name string) *Profile {
	for i := range c.Transcode.Profiles {
		if c.Transcode.Profiles[i].Name == name {
			return &c.Transcode.Profiles[i]
		}
	}
	return nil
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
