package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/store"
)

// pgSchema isolates this package's PostgreSQL tests from the store tests,
// which drop the public schema of the same database.
const pgSchema = "webhooks_test"

// forEachDB runs fn against SQLite, and against PostgreSQL when
// STREAMBLE_TEST_POSTGRES is set.
func forEachDB(t *testing.T, fn func(t *testing.T, db *store.DB)) {
	t.Run("sqlite", func(t *testing.T) {
		db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "hooks.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		fn(t, db)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("STREAMBLE_TEST_POSTGRES")
		if dsn == "" {
			t.Skip("STREAMBLE_TEST_POSTGRES not set")
		}
		fn(t, openPostgres(t, dsn))
	})
}

func openPostgres(t *testing.T, dsn string) *store.DB {
	t.Helper()
	admin, err := store.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.SQL().Exec(`DROP SCHEMA IF EXISTS ` + pgSchema + ` CASCADE; CREATE SCHEMA ` + pgSchema)
	admin.Close()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("options", "-csearch_path="+pgSchema)
	u.RawQuery = q.Encode()
	db, err := store.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// liveConfig is a mutable config.Getter.
type liveConfig struct {
	mu  sync.Mutex
	cfg *config.Config
}

func newLive(hooks ...config.Webhook) *liveConfig {
	cfg := config.Default()
	cfg.Webhooks = hooks
	return &liveConfig{cfg: cfg}
}

func (l *liveConfig) get() *config.Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

func (l *liveConfig) set(hooks ...config.Webhook) {
	cfg := config.Default()
	cfg.Webhooks = hooks
	l.mu.Lock()
	l.cfg = cfg
	l.mu.Unlock()
}

// hit is one request received by a receiver.
type hit struct {
	header http.Header
	body   []byte
	msg    payload
}

// receiver is a webhook endpoint that records requests and answers with
// the status returned by reply (200 when nil).
type receiver struct {
	*httptest.Server
	mu    sync.Mutex
	hits  []hit
	reply func(n int, h hit) int
}

func newReceiver(t *testing.T, reply func(n int, h hit) int) *receiver {
	rc := &receiver{reply: reply}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h := hit{header: r.Header.Clone(), body: b}
		json.Unmarshal(b, &h.msg) //nolint:errcheck
		rc.mu.Lock()
		rc.hits = append(rc.hits, h)
		n := len(rc.hits)
		rc.mu.Unlock()
		code := http.StatusOK
		if rc.reply != nil {
			code = rc.reply(n, h)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) all() []hit {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]hit(nil), rc.hits...)
}

func (rc *receiver) count() int { return len(rc.all()) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newDispatcher(t *testing.T, db *store.DB, live *liveConfig) (*Dispatcher, *events.Bus) {
	bus := events.NewBus()
	d := &Dispatcher{Bus: bus, Settings: live.get, DB: db, Log: quietLog(),
		RetryDelays: []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 15 * time.Millisecond}}
	d.Start()
	t.Cleanup(d.Close)
	return d, bus
}

func verify(secret string, body []byte, header string) bool {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}

func TestSignedDelivery(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *store.DB) {
		rc := newReceiver(t, nil)
		plain := newReceiver(t, nil)
		live := newLive(
			config.Webhook{Name: "signed", URL: rc.URL + "/hook", Secret: "s3cret-value"},
			config.Webhook{Name: "plain", URL: plain.URL},
		)
		d, bus := newDispatcher(t, db, live)

		at := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
		bus.Publish(events.Event{Type: config.EventStreamPublished, At: at,
			Data: map[string]any{"path": "live/tv1", "protocol": "rtmp"}})
		waitFor(t, "deliveries", func() bool { return rc.count() == 1 && plain.count() == 1 })

		h := rc.all()[0]
		if !verify("s3cret-value", h.body, h.header.Get("X-Streamble-Signature")) {
			t.Fatalf("bad signature %q", h.header.Get("X-Streamble-Signature"))
		}
		if verify("other", h.body, h.header.Get("X-Streamble-Signature")) {
			t.Fatal("signature verifies with the wrong secret")
		}
		if got := h.header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content type %q", got)
		}
		if got := h.header.Get("User-Agent"); got != "Streamble-Webhooks" {
			t.Fatalf("user agent %q", got)
		}
		if got := h.header.Get("X-Streamble-Event"); got != config.EventStreamPublished {
			t.Fatalf("event header %q", got)
		}
		if h.msg.ID == "" || h.header.Get("X-Streamble-Delivery") != h.msg.ID {
			t.Fatalf("delivery id header %q, body id %q", h.header.Get("X-Streamble-Delivery"), h.msg.ID)
		}
		if h.msg.Type != config.EventStreamPublished || !h.msg.At.Equal(at) || h.msg.Data["path"] != "live/tv1" {
			t.Fatalf("body %s", h.body)
		}
		var raw map[string]any
		json.Unmarshal(h.body, &raw) //nolint:errcheck
		if len(raw) != 4 {
			t.Fatalf("body must have exactly id, type, at, data: %s", h.body)
		}
		if ph := plain.all()[0]; ph.header.Get("X-Streamble-Signature") != "" {
			t.Fatal("signature sent without a secret")
		}

		var list []*Delivery
		waitFor(t, "recorded delivery", func() bool {
			list, _ = d.Deliveries(context.Background(), "signed", 0)
			return len(list) == 1
		})
		del := list[0]
		if del.ID != h.msg.ID || del.Event != config.EventStreamPublished || del.Status != 200 ||
			del.Attempts != 1 || del.Error != "" || del.At.IsZero() {
			t.Fatalf("recorded %+v", del)
		}
		p, err := d.Payload(context.Background(), del.ID)
		if err != nil || p != string(h.body) {
			t.Fatalf("stored payload %q (%v), sent %q", p, err, h.body)
		}
	})
}

func TestEventFilterAndDisabled(t *testing.T) {
	all := newReceiver(t, nil)
	ended := newReceiver(t, nil)
	off := newReceiver(t, nil)
	live := newLive(
		config.Webhook{Name: "all", URL: all.URL},
		config.Webhook{Name: "ended", URL: ended.URL, Events: []string{config.EventStreamEnded}},
		config.Webhook{Name: "off", URL: off.URL, Disabled: true},
	)
	_, bus := newDispatcher(t, nil, live)

	bus.Publish(events.Event{Type: config.EventStreamPublished})
	bus.Publish(events.Event{Type: config.EventPushState})
	bus.Publish(events.Event{Type: config.EventStreamEnded})
	waitFor(t, "deliveries", func() bool { return all.count() == 3 && ended.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if ended.count() != 1 || ended.all()[0].msg.Type != config.EventStreamEnded {
		t.Fatalf("filtered webhook got %d events", ended.count())
	}
	if off.count() != 0 {
		t.Fatal("disabled webhook received events")
	}
	if !Matches(config.Webhook{}, "anything") || Matches(config.Webhook{Disabled: true}, "x") ||
		Matches(config.Webhook{Events: []string{"a"}}, "b") {
		t.Fatal("Matches")
	}
}

func TestRetries(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *store.DB) {
		flaky := newReceiver(t, func(n int, _ hit) int {
			if n < 3 {
				return http.StatusBadGateway
			}
			return http.StatusOK
		})
		bad := newReceiver(t, func(int, hit) int { return http.StatusNotFound })
		down := newReceiver(t, func(int, hit) int { return http.StatusServiceUnavailable })
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		closedURL := "http://" + l.Addr().String() + "/x"
		l.Close()

		live := newLive(
			config.Webhook{Name: "flaky", URL: flaky.URL},
			config.Webhook{Name: "bad", URL: bad.URL},
			config.Webhook{Name: "down", URL: down.URL},
			config.Webhook{Name: "gone", URL: closedURL},
		)
		d, bus := newDispatcher(t, db, live)
		bus.Publish(events.Event{Type: config.EventStreamEnded})

		get := func(name string) *Delivery {
			var list []*Delivery
			waitFor(t, name+" delivery", func() bool {
				list, _ = d.Deliveries(context.Background(), name, 0)
				return len(list) == 1
			})
			return list[0]
		}
		if del := get("flaky"); del.Status != 200 || del.Attempts != 3 || del.Error != "" {
			t.Fatalf("flaky: %+v", del)
		}
		if flaky.count() != 3 {
			t.Fatalf("flaky got %d requests", flaky.count())
		}
		// All three attempts carry the same delivery id.
		for _, h := range flaky.all() {
			if h.msg.ID != flaky.all()[0].msg.ID {
				t.Fatal("retries changed the delivery id")
			}
		}
		if del := get("bad"); del.Status != 404 || del.Attempts != 1 || !strings.Contains(del.Error, "404") {
			t.Fatalf("4xx must not be retried: %+v", del)
		}
		if del := get("down"); del.Status != 503 || del.Attempts != 4 {
			t.Fatalf("5xx: %+v", del)
		}
		if del := get("gone"); del.Status != 0 || del.Attempts != 4 || del.Error == "" {
			t.Fatalf("network error: %+v", del)
		}
		if bad.count() != 1 || down.count() != 4 {
			t.Fatalf("requests: bad %d, down %d", bad.count(), down.count())
		}
	})
}

func TestOrderPerWebhook(t *testing.T) {
	release := make(chan struct{})
	slow := newReceiver(t, func(int, hit) int { <-release; return 200 })
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var jitter atomic.Int64
	ordered := newReceiver(t, func(int, hit) int {
		time.Sleep(time.Duration(jitter.Add(7)%5) * time.Millisecond)
		return 200
	})
	live := newLive(
		config.Webhook{Name: "slow", URL: slow.URL},
		config.Webhook{Name: "ordered", URL: ordered.URL},
	)
	_, bus := newDispatcher(t, nil, live)

	const n = 30
	for i := range n {
		bus.Publish(events.Event{Type: config.EventPushState, Data: map[string]any{"seq": i}})
	}
	// The slow webhook does not hold up the other one.
	waitFor(t, "ordered deliveries", func() bool { return ordered.count() == n })
	for i, h := range ordered.all() {
		if seq, _ := h.msg.Data["seq"].(float64); int(seq) != i {
			t.Fatalf("delivery %d has seq %v", i, h.msg.Data["seq"])
		}
	}
	if slow.count() != 1 {
		t.Fatalf("slow webhook: %d requests in flight, want 1", slow.count())
	}
	close(release)
	waitFor(t, "slow deliveries", func() bool { return slow.count() == n })
	for i, h := range slow.all() {
		if seq, _ := h.msg.Data["seq"].(float64); int(seq) != i {
			t.Fatalf("slow delivery %d has seq %v", i, h.msg.Data["seq"])
		}
	}
}

func TestQueueFullDrops(t *testing.T) {
	release := make(chan struct{})
	rc := newReceiver(t, func(int, hit) int { <-release; return 200 })
	live := newLive(config.Webhook{Name: "stuck", URL: rc.URL})
	bus := events.NewBus()
	d := &Dispatcher{Bus: bus, Settings: live.get, Log: quietLog(), QueueSize: 2}
	d.Start()
	defer d.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	bus.Publish(events.Event{Type: config.EventPushState, Data: map[string]any{"seq": 0}})
	waitFor(t, "first request", func() bool { return rc.count() == 1 })
	done := make(chan struct{})
	go func() {
		for i := 1; i < 20; i++ {
			bus.Publish(events.Event{Type: config.EventPushState, Data: map[string]any{"seq": i}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing blocked on a stuck webhook")
	}
	// Let the dispatcher drain the bus into the (full) queue.
	time.Sleep(100 * time.Millisecond)
	unblock()
	waitFor(t, "queued deliveries", func() bool { return rc.count() == 3 })
	time.Sleep(100 * time.Millisecond)
	if got := rc.count(); got != 3 {
		t.Fatalf("got %d deliveries, want 3 (1 in flight + queue of 2)", got)
	}
	if seq := rc.all()[2].msg.Data["seq"].(float64); seq != 2 {
		t.Fatalf("kept seq %v, want the oldest queued events", seq)
	}
}

func TestHotReload(t *testing.T) {
	rc := newReceiver(t, nil)
	live := newLive()
	d, bus := newDispatcher(t, nil, live)

	bus.Publish(events.Event{Type: config.EventStreamPublished, Data: map[string]any{"seq": 0}})
	time.Sleep(50 * time.Millisecond)
	if rc.count() != 0 || len(d.Workers()) != 0 {
		t.Fatal("delivered without webhooks")
	}

	live.set(config.Webhook{Name: "new", URL: rc.URL})
	bus.Publish(events.Event{Type: config.EventStreamPublished, Data: map[string]any{"seq": 1}})
	waitFor(t, "delivery to the added webhook", func() bool { return rc.count() == 1 })
	if w := d.Workers(); len(w) != 1 || w[0] != "new" {
		t.Fatalf("workers %v", w)
	}

	// Changing the event filter applies to the next event.
	live.set(config.Webhook{Name: "new", URL: rc.URL, Events: []string{config.EventStreamEnded}})
	bus.Publish(events.Event{Type: config.EventStreamPublished, Data: map[string]any{"seq": 2}})
	bus.Publish(events.Event{Type: config.EventStreamEnded, Data: map[string]any{"seq": 3}})
	waitFor(t, "filtered delivery", func() bool { return rc.count() == 2 })
	if seq := rc.all()[1].msg.Data["seq"].(float64); seq != 3 {
		t.Fatalf("got seq %v", seq)
	}

	live.set()
	bus.Publish(events.Event{Type: config.EventStreamEnded, Data: map[string]any{"seq": 4}})
	waitFor(t, "worker removal", func() bool { return len(d.Workers()) == 0 })
	time.Sleep(50 * time.Millisecond)
	if rc.count() != 2 {
		t.Fatal("removed webhook still receives events")
	}

	// Disabled also stops the worker.
	live.set(config.Webhook{Name: "new", URL: rc.URL})
	bus.Publish(events.Event{Type: config.EventStreamEnded})
	waitFor(t, "worker", func() bool { return len(d.Workers()) == 1 })
	live.set(config.Webhook{Name: "new", URL: rc.URL, Disabled: true})
	bus.Publish(events.Event{Type: config.EventStreamEnded})
	waitFor(t, "worker removal", func() bool { return len(d.Workers()) == 0 })
}

func TestPruneAndTest(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *store.DB) {
		rc := newReceiver(t, nil)
		live := newLive(config.Webhook{Name: "a", URL: rc.URL, Secret: "k", Disabled: true},
			config.Webhook{Name: "b", URL: rc.URL})
		d := &Dispatcher{Settings: live.get, DB: db, Log: quietLog(), Keep: 5}
		ctx := context.Background()

		var ids []string
		for range 8 {
			del, err := d.Test(ctx, "a")
			if err != nil || !del.OK() || del.Event != EventPing || del.Attempts != 1 {
				t.Fatalf("test delivery %+v %v", del, err)
			}
			ids = append(ids, del.ID)
		}
		if _, err := d.Test(ctx, "b"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Test(ctx, "nope"); err != ErrUnknownWebhook {
			t.Fatalf("unknown webhook: %v", err)
		}
		h := rc.all()[0]
		if h.msg.Type != EventPing || h.header.Get("X-Streamble-Event") != EventPing ||
			!verify("k", h.body, h.header.Get("X-Streamble-Signature")) {
			t.Fatalf("ping %s", h.body)
		}

		list, err := d.Deliveries(ctx, "a", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 5 {
			t.Fatalf("kept %d deliveries, want 5", len(list))
		}
		for i, del := range list {
			if del.ID != ids[len(ids)-1-i] {
				t.Fatalf("delivery %d is %s, want newest first", i, del.ID)
			}
		}
		var n int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM webhook_deliveries`).Scan(&n); err != nil || n != 6 {
			t.Fatalf("rows %d %v (pruning must not touch other webhooks)", n, err)
		}
	})
}

func TestNoSecretInErrors(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *store.DB) {
		const secret = "very-secret-token"
		echo := newReceiver(t, func(int, hit) int { return http.StatusBadRequest })
		echo.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "bad signature, expected secret %s\n%s", secret, strings.Repeat("x", 2000))
		})
		live := newLive(
			config.Webhook{Name: "echo", URL: echo.URL, Secret: secret},
			config.Webhook{Name: "badurl", URL: "http://127.0.0.1:1/" + secret, Secret: secret},
		)
		d := &Dispatcher{Settings: live.get, DB: db, Log: quietLog()}
		ctx := context.Background()
		for _, name := range []string{"echo", "badurl"} {
			del, err := d.Test(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			if del.Error == "" || strings.Contains(del.Error, secret) || len(del.Error) > maxErrorLen+4 {
				t.Fatalf("%s: error %q", name, del.Error)
			}
			list, _ := d.Deliveries(ctx, name, 0)
			if len(list) != 1 || strings.Contains(list[0].Error, secret) {
				t.Fatalf("%s: stored %+v", name, list)
			}
			p, _ := d.Payload(ctx, del.ID)
			if strings.Contains(p, secret) {
				t.Fatal("secret in stored payload")
			}
		}
	})
}

func TestRedirects(t *testing.T) {
	other := newReceiver(t, nil)
	var final atomic.Int32
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/moved":
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		case "/away":
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
		default:
			final.Add(1)
		}
	}))
	t.Cleanup(same.Close)
	live := newLive(
		config.Webhook{Name: "moved", URL: same.URL + "/moved"},
		config.Webhook{Name: "away", URL: same.URL + "/away"},
	)
	d := &Dispatcher{Settings: live.get, Log: quietLog()}
	if del, _ := d.Test(context.Background(), "moved"); !del.OK() || final.Load() != 1 {
		t.Fatalf("same-host redirect: %+v", del)
	}
	if del, _ := d.Test(context.Background(), "away"); del.OK() || del.Status != 307 || other.count() != 0 {
		t.Fatalf("cross-host redirect followed: %+v", del)
	}
}

func TestCloseIsBounded(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	rc := newReceiver(t, func(int, hit) int { <-block; return 200 })
	live := newLive(config.Webhook{Name: "hang", URL: rc.URL})
	bus := events.NewBus()
	d := &Dispatcher{Bus: bus, Settings: live.get, Log: quietLog(), CloseTimeout: 100 * time.Millisecond,
		Client: &http.Client{Timeout: time.Minute}}
	d.Start()
	bus.Publish(events.Event{Type: config.EventStreamEnded})
	waitFor(t, "request in flight", func() bool { return rc.count() == 1 })
	start := time.Now()
	d.Close()
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Close took %v", el)
	}
	d.Close() // idempotent
	bus.Publish(events.Event{Type: config.EventStreamEnded})
}

func TestCloseAbortsRetries(t *testing.T) {
	rc := newReceiver(t, func(int, hit) int { return 500 })
	live := newLive(config.Webhook{Name: "x", URL: rc.URL})
	bus := events.NewBus()
	d := &Dispatcher{Bus: bus, Settings: live.get, Log: quietLog(), RetryDelays: []time.Duration{time.Hour}}
	d.Start()
	bus.Publish(events.Event{Type: config.EventStreamEnded})
	waitFor(t, "first attempt", func() bool { return rc.count() == 1 })
	start := time.Now()
	d.Close()
	if time.Since(start) > time.Second {
		t.Fatal("Close waited for a retry delay")
	}
}
