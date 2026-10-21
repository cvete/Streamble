// Package httpstream serves live streams as continuous HTTP responses:
//
//	/<app>/<stream>.ts    MPEG-TS with every track of the stream
//	/<app>/<stream>.mp3   raw MP3 frames (Icecast-style radio)
//	/<app>/<stream>.aac   AAC in ADTS (Icecast-style radio)
//
// Each client gets its own hub subscription, so a slow listener drops
// units (and resumes at the next keyframe) without affecting anyone else.
package httpstream

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/tsconv"
)

// writeTimeout drops clients that cannot take a unit within this time.
const writeTimeout = 10 * time.Second

// Output formats, by URL extension.
const (
	extTS  = ".ts"
	extMP3 = ".mp3"
	extAAC = ".aac"
)

// Handler serves continuous HTTP streams. Route requests to it when Match
// returns true.
type Handler struct {
	Hub      *core.Hub
	Auth     *auth.Authorizer
	Playback config.Playback
	// PlaybackFunc, when set, replaces Playback so toggles apply live.
	PlaybackFunc func() config.Playback
	AllowOrigin  string
	Log          *slog.Logger
	// Country, when set, maps a client IP address to an ISO country code
	// (or "--"). It is called once per connection; see ListenersByCountry.
	Country func(ip string) string

	mu        sync.Mutex
	listeners map[string]int
	countries map[string]map[string]int // path -> country -> listeners
}

// split returns the stream path ("app/stream") and extension of a
// continuous stream URL, or ok=false.
func split(urlPath string) (path, ext string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(urlPath, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		return "", "", false
	}
	for _, e := range []string{extTS, extMP3, extAAC} {
		if name, found := strings.CutSuffix(parts[1], e); found && name != "" {
			return parts[0] + "/" + name, e, true
		}
	}
	return "", "", false
}

// Match reports whether urlPath is a continuous stream URL:
// /<app>/<stream>.ts, .mp3 or .aac.
func (h *Handler) Match(urlPath string) bool {
	_, _, ok := split(urlPath)
	return ok
}

// Listeners returns the number of HTTP stream clients of "<app>/<stream>".
func (h *Handler) Listeners(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listeners[path]
}

// ListenersByCountry returns the HTTP stream clients of "<app>/<stream>"
// by country code ("--" when unknown). It is nil when Country is not set.
func (h *Handler) ListenersByCountry(path string) map[string]int {
	if h.Country == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int, len(h.countries[path]))
	for c, n := range h.countries[path] {
		out[c] = n
	}
	return out
}

// listenerCountry looks up the country of a client, or "" when Country is
// not set. X-Forwarded-For is not trusted (that needs a proxy setting).
func (h *Handler) listenerCountry(r *http.Request) string {
	if h.Country == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if c := h.Country(host); c != "" {
		return c
	}
	return "--"
}

func (h *Handler) addListener(path, country string, d int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listeners == nil {
		h.listeners = make(map[string]int)
	}
	h.listeners[path] += d
	if h.listeners[path] <= 0 {
		delete(h.listeners, path)
	}
	if country == "" {
		return
	}
	if h.countries == nil {
		h.countries = make(map[string]map[string]int)
	}
	m := h.countries[path]
	if m == nil {
		m = make(map[string]int)
		h.countries[path] = m
	}
	m[country] += d
	if m[country] <= 0 {
		delete(m, country)
	}
	if len(m) == 0 {
		delete(h.countries, path)
	}
}

func (h *Handler) log() *slog.Logger {
	if h.Log == nil {
		return slog.Default()
	}
	return h.Log
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.AllowOrigin != "" {
		hd := w.Header()
		hd.Set("Access-Control-Allow-Origin", h.AllowOrigin)
		hd.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
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

	reqPath, ext, ok := split(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pb := h.Playback
	if h.PlaybackFunc != nil {
		pb = h.PlaybackFunc()
	}
	if (ext == extTS && !pb.HTTPTS) || (ext != extTS && !pb.HTTPAudio) {
		http.NotFound(w, r)
		return
	}

	key := r.URL.Query().Get("key")
	path, _, err := h.Auth.Play(r.Context(), auth.Request{
		Path: reqPath, Key: key, Protocol: "http", RemoteAddr: r.RemoteAddr,
	})
	switch {
	case errors.Is(err, auth.ErrDenied):
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case errors.Is(err, auth.ErrUnauthorized) && key == "":
		http.Error(w, "play key required", http.StatusUnauthorized)
		return
	case errors.Is(err, auth.ErrUnauthorized):
		h.log().Warn("HTTP stream rejected: invalid play key", "path", reqPath, "remote", r.RemoteAddr)
		http.Error(w, "invalid play key", http.StatusForbidden)
		return
	case err != nil:
		http.NotFound(w, r)
		return
	}

	stream := h.Hub.Lookup(path)
	if stream == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}

	var (
		contentType string
		track       *core.Track
		adts        *mpeg4audio.ADTSPacket
	)
	switch ext {
	case extTS:
		contentType = "video/mp2t"
	case extMP3:
		contentType = "audio/mpeg"
		track = findTrack(stream, func(c core.Codec) bool { _, ok := c.(*core.MPEGAudio); return ok })
		if track == nil {
			http.Error(w, "stream has no MP3 audio; a transcode rendition with audio_codec: mp3 can provide one",
				http.StatusNotFound)
			return
		}
	case extAAC:
		contentType = "audio/aac"
		track = findTrack(stream, func(c core.Codec) bool {
			a, ok := c.(*core.AAC)
			return ok && a.Config != nil
		})
		if track == nil {
			http.Error(w, "stream has no AAC audio; a transcode rendition with audio_codec: aac can provide one",
				http.StatusNotFound)
			return
		}
		adts = adtsTemplate(track.Codec.(*core.AAC).Config)
		if _, err := wrapADTS(adts, nil); err != nil {
			http.Error(w, "AAC configuration cannot be carried in ADTS", http.StatusNotFound)
			return
		}
	}

	hd := w.Header()
	hd.Set("Content-Type", contentType)
	hd.Set("Cache-Control", "no-cache, no-store")
	if ext != extTS {
		hd.Set("icy-name", path)
		hd.Set("icy-pub", "0")
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	sub := stream.Subscribe("http"+ext, 0)
	if sub == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	defer sub.Close()
	country := h.listenerCountry(r)
	h.addListener(path, country, 1)
	defer h.addListener(path, country, -1)

	rc := http.NewResponseController(w)
	cw := &countingWriter{w: w, stream: stream}
	w.WriteHeader(http.StatusOK)
	rc.Flush() //nolint:errcheck

	var write func(u *core.Unit) error
	switch ext {
	case extTS:
		mux, err := tsconv.NewMuxer(cw, stream.Tracks)
		if err != nil {
			return
		}
		write = mux.WriteUnit
	case extMP3:
		write = func(u *core.Unit) error {
			if u.Track != track {
				return nil
			}
			for _, f := range u.Payload {
				if _, err := cw.Write(f); err != nil {
					return err
				}
			}
			return nil
		}
	case extAAC:
		var buf []byte
		write = func(u *core.Unit) error {
			if u.Track != track {
				return nil
			}
			for _, au := range u.Payload {
				var err error
				if buf, err = wrapADTS(adts, au); err != nil {
					continue // oversized or empty access unit: skip it
				}
				if _, err := cw.Write(buf); err != nil {
					return err
				}
			}
			return nil
		}
	}

	h.log().Debug("HTTP stream client connected", "path", path, "format", ext, "remote", r.RemoteAddr)
	err = h.pump(r, rc, sub, write)
	h.log().Debug("HTTP stream client disconnected", "path", path, "format", ext,
		"remote", r.RemoteAddr, "err", err)
}

// pump writes units until the client leaves, the stream ends or a write
// fails.
func (h *Handler) pump(r *http.Request, rc *http.ResponseController, sub *core.Subscription,
	write func(*core.Unit) error,
) error {
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case u, ok := <-sub.C():
			if !ok {
				return io.EOF
			}
			rc.SetWriteDeadline(time.Now().Add(writeTimeout)) //nolint:errcheck
			if err := write(u); err != nil {
				return err
			}
			if err := rc.Flush(); err != nil {
				return err
			}
		}
	}
}

func findTrack(s *core.Stream, match func(core.Codec) bool) *core.Track {
	for _, t := range s.Tracks {
		if match(t.Codec) {
			return t
		}
	}
	return nil
}

// adtsTemplate builds the ADTS header fields for an AAC configuration.
// For HE-AAC the config holds the core (AAC-LC) type and sample rate,
// which is what ADTS signals (SBR/PS are implicit).
func adtsTemplate(cfg *mpeg4audio.AudioSpecificConfig) *mpeg4audio.ADTSPacket {
	return &mpeg4audio.ADTSPacket{
		Type:          cfg.Type,
		SampleRate:    cfg.SampleRate,
		ChannelConfig: cfg.ChannelConfig,
		ChannelCount:  cfg.ChannelCount, //nolint:staticcheck
	}
}

// wrapADTS returns au with an ADTS header taken from tmpl.
func wrapADTS(tmpl *mpeg4audio.ADTSPacket, au []byte) ([]byte, error) {
	if len(au) > mpeg4audio.MaxAccessUnitSize {
		return nil, errors.New("access unit too big")
	}
	pkt := *tmpl
	pkt.AU = au
	return mpeg4audio.ADTSPackets{&pkt}.Marshal()
}

// countingWriter adds written bytes to the stream's outgoing counter.
type countingWriter struct {
	w      io.Writer
	stream *core.Stream
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.stream.BytesOut.Add(uint64(n))
	return n, err
}
