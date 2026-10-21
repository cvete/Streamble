package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/settings"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/users"
)

type testEnv struct {
	srv   *httptest.Server
	hub   *core.Hub
	db    *store.DB
	live  *settings.Store
	authz *auth.Authorizer
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	base := config.Default()
	base.Transcode.Profiles = []config.Profile{{Name: "tv", Renditions: []config.Rendition{{Name: "720p", Height: 720}}}}
	live, err := settings.Open(context.Background(), base, db, log)
	if err != nil {
		t.Fatal(err)
	}
	hub := core.NewHub(log)
	hm := hls.NewManager(base.HLS, "*", log)
	hub.AddHook(hm)
	accounts := &users.Service{DB: db, TTL: time.Hour, Log: log}
	t.Setenv("STREAMBLE_ADMIN_PASSWORD", "first-admin-password")
	if err := accounts.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := New(Options{Hub: hub, HLS: hm, Users: accounts, Settings: live, DB: db, Bus: events.NewBus(), Log: log})
	t.Cleanup(a.Close)
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, hub: hub, db: db, live: live, authz: auth.NewDynamic(live.Getter())}
}

// client is a browser-like session (cookie jar + CSRF header).
type client struct {
	t     *testing.T
	base  string
	http  *http.Client
	token string // bearer token instead of cookies when set
	csrf  bool
}

func (e *testEnv) client(t *testing.T) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: e.srv.URL, http: &http.Client{Jar: jar}, csrf: true}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	if c.csrf {
		req.Header.Set(users.CSRFHeader, "1")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck
	return resp.StatusCode, out
}

func (c *client) must(code int, method, path string, body any) map[string]any {
	c.t.Helper()
	got, out := c.do(method, path, body)
	if got != code {
		c.t.Fatalf("%s %s: got %d, want %d: %v", method, path, got, code, out)
	}
	return out
}

// adminClient logs in as the first admin and completes the forced
// password change.
func (e *testEnv) adminClient(t *testing.T) *client {
	c := e.client(t)
	c.must(200, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "first-admin-password"})
	c.must(403, "GET", "/api/v1/streams", nil) // must change password first
	c.must(200, "POST", "/api/v1/auth/password", map[string]string{
		"current_password": "first-admin-password", "new_password": "new-admin-password"})
	return c
}

func (e *testEnv) userClient(t *testing.T, admin *client, name, role string) *client {
	admin.must(201, "POST", "/api/v1/users", map[string]string{"username": name, "password": "initial-password", "role": role})
	c := e.client(t)
	c.must(200, "POST", "/api/v1/auth/login", map[string]string{"username": name, "password": "initial-password"})
	c.must(200, "POST", "/api/v1/auth/password", map[string]string{
		"current_password": "initial-password", "new_password": name + "-real-password"})
	return c
}

func TestPublicEndpointsAndLogin(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	c.must(200, "GET", "/api/v1/health", nil)
	if v := c.must(200, "GET", "/api/v1/version", nil); v["license"] != "AGPL-3.0-only" {
		t.Fatalf("version: %v", v)
	}
	c.must(401, "GET", "/api/v1/streams", nil)
	c.must(401, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong-password"})
	c.must(400, "POST", "/api/v1/auth/login", map[string]any{"user": "x"}) // unknown field
	c.must(404, "GET", "/api/v1/nope", nil)

	admin := e.adminClient(t)
	me := admin.must(200, "GET", "/api/v1/auth/me", nil)
	if me["user"].(map[string]any)["must_change_password"] != false {
		t.Fatalf("me: %v", me)
	}
	admin.must(200, "POST", "/api/v1/auth/logout", nil)
	admin.must(401, "GET", "/api/v1/streams", nil)
}

func TestCSRFHeaderRequiredForCookieWrites(t *testing.T) {
	e := newEnv(t)
	admin := e.adminClient(t)
	admin.csrf = false
	admin.must(200, "GET", "/api/v1/config/apps", nil) // reads are fine
	admin.must(403, "POST", "/api/v1/config/apps", map[string]string{"name": "x"})
}

func TestRolesAndSecretMasking(t *testing.T) {
	e := newEnv(t)
	admin := e.adminClient(t)
	admin.must(201, "POST", "/api/v1/config/apps", map[string]any{
		"name": "tv", "publish_key": "secret-key", "stream_keys": map[string]string{"tv1": "tv1-key"},
		"push": []map[string]any{{"name": "yt", "url": "rtmp://a.rtmp.youtube.com/live2/YTKEY"}},
	})

	viewer := e.userClient(t, admin, "watcher", store.RoleViewer)
	app := viewer.must(200, "GET", "/api/v1/config/apps/tv", nil)["app"].(map[string]any)
	if app["publish_key"] != Masked || app["stream_keys"].(map[string]any)["tv1"] != Masked {
		t.Fatalf("viewer saw keys: %v", app)
	}
	if u := app["push"].([]any)[0].(map[string]any)["url"].(string); strings.Contains(u, "YTKEY") {
		t.Fatalf("viewer saw the stream key in %s", u)
	}
	viewer.must(403, "POST", "/api/v1/config/apps", map[string]string{"name": "x"})
	viewer.must(403, "GET", "/api/v1/users", nil)

	op := e.userClient(t, admin, "operator1", store.RoleOperator)
	if k := op.must(200, "GET", "/api/v1/config/apps/tv", nil)["app"].(map[string]any)["publish_key"]; k != "secret-key" {
		t.Fatalf("operator must see keys to configure encoders, got %v", k)
	}
	op.must(201, "POST", "/api/v1/config/apps", map[string]string{"name": "radio"})
	op.must(403, "GET", "/api/v1/config/webhooks", nil)
	op.must(403, "PUT", "/api/v1/config/playback", map[string]bool{"rtmp": false})
}

func TestAppLifecycleAppliesLive(t *testing.T) {
	e := newEnv(t)
	admin := e.adminClient(t)

	if _, _, err := e.authz.AuthorizePublish("news/a", ""); err == nil {
		t.Fatal("unknown app accepted")
	}
	admin.must(201, "POST", "/api/v1/config/apps", map[string]any{"name": "news", "publish_key": "k1", "transcode": "tv"})
	if _, _, err := e.authz.AuthorizePublish("news/a", "k1"); err != nil {
		t.Fatalf("new app not live: %v", err)
	}
	admin.must(409, "POST", "/api/v1/config/apps", map[string]string{"name": "news"})

	// Validation errors come back as a list.
	_, out := admin.do("POST", "/api/v1/config/apps", map[string]string{"name": "panel", "transcode": "nope"})
	if d, _ := out["details"].([]any); len(d) < 2 {
		t.Fatalf("expected two validation details, got %v", out)
	}

	// Sending back masked secrets keeps them.
	admin.must(200, "PUT", "/api/v1/config/apps/news", map[string]any{"publish_key": Masked, "transcode": "tv", "description": "News desk"})
	if _, _, err := e.authz.AuthorizePublish("news/a", "k1"); err != nil {
		t.Fatal("masked key overwrote the real one")
	}
	admin.must(400, "PUT", "/api/v1/config/apps/news", map[string]any{"name": "renamed"})
	admin.must(404, "PUT", "/api/v1/config/apps/ghost", map[string]any{})

	// A profile in use cannot be deleted.
	admin.must(400, "DELETE", "/api/v1/config/profiles/tv", nil)
	admin.must(200, "DELETE", "/api/v1/config/apps/news", nil)
	admin.must(200, "DELETE", "/api/v1/config/profiles/tv", nil)

	// Sources need an existing app.
	admin.must(400, "POST", "/api/v1/config/sources", map[string]string{"path": "ghost/x", "url": "udp://@:5000"})
	admin.must(201, "POST", "/api/v1/config/sources", map[string]string{"path": "live/relay", "url": "udp://@:5000"})
	admin.must(200, "PUT", "/api/v1/config/sources/live/relay", map[string]any{"url": "udp://@:5001", "disabled": true})
	if s := e.live.Get().Source("live/relay"); s == nil || !s.Disabled || s.URL != "udp://@:5001" {
		t.Fatalf("source not updated: %+v", s)
	}

	admin.must(200, "PUT", "/api/v1/config/playback", map[string]bool{"http_ts": true, "rtmp": false})
	if e.live.Get().Playback.RTMP {
		t.Fatal("playback not applied")
	}

	admin.must(201, "POST", "/api/v1/config/webhooks", map[string]any{"name": "ops", "url": "https://example.com/h", "events": []string{"stream.ended"}})
	admin.must(400, "POST", "/api/v1/config/webhooks", map[string]any{"name": "bad", "url": "https://x", "events": []string{"nope"}})

	entries := admin.must(200, "GET", "/api/v1/audit", nil)["entries"].([]any)
	if len(entries) < 8 || entries[0].(map[string]any)["action"] != "webhook.create" {
		t.Fatalf("audit log: %v", entries[:min(len(entries), 2)])
	}
}

func TestUserManagementSafeguards(t *testing.T) {
	e := newEnv(t)
	admin := e.adminClient(t)
	me := admin.must(200, "GET", "/api/v1/auth/me", nil)["user"].(map[string]any)
	adminID := me["id"].(string)

	admin.must(400, "POST", "/api/v1/users", map[string]string{"username": "bob", "password": "short", "role": "viewer"})
	admin.must(400, "POST", "/api/v1/users", map[string]string{"username": "bob", "password": "long-enough-pw", "role": "god"})
	bob := e.userClient(t, admin, "bob", store.RoleViewer)
	admin.must(409, "POST", "/api/v1/users", map[string]string{"username": "BOB", "password": "long-enough-pw", "role": "viewer"})

	admin.must(400, "DELETE", "/api/v1/users/"+adminID, nil)
	admin.must(400, "PUT", "/api/v1/users/"+adminID, map[string]any{"role": "viewer"})

	// Disabling bob signs him out immediately.
	users := admin.must(200, "GET", "/api/v1/users", nil)["users"].([]any)
	var bobID string
	for _, u := range users {
		if u.(map[string]any)["username"] == "bob" {
			bobID = u.(map[string]any)["id"].(string)
		}
	}
	bob.must(200, "GET", "/api/v1/streams", nil)
	admin.must(200, "PUT", "/api/v1/users/"+bobID, map[string]any{"disabled": true})
	bob.must(401, "GET", "/api/v1/streams", nil)

	// Promote bob, then the original admin may step down.
	admin.must(200, "PUT", "/api/v1/users/"+bobID, map[string]any{"disabled": false, "role": "admin"})
	admin.must(200, "DELETE", "/api/v1/users/"+bobID, nil)
}

func TestAPITokens(t *testing.T) {
	e := newEnv(t)
	admin := e.adminClient(t)
	out := admin.must(201, "POST", "/api/v1/auth/tokens", map[string]string{"name": "grafana"})
	token := out["token"].(string)
	id := out["info"].(map[string]any)["id"].(string)

	bot := &client{t: t, base: e.srv.URL, http: http.DefaultClient, token: token}
	bot.must(200, "GET", "/api/v1/streams", nil)
	bot.must(201, "POST", "/api/v1/config/apps", map[string]string{"name": "bots"}) // no CSRF header needed

	toks := admin.must(200, "GET", "/api/v1/auth/tokens", nil)["tokens"].([]any)
	if len(toks) != 1 || strings.Contains(toks[0].(map[string]any)["prefix"].(string), token[12:]) {
		t.Fatalf("tokens: %v", toks)
	}
	admin.must(200, "DELETE", "/api/v1/auth/tokens/"+id, nil)
	bot.must(401, "GET", "/api/v1/streams", nil)
}

func TestStreamsKickAndEvents(t *testing.T) {
	e := newEnv(t)
	admin := e.adminClient(t)

	kicked := make(chan struct{}, 1)
	_, err := e.hub.Publish("live/cam", []*core.Track{core.NewTrack(&core.H264{})},
		core.SourceInfo{Protocol: "rtmp", RemoteAddr: "1.2.3.4:5", Kick: func() { kicked <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	e.hub.Publish("live/r1", []*core.Track{core.NewTrack(&core.H264{})}, core.SourceInfo{Protocol: "transcode"}) //nolint:errcheck

	list := admin.must(200, "GET", "/api/v1/streams", nil)["streams"].([]any)
	if len(list) != 2 {
		t.Fatalf("streams: %v", list)
	}
	s := admin.must(200, "GET", "/api/v1/streams/live/cam", nil)
	if s["kickable"] != true || s["http"].(map[string]any)["ts"] != "/live/cam.ts" {
		t.Fatalf("stream info: %v", s)
	}

	viewer := e.userClient(t, admin, "v", store.RoleViewer)
	viewer.must(403, "POST", "/api/v1/streams/live/cam/kick", nil)
	admin.must(200, "POST", "/api/v1/streams/live/cam/kick", nil)
	select {
	case <-kicked:
	case <-time.After(time.Second):
		t.Fatal("kick not delivered")
	}
	admin.must(409, "POST", "/api/v1/streams/live/r1/kick", nil)
	admin.must(404, "POST", "/api/v1/streams/live/none/kick", nil)

	// The event stream starts with a snapshot.
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := admin.http.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var event string
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") && event == "snapshot" {
			var snap map[string]any
			json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &snap) //nolint:errcheck
			if snap["overview"].(map[string]any)["streams"] != float64(2) {
				t.Fatalf("snapshot: %v", snap["overview"])
			}
			return
		}
	}
	t.Fatal("no snapshot received")
}
