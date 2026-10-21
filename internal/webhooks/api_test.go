package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/api"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/settings"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/users"
)

// apiClient is a logged-in browser-like session.
type apiClient struct {
	t    *testing.T
	base string
	http *http.Client
}

func (c *apiClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(users.CSRFHeader, "1")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck
	return resp.StatusCode, out
}

func (c *apiClient) must(code int, method, path string, body any) map[string]any {
	c.t.Helper()
	got, out := c.do(method, path, body)
	if got != code {
		c.t.Fatalf("%s %s: got %d, want %d: %v", method, path, got, code, out)
	}
	return out
}

func login(t *testing.T, base, user, pw, newPW string) *apiClient {
	jar, _ := cookiejar.New(nil)
	c := &apiClient{t: t, base: base, http: &http.Client{Jar: jar}}
	c.must(200, "POST", "/api/v1/auth/login", map[string]string{"username": user, "password": pw})
	c.must(200, "POST", "/api/v1/auth/password", map[string]string{"current_password": pw, "new_password": newPW})
	return c
}

func TestAPI(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *store.DB) {
		log := quietLog()
		base := config.Default()
		live, err := settings.Open(context.Background(), base, db, log)
		if err != nil {
			t.Fatal(err)
		}
		hub := core.NewHub(log)
		hm := hls.NewManager(base.HLS, "*", log)
		accounts := &users.Service{DB: db, TTL: time.Hour, Log: log}
		t.Setenv("STREAMBLE_ADMIN_PASSWORD", "first-admin-password")
		if err := accounts.EnsureAdmin(context.Background()); err != nil {
			t.Fatal(err)
		}
		bus := events.NewBus()
		a := api.New(api.Options{Hub: hub, HLS: hm, Users: accounts, Settings: live, DB: db, Bus: bus, Log: log})
		t.Cleanup(a.Close)
		d := &Dispatcher{Bus: bus, Settings: live.Getter(), DB: db, Log: log,
			RetryDelays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
		d.Start()
		t.Cleanup(d.Close)
		Register(a, d)
		srv := httptest.NewServer(a)
		t.Cleanup(srv.Close)

		rc := newReceiver(t, nil)
		admin := login(t, srv.URL, "admin", "first-admin-password", "new-admin-password")
		admin.must(201, "POST", "/api/v1/config/webhooks", map[string]any{
			"name": "ops", "url": rc.URL, "secret": "shh-secret", "events": []string{"stream.ended"}})

		// Test endpoint.
		del := admin.must(200, "POST", "/api/v1/config/webhooks/ops/test", nil)
		if del["status"] != float64(200) || del["event"] != "ping" || del["attempts"] != float64(1) ||
			del["id"] == "" || del["error"] != "" {
			t.Fatalf("test delivery: %v", del)
		}
		for _, k := range []string{"id", "event", "at", "status", "attempts", "error", "duration_ms"} {
			if _, ok := del[k]; !ok {
				t.Fatalf("delivery lacks %q: %v", k, del)
			}
		}
		if h := rc.all()[0]; !verify("shh-secret", h.body, h.header.Get("X-Streamble-Signature")) {
			t.Fatal("test delivery not signed")
		}
		admin.must(404, "POST", "/api/v1/config/webhooks/nope/test", nil)

		// A bus event, through the live settings created by the API.
		bus.Publish(events.Event{Type: config.EventStreamEnded, Data: map[string]any{"path": "live/tv1"}})
		var list []any
		waitFor(t, "two deliveries", func() bool {
			out := admin.must(200, "GET", "/api/v1/config/webhooks/ops/deliveries", nil)
			list, _ = out["deliveries"].([]any)
			return len(list) == 2
		})
		first := list[0].(map[string]any)
		if first["event"] != "stream.ended" || list[1].(map[string]any)["id"] != del["id"] {
			t.Fatalf("deliveries not newest first: %v", list)
		}
		admin.must(404, "GET", "/api/v1/config/webhooks/nope/deliveries", nil)
		admin.must(201, "POST", "/api/v1/config/webhooks", map[string]any{"name": "idle", "url": rc.URL})
		if out := admin.must(200, "GET", "/api/v1/config/webhooks/idle/deliveries", nil); out["deliveries"] == nil {
			t.Fatalf("empty list must be [], got %v", out)
		}

		// Deleting the webhook via the API stops its deliveries.
		admin.must(200, "DELETE", "/api/v1/config/webhooks/ops", nil)
		bus.Publish(events.Event{Type: config.EventStreamEnded})
		waitFor(t, "worker removal", func() bool {
			for _, w := range d.Workers() {
				if w == "ops" {
					return false
				}
			}
			return true
		})

		// Admin only; anonymous callers are refused.
		admin.must(201, "POST", "/api/v1/users", map[string]string{"username": "op", "password": "initial-password", "role": "operator"})
		op := login(t, srv.URL, "op", "initial-password", "operator-password")
		op.must(403, "GET", "/api/v1/config/webhooks/idle/deliveries", nil)
		op.must(403, "POST", "/api/v1/config/webhooks/idle/test", nil)
		anon := &apiClient{t: t, base: srv.URL, http: http.DefaultClient}
		anon.must(401, "POST", "/api/v1/config/webhooks/idle/test", nil)
	})
}
