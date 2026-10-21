// Package api exposes the read-only status API under /api/v1/.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/output/hls"
)

// SourceURL is where the source code of this program can be obtained.
// The AGPL requires offering it to network users; it is shown in the API
// and the web UI. Forks should change it to point at their own source.
const SourceURL = "https://github.com/cvete/streamble"

// Version is set at build time with -ldflags "-X .../api.Version=v0.1.0".
var Version = "dev"

const sampleInterval = 2 * time.Second

// API serves /api/v1/.
type API struct {
	Hub      *core.Hub
	HLS      *hls.Manager
	Token    string
	Primary  string
	started  time.Time
	rates    *rateSampler
	stopOnce sync.Once
	stop     chan struct{}
}

// New creates the API and starts its bitrate sampler.
func New(hub *core.Hub, h *hls.Manager, token, primary string) *API {
	a := &API{
		Hub:     hub,
		HLS:     h,
		Token:   token,
		Primary: primary,
		started: time.Now(),
		rates:   newRateSampler(),
		stop:    make(chan struct{}),
	}
	go a.sampleLoop()
	return a
}

// Close stops the sampler.
func (a *API) Close() { a.stopOnce.Do(func() { close(a.stop) }) }

func (a *API) sampleLoop() {
	t := time.NewTicker(sampleInterval)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			a.rates.sample(a.Hub.Streams())
		}
	}
}

// TrackInfo describes one track.
type TrackInfo struct {
	Codec string `json:"codec"`
	Type  string `json:"type"`
}

// StreamInfo is the API representation of a live stream.
type StreamInfo struct {
	Path           string      `json:"path"`
	Protocol       string      `json:"protocol"`
	RemoteAddr     string      `json:"remote_addr"`
	StartedAt      time.Time   `json:"started_at"`
	UptimeSeconds  int64       `json:"uptime_seconds"`
	Tracks         []TrackInfo `json:"tracks"`
	BytesIn        uint64      `json:"bytes_in"`
	BytesOut       uint64      `json:"bytes_out"`
	BitrateInKbps  int64       `json:"bitrate_in_kbps"`
	BitrateOutKbps int64       `json:"bitrate_out_kbps"`
	Viewers        int         `json:"viewers"`
	HLS            HLSURLs     `json:"hls"`
}

// HLSURLs lists playback URLs relative to the server root.
type HLSURLs struct {
	Primary  string            `json:"primary"`
	Variants map[string]string `json:"variants"`
}

// ServeHTTP routes API requests.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	route := strings.TrimPrefix(r.URL.Path, "/api/v1")

	// Health is always public so load balancers can probe it.
	if route == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	if !a.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="streamble"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	switch {
	case route == "/version":
		writeJSON(w, http.StatusOK, map[string]any{
			"name":           "Streamble",
			"version":        Version,
			"license":        "AGPL-3.0-only",
			"source":         SourceURL,
			"uptime_seconds": int64(time.Since(a.started).Seconds()),
		})

	case route == "/streams":
		streams := a.Hub.Streams()
		out := make([]StreamInfo, 0, len(streams))
		for _, s := range streams {
			out = append(out, a.info(s))
		}
		writeJSON(w, http.StatusOK, map[string]any{"streams": out})

	case strings.HasPrefix(route, "/streams/"):
		s := a.Hub.Lookup(strings.TrimPrefix(route, "/streams/"))
		if s == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "stream not found"})
			return
		}
		writeJSON(w, http.StatusOK, a.info(s))

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (a *API) authorized(r *http.Request) bool {
	if a.Token == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) == 1
}

func (a *API) info(s *core.Stream) StreamInfo {
	in, out := a.rates.get(s)
	info := StreamInfo{
		Path:           s.Path,
		Protocol:       s.Source.Protocol,
		RemoteAddr:     s.Source.RemoteAddr,
		StartedAt:      s.StartedAt.UTC(),
		UptimeSeconds:  int64(time.Since(s.StartedAt).Seconds()),
		BytesIn:        s.BytesIn(),
		BytesOut:       s.BytesOut.Load(),
		BitrateInKbps:  in,
		BitrateOutKbps: out,
		Viewers:        a.HLS.Viewers(s.Path),
		HLS: HLSURLs{
			Primary:  "/" + s.Path + "/index.m3u8",
			Variants: map[string]string{},
		},
	}
	for _, t := range s.Tracks {
		typ := "audio"
		if t.Codec.IsVideo() {
			typ = "video"
		}
		info.Tracks = append(info.Tracks, TrackInfo{Codec: t.Codec.Name(), Type: typ})
	}
	for _, v := range a.HLS.Variants(s.Path) {
		info.HLS.Variants[v] = "/" + s.Path + "/" + v + "/index.m3u8"
	}
	return info
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v) //nolint:errcheck
}

// rateSampler computes per-stream bitrates from periodic counter samples.
type rateSampler struct {
	mu   sync.Mutex
	prev map[*core.Stream]sample
	rate map[*core.Stream][2]int64
}

type sample struct {
	at      time.Time
	in, out uint64
}

func newRateSampler() *rateSampler {
	return &rateSampler{prev: map[*core.Stream]sample{}, rate: map[*core.Stream][2]int64{}}
}

func (r *rateSampler) sample(streams []*core.Stream) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	live := make(map[*core.Stream]bool, len(streams))
	for _, s := range streams {
		live[s] = true
		cur := sample{at: now, in: s.BytesIn(), out: s.BytesOut.Load()}
		if p, ok := r.prev[s]; ok {
			secs := now.Sub(p.at).Seconds()
			if secs > 0 {
				r.rate[s] = [2]int64{
					int64(float64(cur.in-p.in) * 8 / 1000 / secs),
					int64(float64(cur.out-p.out) * 8 / 1000 / secs),
				}
			}
		}
		r.prev[s] = cur
	}
	for s := range r.prev {
		if !live[s] {
			delete(r.prev, s)
			delete(r.rate, s)
		}
	}
}

func (r *rateSampler) get(s *core.Stream) (int64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.rate[s]
	return v[0], v[1]
}
