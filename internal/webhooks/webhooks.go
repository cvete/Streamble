// Package webhooks sends server events (stream published or ended, push,
// pull and transcode state changes) as signed JSON POST requests to the
// webhooks configured in the live settings, and records every delivery.
//
// Each webhook has its own worker and bounded queue: deliveries to one
// webhook happen in order, different webhooks do not wait for each other,
// and a slow endpoint never blocks the event bus (events are dropped, with
// a warning, when its queue is full).
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/store"
)

// EventPing is the event type sent by Test.
const EventPing = "ping"

// DefaultRetryDelays are the waits before each retry.
var DefaultRetryDelays = []time.Duration{time.Second, 10 * time.Second, time.Minute}

// Defaults for the optional Dispatcher fields.
const (
	DefaultQueueSize    = 256
	DefaultKeep         = 500
	DefaultTimeout      = 5 * time.Second
	DefaultCloseTimeout = 5 * time.Second

	maxErrorLen     = 300
	maxRedirects    = 3
	reconcilePeriod = 30 * time.Second
)

// ErrUnknownWebhook is returned by Test for a name that is not configured.
var ErrUnknownWebhook = errors.New("no such webhook")

// Delivery is the record of one event sent to one webhook (with retries).
type Delivery struct {
	ID      string    `json:"id"`
	Webhook string    `json:"-"`
	Event   string    `json:"event"`
	At      time.Time `json:"at"`
	// Status is the HTTP status of the last attempt, 0 for a network error.
	Status   int    `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
	// DurationMS is how long the last attempt took.
	DurationMS int64 `json:"duration_ms"`
}

// OK reports whether the endpoint accepted the delivery (2xx).
func (d *Delivery) OK() bool { return d.Status >= 200 && d.Status <= 299 }

// Dispatcher delivers bus events to webhooks.
type Dispatcher struct {
	Bus *events.Bus
	// Settings returns the current configuration; webhooks are read from it
	// for every event, so changes apply immediately.
	Settings config.Getter
	DB       *store.DB
	Log      *slog.Logger
	// Client sends the requests. Optional: the default has a 5 s timeout.
	// Redirects to another host are never followed.
	Client *http.Client

	// RetryDelays are the waits before each retry (default 1 s, 10 s, 60 s).
	RetryDelays []time.Duration
	// QueueSize bounds each webhook's queue (default 256).
	QueueSize int
	// Keep is how many deliveries are kept per webhook (default 500).
	Keep int
	// CloseTimeout bounds how long Close waits for in-flight requests.
	CloseTimeout time.Duration

	initOnce sync.Once
	client   *http.Client
	ctx      context.Context // canceled when Close gives up waiting
	cancel   context.CancelFunc
	quit     chan struct{}
	loopDone chan struct{}
	started  bool
	sub      *events.Subscription

	mu      sync.Mutex
	workers map[string]*worker
	wg      sync.WaitGroup
	lastAt  int64 // Unix ms; keeps delivery times strictly increasing
}

type worker struct {
	name  string
	queue chan job
	quit  chan struct{}
}

type job struct {
	hook  config.Webhook
	event events.Event
}

func (d *Dispatcher) init() {
	d.initOnce.Do(func() {
		if d.Log == nil {
			d.Log = slog.Default()
		}
		if d.RetryDelays == nil {
			d.RetryDelays = DefaultRetryDelays
		}
		if d.QueueSize <= 0 {
			d.QueueSize = DefaultQueueSize
		}
		if d.Keep <= 0 {
			d.Keep = DefaultKeep
		}
		if d.CloseTimeout <= 0 {
			d.CloseTimeout = DefaultCloseTimeout
		}
		c := http.Client{Timeout: DefaultTimeout}
		if d.Client != nil {
			c = *d.Client
		}
		c.CheckRedirect = checkRedirect
		d.client = &c
		d.ctx, d.cancel = context.WithCancel(context.Background())
		d.quit = make(chan struct{})
		d.loopDone = make(chan struct{})
		d.workers = make(map[string]*worker)
	})
}

// checkRedirect follows a few redirects on the same host only; otherwise
// the redirect response itself is the result (a failed delivery).
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects || req.URL.Host != via[0].URL.Host {
		return http.ErrUseLastResponse
	}
	return nil
}

// Start subscribes to the bus and returns immediately.
func (d *Dispatcher) Start() {
	d.init()
	d.mu.Lock()
	closed := false
	select {
	case <-d.quit:
		closed = true
	default:
	}
	if d.started || closed {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.sub = d.Bus.Subscribe(1024)
	d.mu.Unlock()
	go d.loop()
}

// Close stops the dispatcher. Queued deliveries are dropped and pending
// retries abandoned (their last result is recorded); requests in flight
// get up to CloseTimeout to finish.
func (d *Dispatcher) Close() {
	d.init()
	d.mu.Lock()
	select {
	case <-d.quit:
		d.mu.Unlock()
		return
	default:
	}
	close(d.quit)
	started := d.started
	d.mu.Unlock()

	if started {
		d.sub.Close()
		<-d.loopDone
	}
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d.CloseTimeout):
		d.cancel()
		<-done
	}
	d.cancel()
}

func (d *Dispatcher) loop() {
	defer close(d.loopDone)
	t := time.NewTicker(reconcilePeriod)
	defer t.Stop()
	for {
		select {
		case <-d.quit:
			return
		case <-t.C:
			d.reconcile(d.Settings())
		case e, ok := <-d.sub.C():
			if !ok {
				return
			}
			if n := d.sub.Dropped(); n > 0 {
				d.Log.Warn("webhooks fell behind; events were lost", "events", n)
			}
			d.dispatch(e)
		}
	}
}

// dispatch queues e for every matching webhook of the current settings.
func (d *Dispatcher) dispatch(e events.Event) {
	cfg := d.Settings()
	d.reconcile(cfg)
	for _, h := range cfg.Webhooks {
		if !Matches(h, e.Type) {
			continue
		}
		w := d.worker(h.Name)
		if w == nil {
			continue
		}
		select {
		case w.queue <- job{hook: h, event: e}:
		default:
			d.Log.Warn("webhook queue full; event dropped", "webhook", h.Name, "event", e.Type)
		}
	}
}

// Matches reports whether webhook h wants events of type t.
func Matches(h config.Webhook, t string) bool {
	return !h.Disabled && (len(h.Events) == 0 || slices.Contains(h.Events, t))
}

// worker returns the worker for name, starting it if needed. It returns nil
// once the dispatcher is closing.
func (d *Dispatcher) worker(name string) *worker {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.quit:
		return nil
	default:
	}
	if w := d.workers[name]; w != nil {
		return w
	}
	w := &worker{name: name, queue: make(chan job, d.QueueSize), quit: make(chan struct{})}
	d.workers[name] = w
	d.wg.Add(1)
	go d.run(w)
	return w
}

// reconcile stops the workers of webhooks that were removed or disabled.
func (d *Dispatcher) reconcile(cfg *config.Config) {
	want := map[string]bool{}
	for _, h := range cfg.Webhooks {
		if !h.Disabled {
			want[h.Name] = true
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, w := range d.workers {
		if !want[name] {
			close(w.quit)
			delete(d.workers, name)
		}
	}
}

// Workers returns the names of the running workers (for tests and status).
func (d *Dispatcher) Workers() []string {
	d.init()
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for name := range d.workers {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func (d *Dispatcher) run(w *worker) {
	defer d.wg.Done()
	for {
		select {
		case <-w.quit:
			return
		case <-d.quit:
			return
		case j := <-w.queue:
			d.deliver(d.ctx, j.hook, j.event, true, w.quit)
		}
	}
}

// Test sends a ping event to the named webhook now, once, without retries,
// records it and returns the delivery. Disabled webhooks can be tested.
func (d *Dispatcher) Test(ctx context.Context, name string) (*Delivery, error) {
	d.init()
	h := d.Settings().Webhook(name)
	if h == nil {
		return nil, ErrUnknownWebhook
	}
	e := events.Event{Type: EventPing, At: time.Now().UTC(), Data: map[string]any{
		"webhook": h.Name,
		"message": "Test delivery from Streamble",
	}}
	return d.deliver(ctx, *h, e, false, nil), nil
}

// payload is the request body.
type payload struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data"`
}

// Sign returns the X-Streamble-Signature value for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// deliver sends e to h, retrying network errors and 5xx when retry is set,
// and records the result. stop (optional) aborts pending retries.
func (d *Dispatcher) deliver(ctx context.Context, h config.Webhook, e events.Event, retry bool, stop <-chan struct{}) *Delivery {
	del := &Delivery{ID: store.NewID(), Webhook: h.Name, Event: e.Type, At: d.now()}
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	body, err := json.Marshal(payload{ID: del.ID, Type: e.Type, At: e.At, Data: data})
	if err != nil {
		del.Error = "cannot encode event: " + err.Error()
		d.record(del, nil)
		return del
	}

	for {
		del.Attempts++
		status, dur, errText := d.attempt(ctx, h, del.ID, e.Type, body)
		del.Status, del.DurationMS, del.Error = status, dur.Milliseconds(), errText
		if del.OK() || !retry || (status != 0 && status < 500) || del.Attempts > len(d.RetryDelays) {
			break
		}
		wait := d.RetryDelays[del.Attempts-1]
		d.Log.Debug("webhook delivery failed; retrying", "webhook", h.Name, "event", e.Type,
			"status", status, "attempt", del.Attempts, "retry_in", wait)
		if !sleep(wait, stop, d.quit, ctx.Done()) {
			break
		}
	}

	if del.OK() {
		d.Log.Debug("webhook delivered", "webhook", h.Name, "event", e.Type, "status", del.Status, "attempts", del.Attempts)
	} else {
		d.Log.Warn("webhook delivery failed", "webhook", h.Name, "event", e.Type,
			"status", del.Status, "attempts", del.Attempts, "err", del.Error)
	}
	d.record(del, body)
	return del
}

// sleep waits for d; it returns false when a, b or c is closed first (nil
// channels never are).
func sleep(d time.Duration, a, b, c <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-a:
	case <-b:
	case <-c:
	}
	return false
}

// attempt sends one request. It returns the status (0 on network errors),
// the duration and an error text ("" on 2xx).
func (d *Dispatcher) attempt(ctx context.Context, h config.Webhook, id, event string, body []byte) (int, time.Duration, string) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return 0, 0, clean("invalid URL", h.Secret)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Streamble-Webhooks")
	req.Header.Set("X-Streamble-Event", event)
	req.Header.Set("X-Streamble-Delivery", id)
	if h.Secret != "" {
		req.Header.Set("X-Streamble-Signature", Sign(h.Secret, body))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		// *url.Error repeats the URL, which may carry a token; keep the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, time.Since(start), clean(err.Error(), h.Secret)
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck
	resp.Body.Close()
	dur := time.Since(start)
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return resp.StatusCode, dur, ""
	}
	msg := "HTTP " + strconv.Itoa(resp.StatusCode)
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		msg += " (redirects to another host are not followed)"
	}
	if s := strings.TrimSpace(string(snippet)); s != "" {
		msg += ": " + s
	}
	return resp.StatusCode, dur, clean(msg, h.Secret)
}

// clean removes the secret from an error text and truncates it.
func clean(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "********")
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxErrorLen {
		cut := maxErrorLen
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// now returns the current time in ms, strictly after the previous call, so
// deliveries of one webhook sort in the order they were made.
func (d *Dispatcher) now() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	ms := time.Now().UnixMilli()
	if ms <= d.lastAt {
		ms = d.lastAt + 1
	}
	d.lastAt = ms
	return time.UnixMilli(ms).UTC()
}

// record stores a delivery and prunes old ones of the same webhook.
func (d *Dispatcher) record(del *Delivery, body []byte) {
	if d.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := d.DB.Exec(ctx, `INSERT INTO webhook_deliveries
		(id, webhook, event, at, status, attempts, error, duration_ms, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		del.ID, del.Webhook, del.Event, del.At.UnixMilli(), del.Status, del.Attempts,
		del.Error, del.DurationMS, string(body)); err != nil {
		d.Log.Warn("cannot record webhook delivery", "webhook", del.Webhook, "err", err)
		return
	}
	if _, err := d.DB.Exec(ctx, `DELETE FROM webhook_deliveries WHERE webhook = ? AND id NOT IN
		(SELECT id FROM webhook_deliveries WHERE webhook = ? ORDER BY at DESC, id DESC LIMIT ?)`,
		del.Webhook, del.Webhook, d.Keep); err != nil {
		d.Log.Warn("cannot prune webhook deliveries", "webhook", del.Webhook, "err", err)
	}
}

// Deliveries returns the recorded deliveries of a webhook, newest first.
func (d *Dispatcher) Deliveries(ctx context.Context, name string, limit int) ([]*Delivery, error) {
	d.init()
	if d.DB == nil {
		return []*Delivery{}, nil
	}
	if limit <= 0 || limit > d.Keep {
		limit = d.Keep
	}
	rows, err := d.DB.Query(ctx, `SELECT id, webhook, event, at, status, attempts, error, duration_ms
		FROM webhook_deliveries WHERE webhook = ? ORDER BY at DESC, id DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Delivery{}
	for rows.Next() {
		var (
			del             Delivery
			at, status, att int64
		)
		if err := rows.Scan(&del.ID, &del.Webhook, &del.Event, &at, &status, &att, &del.Error, &del.DurationMS); err != nil {
			return nil, err
		}
		del.At, del.Status, del.Attempts = store.Time(at).UTC(), int(status), int(att)
		out = append(out, &del)
	}
	return out, rows.Err()
}

// Payload returns the stored request body of a delivery (for debugging).
func (d *Dispatcher) Payload(ctx context.Context, id string) (string, error) {
	if d.DB == nil {
		return "", store.ErrNotFound
	}
	var p string
	err := d.DB.QueryRow(ctx, `SELECT payload FROM webhook_deliveries WHERE id = ?`, id).Scan(&p)
	if err != nil {
		return "", fmt.Errorf("delivery %s: %w", id, err)
	}
	return p, nil
}
