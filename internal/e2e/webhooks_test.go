package e2e

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/webhooks"
)

const hookSecret = "e2e-webhook-secret"

// hookRecorder collects JSON requests sent to an httptest server.
type hookRecorder struct {
	mu   sync.Mutex
	reqs []hookRequest
}

type hookRequest struct {
	header http.Header
	body   []byte
	json   map[string]any
}

func (r *hookRecorder) add(req *http.Request) hookRequest {
	b, _ := io.ReadAll(req.Body)
	h := hookRequest{header: req.Header.Clone(), body: b}
	json.Unmarshal(b, &h.json) //nolint:errcheck
	r.mu.Lock()
	r.reqs = append(r.reqs, h)
	r.mu.Unlock()
	return h
}

func (r *hookRecorder) all() []hookRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hookRequest(nil), r.reqs...)
}

// hookFFmpeg is an FFmpeg publisher started by hookPublish.
type hookFFmpeg struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when FFmpeg has exited
	err    error         // exit error, valid after done
}

func (f *hookFFmpeg) stop() {
	f.cancel()
	<-f.done
}

// hookPublish starts FFmpeg publishing test video to url until the test
// ends or stop is called.
func hookPublish(t *testing.T, url string) *hookFFmpeg {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	args := join([]string{"-hide_banner", "-loglevel", "error", "-re"}, video264, []string{"-f", "flv", url})
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	f := &hookFFmpeg{cancel: cancel, done: make(chan struct{})}
	go func() {
		f.err = cmd.Wait()
		close(f.done)
	}()
	t.Cleanup(f.stop)
	return f
}

func hookWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWebhooksAndAuthCallback(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Auth callback: allows key "good" only.
	var callbacks hookRecorder
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := callbacks.add(r); c.json["key"] != "good" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(authSrv.Close)

	// Webhook receiver.
	var hooks hookRecorder
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hooks.add(r)
	}))
	t.Cleanup(hookSrv.Close)

	cfg := config.Default()
	cfg.Apps = []config.App{{Name: "guarded", AuthURL: authSrv.URL + "/auth"}}
	cfg.Webhooks = []config.Webhook{{Name: "e2e", URL: hookSrv.URL, Secret: hookSecret,
		Events: []string{config.EventStreamPublished, config.EventStreamEnded}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	hub := core.NewHub(log)
	bus := events.NewBus()
	hub.AddHook(events.StreamHook{Bus: bus})
	d := &webhooks.Dispatcher{Bus: bus, Settings: config.Static(cfg), DB: db, Log: log}
	d.Start()
	t.Cleanup(d.Close)

	rs := &rtmp.Server{Addr: "127.0.0.1:0", Hub: hub, Auth: auth.New(cfg), Log: log}
	if err := rs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)
	base := "rtmp://" + rs.ListenAddr().String() + "/guarded/"

	// Denied key: FFmpeg is refused and no stream appears.
	bad := hookPublish(t, base+"tv?key=bad")
	select {
	case <-bad.done:
		if bad.err == nil {
			t.Fatal("FFmpeg succeeded with a denied key")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("publish with a denied key was not refused")
	}
	if hub.Lookup("guarded/tv") != nil {
		t.Fatal("stream published with a denied key")
	}
	denied := callbacks.all()
	if len(denied) != 1 || denied[0].json["key"] != "bad" || denied[0].json["action"] != "publish" {
		t.Fatalf("callbacks after denied publish: %v", denied)
	}

	// Allowed key.
	good := hookPublish(t, base+"tv?key=good")
	hookWait(t, "stream", func() bool { return hub.Lookup("guarded/tv") != nil })

	all := callbacks.all()
	c := all[len(all)-1].json
	host, port, err := net.SplitHostPort(c["remote_addr"].(string))
	if c["protocol"] != "rtmp" || c["action"] != "publish" || c["app"] != "guarded" || c["stream"] != "tv" ||
		c["path"] != "guarded/tv" || c["key"] != "good" || err != nil || host != "127.0.0.1" || port == "0" {
		t.Fatalf("callback body: %v", c)
	}

	hookWait(t, "stream.published webhook", func() bool { return len(hooks.all()) >= 1 })
	good.stop()
	hookWait(t, "stream end", func() bool { return hub.Lookup("guarded/tv") == nil })
	hookWait(t, "stream.ended webhook", func() bool { return len(hooks.all()) >= 2 })

	got := hooks.all()
	if len(got) != 2 {
		t.Fatalf("got %d webhook deliveries, want 2", len(got))
	}
	for i, want := range []string{config.EventStreamPublished, config.EventStreamEnded} {
		h := got[i]
		mac := hmac.New(sha256.New, []byte(hookSecret))
		mac.Write(h.body)
		sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(sig), []byte(h.header.Get("X-Streamble-Signature"))) {
			t.Fatalf("%s: invalid signature", want)
		}
		if h.json["type"] != want || h.header.Get("X-Streamble-Event") != want ||
			h.header.Get("X-Streamble-Delivery") != h.json["id"] {
			t.Fatalf("delivery %d: %s", i, h.body)
		}
		data, _ := h.json["data"].(map[string]any)
		if data["path"] != "guarded/tv" || data["protocol"] != "rtmp" {
			t.Fatalf("delivery %d data: %v", i, data)
		}
	}
	if data := got[0].json["data"].(map[string]any); !strings.HasPrefix(data["remote_addr"].(string), "127.0.0.1:") {
		t.Fatalf("published data: %v", data)
	}

	hookWait(t, "recorded deliveries", func() bool {
		list, _ := d.Deliveries(context.Background(), "e2e", 0)
		return len(list) == 2 && list[0].Event == config.EventStreamEnded && list[0].Status == 200
	})
}
