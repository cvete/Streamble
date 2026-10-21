// Package api serves the JSON API under /api/v1/ used by the control panel
// and by scripts. See docs/API.md for the reference.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/settings"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/users"
)

// SourceURL is where the source code of this program can be obtained.
// The AGPL requires offering it to network users; it is shown in the API
// and the web UI. Forks should change it to point at their own source.
const SourceURL = "https://github.com/cvete/streamble"

// Version is set at build time with -ldflags "-X .../api.Version=v0.1.0".
var Version = "dev"

const sampleInterval = 2 * time.Second

// Options wires the API to the rest of the server.
type Options struct {
	Hub      *core.Hub
	HLS      *hls.Manager
	Users    *users.Service
	Settings *settings.Store
	DB       *store.DB
	Bus      *events.Bus
	Log      *slog.Logger
	// Listeners adds non-HLS clients (HTTP .ts/.mp3/.aac) to viewer counts.
	Listeners func(path string) int
	// Kickers can restart work that is not a publisher, e.g. a transcode job.
	Restart map[string]func(path string) bool
}

// HandlerFunc is an authenticated handler. p is nil for public routes.
type HandlerFunc func(w http.ResponseWriter, r *http.Request, p *users.Principal)

// API serves /api/v1/.
type API struct {
	Options
	mux      *http.ServeMux
	started  time.Time
	rates    *rateSampler
	stopOnce sync.Once
	stop     chan struct{}

	statusMu sync.RWMutex
	statuses map[string]func() any
}

// New creates the API and starts its bitrate sampler.
func New(o Options) *API {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	a := &API{
		Options: o,
		mux:     http.NewServeMux(),
		started: time.Now(),
		rates:   newRateSampler(),
		stop:    make(chan struct{}),
	}
	a.routes()
	go a.sampleLoop()
	return a
}

// Close stops background work.
func (a *API) Close() { a.stopOnce.Do(func() { close(a.stop) }) }

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	a.mux.ServeHTTP(w, r)
}

// Route registers a handler for a Go 1.22 pattern such as
// "GET /api/v1/stats" with a minimum role ("" for public routes).
// Packages that own features (stats, webhooks) add their routes this way.
func (a *API) Route(pattern, role string, h HandlerFunc) {
	a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if role == "" {
			h(w, r, nil)
			return
		}
		p, err := a.Users.Authenticate(r)
		if err != nil {
			Error(w, http.StatusUnauthorized, err)
			return
		}
		if err := users.CheckCSRF(r, p); err != nil {
			Error(w, http.StatusForbidden, err)
			return
		}
		if p.User.MustChangePassword && !allowedBeforePasswordChange(r) {
			Error(w, http.StatusForbidden, errMustChangePassword)
			return
		}
		if !p.Can(role) {
			Error(w, http.StatusForbidden, users.ErrForbidden)
			return
		}
		h(w, r, p)
	})
}

var errMustChangePassword = errors.New("change your password first")

func allowedBeforePasswordChange(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/v1/auth/me", "/api/v1/auth/password", "/api/v1/auth/logout":
		return true
	}
	return false
}

// AddStatus exposes fn's result at GET /api/v1/<name> for viewers, e.g.
// the state of transcode jobs or restream targets.
func (a *API) AddStatus(name string, fn func() any) {
	a.statusMu.Lock()
	if a.statuses == nil {
		a.statuses = make(map[string]func() any)
	}
	a.statuses[name] = fn
	a.statusMu.Unlock()
	a.Route("GET /api/v1/"+name, store.RoleViewer, func(w http.ResponseWriter, _ *http.Request, _ *users.Principal) {
		JSON(w, http.StatusOK, map[string]any{name: fn()})
	})
}

func (a *API) status(name string) any {
	a.statusMu.RLock()
	fn := a.statuses[name]
	a.statusMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

func (a *API) routes() {
	pub, view, op, admin := "", store.RoleViewer, store.RoleOperator, store.RoleAdmin

	a.Route("GET /api/v1/health", pub, func(w http.ResponseWriter, _ *http.Request, _ *users.Principal) {
		JSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	a.Route("GET /api/v1/version", pub, a.version)

	a.Route("POST /api/v1/auth/login", pub, a.login)
	a.Route("POST /api/v1/auth/logout", view, a.logout)
	a.Route("GET /api/v1/auth/me", view, a.me)
	a.Route("POST /api/v1/auth/password", view, a.changePassword)
	a.Route("GET /api/v1/auth/tokens", view, a.listTokens)
	a.Route("POST /api/v1/auth/tokens", view, a.createToken)
	a.Route("DELETE /api/v1/auth/tokens/{id}", view, a.deleteToken)

	a.Route("GET /api/v1/server", view, a.server)
	a.Route("GET /api/v1/streams", view, a.listStreams)
	a.Route("GET /api/v1/streams/{app}/{stream}", view, a.getStream)
	a.Route("POST /api/v1/streams/{app}/{stream}/kick", op, a.kickStream)
	a.Route("GET /api/v1/events", view, a.eventStream)

	a.Route("GET /api/v1/config/apps", view, a.listApps)
	a.Route("POST /api/v1/config/apps", op, a.createApp)
	a.Route("GET /api/v1/config/apps/{name}", view, a.getApp)
	a.Route("PUT /api/v1/config/apps/{name}", op, a.updateApp)
	a.Route("DELETE /api/v1/config/apps/{name}", op, a.deleteApp)

	a.Route("GET /api/v1/config/profiles", view, a.listProfiles)
	a.Route("POST /api/v1/config/profiles", op, a.createProfile)
	a.Route("GET /api/v1/config/profiles/{name}", view, a.getProfile)
	a.Route("PUT /api/v1/config/profiles/{name}", op, a.updateProfile)
	a.Route("DELETE /api/v1/config/profiles/{name}", op, a.deleteProfile)

	a.Route("GET /api/v1/config/sources", view, a.listSources)
	a.Route("POST /api/v1/config/sources", op, a.createSource)
	a.Route("GET /api/v1/config/sources/{app}/{stream}", view, a.getSource)
	a.Route("PUT /api/v1/config/sources/{app}/{stream}", op, a.updateSource)
	a.Route("DELETE /api/v1/config/sources/{app}/{stream}", op, a.deleteSource)

	a.Route("GET /api/v1/config/webhooks", admin, a.listWebhooks)
	a.Route("POST /api/v1/config/webhooks", admin, a.createWebhook)
	a.Route("GET /api/v1/config/webhooks/{name}", admin, a.getWebhook)
	a.Route("PUT /api/v1/config/webhooks/{name}", admin, a.updateWebhook)
	a.Route("DELETE /api/v1/config/webhooks/{name}", admin, a.deleteWebhook)

	a.Route("GET /api/v1/config/playback", view, a.getPlayback)
	a.Route("PUT /api/v1/config/playback", admin, a.updatePlayback)

	a.Route("GET /api/v1/users", admin, a.listUsers)
	a.Route("POST /api/v1/users", admin, a.createUser)
	a.Route("PUT /api/v1/users/{id}", admin, a.updateUser)
	a.Route("DELETE /api/v1/users/{id}", admin, a.deleteUser)

	a.Route("GET /api/v1/audit", admin, a.listAudit)

	a.mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		Error(w, http.StatusNotFound, errors.New("no such API endpoint"))
	})
}

func (a *API) version(w http.ResponseWriter, _ *http.Request, _ *users.Principal) {
	JSON(w, http.StatusOK, map[string]any{
		"name":           "Streamble",
		"version":        Version,
		"license":        "AGPL-3.0-only",
		"source":         SourceURL,
		"uptime_seconds": int64(time.Since(a.started).Seconds()),
	})
}

// JSON writes v with a status code.
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v) //nolint:errcheck
}

// Error writes {"error": "..."}; validation errors list each problem in
// "details".
func Error(w http.ResponseWriter, code int, err error) {
	body := map[string]any{"error": err.Error()}
	if errors.Is(err, settings.ErrInvalid) {
		var details []string
		for _, line := range strings.Split(err.Error(), "\n") {
			line = strings.TrimPrefix(line, settings.ErrInvalid.Error()+": ")
			if line != "" {
				details = append(details, line)
			}
		}
		body["error"] = "the settings are not valid"
		body["details"] = details
	}
	JSON(w, code, body)
}

// ReadJSON decodes a request body (max 1 MB), rejecting unknown fields.
func ReadJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func (a *API) audit(r *http.Request, p *users.Principal, action, target, detail string) {
	if err := a.DB.AddAudit(r.Context(), p.User.Username, action, target, detail); err != nil {
		a.Log.Warn("cannot write audit log", "err", err)
	}
}

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

// Bitrates returns the latest sampled input and output bitrates of a
// stream in kbit/s. Used by the stats recorder.
func (a *API) Bitrates(s *core.Stream) (in, out int64) { return a.rates.get(s) }

// Viewers returns the current number of viewers of a stream (HLS players
// plus HTTP stream listeners).
func (a *API) Viewers(path string) int {
	n := a.HLS.Viewers(path)
	if a.Listeners != nil {
		n += a.Listeners(path)
	}
	return n
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
