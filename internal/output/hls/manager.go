// Package hls serves every live stream as HLS: Apple Low-Latency HLS,
// standard fMP4 HLS and classic MPEG-TS HLS, depending on configuration.
//
// URLs (for stream "live/tv1"):
//
//	/live/tv1/index.m3u8           primary variant
//	/live/tv1/<variant>/index.m3u8 a specific variant (llhls, fmp4, mpegts)
package hls

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2"
	hlscodecs "github.com/bluenviron/gohlslib/v2/pkg/codecs"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
)

// subscriberBuffer is the per-muxer queue length, in units. At 30 fps video
// plus audio this is several seconds of headroom.
const subscriberBuffer = 2048

// Manager creates HLS muxers for published streams and serves them over HTTP.
// It implements core.Hook.
type Manager struct {
	cfg         config.HLS
	allowOrigin string
	log         *slog.Logger

	// ABR, when set, returns the rendition stream paths of a transcoded
	// source stream, in ladder order. It enables /<stream>/abr.m3u8.
	ABR func(path string) []string

	// Country, when set, maps a client IP address to an ISO country code
	// (or "--"). It is called once per new viewer; see ViewersByCountry.
	Country func(ip string) string

	mu      sync.RWMutex
	streams map[string]*hlsStream
}

// NewManager creates a Manager.
func NewManager(cfg config.HLS, allowOrigin string, log *slog.Logger) *Manager {
	return &Manager{
		cfg:         cfg,
		allowOrigin: allowOrigin,
		log:         log,
		streams:     make(map[string]*hlsStream),
	}
}

type hlsStream struct {
	stream  *core.Stream
	muxers  map[string]*gohlslib.Muxer
	viewers *viewerTracker
}

// OnPublish implements core.Hook.
func (m *Manager) OnPublish(s *core.Stream) {
	hs := &hlsStream{
		stream:  s,
		muxers:  make(map[string]*gohlslib.Muxer),
		viewers: newViewerTracker(),
	}

	for _, variant := range m.cfg.Variants {
		mux, trackMap := m.newMuxer(s, variant)
		if mux == nil {
			continue
		}
		sub := s.Subscribe("hls-"+variant, subscriberBuffer)
		if sub == nil {
			mux.Close()
			return // stream already ended
		}
		hs.muxers[variant] = mux
		go m.pump(s.Path, variant, sub, mux, trackMap)
	}

	m.mu.Lock()
	m.streams[s.Path] = hs
	m.mu.Unlock()
}

// OnUnpublish implements core.Hook. Subscriptions are closed by the hub,
// which ends each pump goroutine; the pump closes its muxer.
func (m *Manager) OnUnpublish(s *core.Stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if hs, ok := m.streams[s.Path]; ok && hs.stream == s {
		delete(m.streams, s.Path)
	}
}

func (m *Manager) newMuxer(s *core.Stream, variant string) (*gohlslib.Muxer, map[*core.Track]*gohlslib.Track) {
	trackMap := make(map[*core.Track]*gohlslib.Track)
	var tracks []*gohlslib.Track
	for _, t := range s.Tracks {
		ht := toHLSTrack(t, variant)
		if ht == nil {
			m.log.Info("codec not supported by this HLS variant, track skipped",
				"path", s.Path, "variant", variant, "codec", t.Codec.Name())
			continue
		}
		trackMap[t] = ht
		tracks = append(tracks, ht)
	}
	if len(tracks) == 0 {
		return nil, nil
	}

	mux := &gohlslib.Muxer{
		Tracks:             tracks,
		Variant:            muxerVariant(variant),
		SegmentCount:       m.cfg.SegmentCount,
		SegmentMinDuration: m.cfg.SegmentDuration.D(),
		PartMinDuration:    m.cfg.PartDuration.D(),
		OnEncodeError: func(err error) {
			m.log.Debug("HLS encode error", "path", s.Path, "variant", variant, "err", err)
		},
	}
	if err := mux.Start(); err != nil {
		m.log.Error("cannot start HLS muxer", "path", s.Path, "variant", variant, "err", err)
		return nil, nil
	}
	return mux, trackMap
}

// pump moves units from the hub into one muxer until the stream ends.
func (m *Manager) pump(path, variant string, sub *core.Subscription, mux *gohlslib.Muxer, tracks map[*core.Track]*gohlslib.Track) {
	defer mux.Close()
	var lastErr time.Time

	for u := range sub.C() {
		ht := tracks[u.Track]
		if ht == nil {
			continue
		}
		pts := durationToTicks(u.PTS, ht.ClockRate)

		var err error
		switch ht.Codec.(type) {
		case *hlscodecs.H264:
			err = mux.WriteH264(ht, u.NTP, pts, u.Payload)
		case *hlscodecs.H265:
			err = mux.WriteH265(ht, u.NTP, pts, u.Payload)
		case *hlscodecs.MPEG4Audio:
			err = mux.WriteMPEG4Audio(ht, u.NTP, pts, u.Payload)
		case *hlscodecs.Opus:
			err = mux.WriteOpus(ht, u.NTP, pts, u.Payload)
		}
		// Log at most once per 10 seconds per muxer to avoid flooding.
		if err != nil && time.Since(lastErr) > 10*time.Second {
			lastErr = time.Now()
			m.log.Warn("HLS muxer write failed", "path", path, "variant", variant, "err", err)
		}
	}
}

func muxerVariant(v string) gohlslib.MuxerVariant {
	switch v {
	case config.VariantMPEGTS:
		return gohlslib.MuxerVariantMPEGTS
	case config.VariantFMP4:
		return gohlslib.MuxerVariantFMP4
	default:
		return gohlslib.MuxerVariantLowLatency
	}
}

// toHLSTrack converts a hub track; it returns nil when the variant cannot
// carry the codec. MPEG-TS HLS supports H.264 and AAC only; Apple requires
// fMP4 for HEVC and Opus, so those go to the llhls and fmp4 variants.
func toHLSTrack(t *core.Track, variant string) *gohlslib.Track {
	var c hlscodecs.Codec
	switch tc := t.Codec.(type) {
	case *core.H264:
		c = &hlscodecs.H264{SPS: tc.SPS, PPS: tc.PPS}
	case *core.H265:
		if variant == config.VariantMPEGTS {
			return nil
		}
		c = &hlscodecs.H265{VPS: tc.VPS, SPS: tc.SPS, PPS: tc.PPS}
	case *core.AAC:
		if tc.Config == nil {
			return nil
		}
		c = &hlscodecs.MPEG4Audio{Config: *tc.Config}
	case *core.Opus:
		if variant == config.VariantMPEGTS {
			return nil
		}
		c = &hlscodecs.Opus{ChannelCount: tc.ChannelCount}
	default:
		return nil
	}
	return &gohlslib.Track{Codec: c, ClockRate: t.ClockRate}
}

// durationToTicks converts without overflowing for long-running streams.
func durationToTicks(d time.Duration, clockRate int) int64 {
	secs := int64(d / time.Second)
	rem := int64(d % time.Second)
	return secs*int64(clockRate) + rem*int64(clockRate)/int64(time.Second)
}

// ServeHTTP serves HLS playlists and segments.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.allowOrigin != "" {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", m.allowOrigin)
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Range")
		h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
	}
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet, http.MethodHead:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path, variant, file, ok := m.route(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if file == abrPlaylist && m.ABR != nil {
		m.serveABR(w, r, path, variant, strings.Count(strings.Trim(r.URL.Path, "/"), "/"))
		return
	}

	m.mu.RLock()
	hs := m.streams[path]
	m.mu.RUnlock()
	if hs == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	mux := hs.muxers[variant]
	if mux == nil {
		http.Error(w, "variant not available", http.StatusNotFound)
		return
	}

	if strings.HasSuffix(file, ".m3u8") {
		var country func() string
		if lookup := m.Country; lookup != nil {
			country = func() string { return lookup(remoteIP(r)) }
		}
		hs.viewers.touchFrom(clientID(r), country)
	}

	cw := &countingWriter{ResponseWriter: w}
	mux.Handle(cw, r)
	if !cw.wroteHeader {
		// gohlslib writes nothing for unknown files.
		http.NotFound(w, r)
		return
	}
	hs.stream.BytesOut.Add(cw.n)
}

// route splits /app/stream[/variant]/file.
func (m *Manager) route(urlPath string) (path, variant, file string, ok bool) {
	parts := strings.Split(strings.Trim(urlPath, "/"), "/")
	switch len(parts) {
	case 3:
		return parts[0] + "/" + parts[1], m.cfg.Primary, parts[2], true
	case 4:
		for _, v := range m.cfg.Variants {
			if v == parts[2] {
				return parts[0] + "/" + parts[1], v, parts[3], true
			}
		}
	}
	return "", "", "", false
}

// Viewers returns the estimated number of current HLS viewers of a stream.
func (m *Manager) Viewers(path string) int {
	m.mu.RLock()
	hs := m.streams[path]
	m.mu.RUnlock()
	if hs == nil {
		return 0
	}
	return hs.viewers.count()
}

// ViewersByCountry returns the estimated current HLS viewers of a stream
// by country code ("--" when unknown). It is nil when Country is not set.
func (m *Manager) ViewersByCountry(path string) map[string]int {
	if m.Country == nil {
		return nil
	}
	m.mu.RLock()
	hs := m.streams[path]
	m.mu.RUnlock()
	if hs == nil {
		return map[string]int{}
	}
	return hs.viewers.byCountry()
}

// Variants returns the variants available for a stream.
func (m *Manager) Variants(path string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	hs := m.streams[path]
	if hs == nil {
		return nil
	}
	var out []string
	for _, v := range m.cfg.Variants {
		if hs.muxers[v] != nil {
			out = append(out, v)
		}
	}
	return out
}

type countingWriter struct {
	http.ResponseWriter
	n           uint64
	wroteHeader bool
}

func (c *countingWriter) WriteHeader(code int) {
	c.wroteHeader = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.wroteHeader = true
	n, err := c.ResponseWriter.Write(p)
	c.n += uint64(n)
	return n, err
}

// Flush lets LL-HLS responses stream partial data when supported.
func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
