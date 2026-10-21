package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/config"
)

// callbackServer answers with code and records the last request body.
type callbackServer struct {
	*httptest.Server
	mu    sync.Mutex
	last  map[string]any
	calls int
}

func newCallbackServer(t *testing.T, handler func(body map[string]any) int) *callbackServer {
	cs := &callbackServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		cs.mu.Lock()
		cs.last = body
		cs.calls++
		cs.mu.Unlock()
		w.WriteHeader(handler(body))
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *callbackServer) lastBody() (map[string]any, int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.last, cs.calls
}

func authApps(url string) *config.Config {
	cfg := config.Default()
	cfg.Apps = []config.App{
		{Name: "live", PublishKey: "pk", PlayKey: "vk", AuthURL: url},
		{Name: "open", AuthURL: url},
	}
	return cfg
}

func TestCallbackAllowAndDeny(t *testing.T) {
	cs := newCallbackServer(t, func(b map[string]any) int {
		if b["stream"] == "allowed" {
			return http.StatusNoContent
		}
		return http.StatusForbidden
	})
	a := New(authApps(cs.URL))
	ctx := context.Background()

	p, app, err := a.Publish(ctx, Request{Path: "/live/allowed/", Key: "pk", Protocol: "rtmp", RemoteAddr: "203.0.113.7:51234"})
	if err != nil || p != "live/allowed" || app.Name != "live" {
		t.Fatalf("allowed publish: %q %v", p, err)
	}
	body, _ := cs.lastBody()
	want := map[string]any{"action": "publish", "app": "live", "stream": "allowed", "path": "live/allowed",
		"key": "pk", "protocol": "rtmp", "remote_addr": "203.0.113.7:51234"}
	if len(body) != len(want) {
		t.Fatalf("callback body %v, want %v", body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Fatalf("callback body %s=%v, want %v (%v)", k, body[k], v, body)
		}
	}

	if _, _, err := a.Publish(ctx, Request{Path: "live/other", Key: "pk", Protocol: "srt"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("403 must deny, got %v", err)
	}

	if _, _, err := a.Play(ctx, Request{Path: "live/allowed", Key: "vk", Protocol: "http", RemoteAddr: "1.2.3.4:5"}); err != nil {
		t.Fatalf("allowed play: %v", err)
	}
	body, _ = cs.lastBody()
	if body["action"] != "play" || body["protocol"] != "http" || body["key"] != "vk" {
		t.Fatalf("play body: %v", body)
	}
	if _, _, err := a.Play(ctx, Request{Path: "live/x", Key: "vk"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied play: %v", err)
	}

	// The legacy wrappers go through the callback too.
	if _, _, err := a.AuthorizePublish("open/allowed", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AuthorizePlay("open/nope", ""); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestCallbackKeyChecksFirst(t *testing.T) {
	cs := newCallbackServer(t, func(map[string]any) int { return http.StatusOK })
	a := New(authApps(cs.URL))
	ctx := context.Background()
	if _, _, err := a.Publish(ctx, Request{Path: "live/x", Key: "wrong"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, _, err := a.Play(ctx, Request{Path: "live/x", Key: "pk"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong play key: %v", err)
	}
	if _, _, err := a.Publish(ctx, Request{Path: "nope/x"}); !errors.Is(err, ErrUnknownApp) {
		t.Fatalf("unknown app: %v", err)
	}
	if _, _, err := a.Publish(ctx, Request{Path: "live/../x", Key: "pk"}); !errors.Is(err, ErrBadPath) {
		t.Fatalf("bad path: %v", err)
	}
	if _, calls := cs.lastBody(); calls != 0 {
		t.Fatalf("callback called %d times for requests that failed key checks", calls)
	}
}

func TestCallbackTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	a := New(authApps(srv.URL))
	start := time.Now()
	_, _, err := a.Publish(context.Background(), Request{Path: "open/x"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("timeout must deny, got %v", err)
	}
	if d := time.Since(start); d < CallbackTimeout-100*time.Millisecond || d > CallbackTimeout+2*time.Second {
		t.Fatalf("timed out after %v, want about %v", d, CallbackTimeout)
	}

	// A caller's context shortens the wait.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, _, err := a.Publish(ctx, Request{Path: "open/x"}); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("caller context ignored")
	}
}

func TestCallbackUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	a := New(authApps("http://" + addr + "/auth"))
	if _, _, err := a.Publish(context.Background(), Request{Path: "open/x"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("unreachable must deny, got %v", err)
	}
}

func TestCallbackRedirectAndServerError(t *testing.T) {
	target := newCallbackServer(t, func(map[string]any) int { return http.StatusOK })
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	a := New(authApps(redirect.URL))
	if _, _, err := a.Publish(context.Background(), Request{Path: "open/x"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("redirect must deny, got %v", err)
	}
	if _, calls := target.lastBody(); calls != 0 {
		t.Fatal("redirect was followed")
	}

	failing := newCallbackServer(t, func(map[string]any) int { return http.StatusInternalServerError })
	a = New(authApps(failing.URL))
	if _, _, err := a.Publish(context.Background(), Request{Path: "open/x"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("500 must deny, got %v", err)
	}
}

func TestCallbackHotReload(t *testing.T) {
	cs := newCallbackServer(t, func(map[string]any) int { return http.StatusForbidden })
	cfg := authApps("")
	var mu sync.Mutex
	a := NewDynamic(func() *config.Config { mu.Lock(); defer mu.Unlock(); return cfg })
	if _, _, err := a.Publish(context.Background(), Request{Path: "open/x"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	cfg = authApps(cs.URL)
	mu.Unlock()
	if _, _, err := a.Publish(context.Background(), Request{Path: "open/x"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("new auth_url must apply to the next request, got %v", err)
	}
}
