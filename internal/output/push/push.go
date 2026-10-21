// Package push restreams live streams to other servers: YouTube, Twitch,
// Facebook or any RTMP/RTMPS server, SRT listeners, and Icecast radio
// servers.
//
// Targets are configured per app (config.App.Push). When a stream is
// published, every matching target gets its own goroutine that connects,
// sends the stream from a fresh hub subscription (so it starts at a
// keyframe), and reconnects with exponential backoff until the stream ends.
package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
)

// Target states reported by Status.
const (
	StateConnecting = "connecting"
	StateLive       = "live"
	StateRetrying   = "retrying"
	StateFailed     = "failed"
)

// Backoff parameters. A connection that stayed live for resetAfter starts
// again from minBackoff after it drops.
var (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	resetAfter = 30 * time.Second
)

const (
	// subscriberBuffer is the queue length, in units, of each push
	// subscription: several seconds of headroom for a slow uplink.
	subscriberBuffer = 2048
	connectTimeout   = 10 * time.Second
	writeTimeout     = 10 * time.Second
)

// Status describes one push target of one live stream.
type Status struct {
	Stream     string    `json:"stream"` // "<app>/<stream>"
	Name       string    `json:"name"`   // Push.Name, or "<scheme>://<host>" if empty
	URL        string    `json:"url"`    // redacted
	State      string    `json:"state"`  // "connecting", "live", "retrying", "failed"
	LastError  string    `json:"last_error,omitempty"`
	Since      time.Time `json:"since"`
	BytesSent  uint64    `json:"bytes_sent"`
	Reconnects int       `json:"reconnects"`
}

// Manager starts and stops push targets as streams come and go, and as
// targets are added, changed or removed in the configuration (Reconcile).
// It implements core.Hook.
type Manager struct {
	cfg config.Getter
	log *slog.Logger
	// OnState, when set before streams are published, is called whenever a
	// target changes state (outside the manager's locks).
	OnState func(Status)

	mu      sync.Mutex
	streams map[*core.Stream]*streamPush // every live stream, with or without targets
	closed  bool
	wg      sync.WaitGroup
}

type streamPush struct {
	ctx     context.Context
	cancel  context.CancelFunc
	targets map[string]*target // by targetKey
}

// NewManager creates a Manager.
func NewManager(cfg config.Getter, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{cfg: cfg, log: log, streams: make(map[*core.Stream]*streamPush)}
}

// OnPublish implements core.Hook. It starts one goroutine per matching
// target and returns immediately.
func (m *Manager) OnPublish(s *core.Stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sp := &streamPush{ctx: ctx, cancel: cancel, targets: map[string]*target{}}
	m.streams[s] = sp
	m.reconcileLocked(s, sp)
}

// Reconcile applies the current configuration to every live stream:
// new targets start, removed or disabled ones stop, and unchanged ones keep
// running without interruption. Call it after the configuration changes.
func (m *Manager) Reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for s, sp := range m.streams {
		m.reconcileLocked(s, sp)
	}
}

func targetKey(p config.Push, raw string) string { return p.Name + "\x00" + raw }

func (m *Manager) reconcileLocked(s *core.Stream, sp *streamPush) {
	app, name, pushes := selectPushes(m.cfg(), s)
	want := map[string]bool{}
	for _, p := range pushes {
		if p.Disabled {
			continue
		}
		raw := expand(p.URL, app, name)
		key := targetKey(p, raw)
		want[key] = true
		if sp.targets[key] != nil {
			continue
		}
		ctx, cancel := context.WithCancel(sp.ctx)
		t := &target{
			stream:  s,
			rawURL:  raw,
			secrets: secretsOf(raw),
			cancel:  cancel,
			onState: m.OnState,
			status: Status{
				Stream: s.Path,
				Name:   targetName(p, raw),
				URL:    redactURL(raw),
				State:  StateConnecting,
				Since:  time.Now(),
			},
		}
		t.log = m.log.With("path", s.Path, "target", t.status.Name, "url", t.status.URL)
		sp.targets[key] = t
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			t.run(ctx)
		}()
	}
	for key, t := range sp.targets {
		if !want[key] {
			t.log.Info("push target removed from configuration, stopping")
			t.cancel()
			delete(sp.targets, key)
		}
	}
}

// OnUnpublish implements core.Hook. It stops the stream's targets without
// waiting for them.
func (m *Manager) OnUnpublish(s *core.Stream) {
	m.mu.Lock()
	sp := m.streams[s]
	delete(m.streams, s)
	m.mu.Unlock()
	if sp != nil {
		sp.cancel()
	}
}

// Close stops every target and waits for them to finish.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for s, sp := range m.streams {
		sp.cancel()
		delete(m.streams, s)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Status returns the targets of all live streams, sorted by stream, then
// name.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	var out []Status
	for _, sp := range m.streams {
		for _, t := range sp.targets {
			out = append(out, t.snapshot())
		}
	}
	m.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Stream != out[j].Stream {
			return out[i].Stream < out[j].Stream
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err} }

func permanentf(format string, a ...any) error { return permanent(fmt.Errorf(format, a...)) }

// session is one connection to a target. send blocks until the
// subscription ends or a write fails; close aborts it from another
// goroutine.
type session interface {
	send(sub *core.Subscription) error
	close()
	bytesSent() uint64
}

// connector opens a session.
type connector func(ctx context.Context, rawURL string, s *core.Stream) (session, error)

func connectorFor(rawURL string) (connector, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	switch u.Scheme {
	case "rtmp", "rtmps":
		return dialRTMP, nil
	case "srt":
		return dialSRT, nil
	case "icecast", "icecasts":
		return dialIcecast, nil
	}
	return nil, fmt.Errorf("unsupported scheme %q (use rtmp, rtmps, srt, icecast or icecasts)", u.Scheme)
}

type target struct {
	stream  *core.Stream
	rawURL  string
	secrets []string
	log     *slog.Logger
	cancel  context.CancelFunc
	onState func(Status)

	mu        sync.Mutex
	status    Status
	sent      uint64 // bytes of finished sessions
	cur       session
	connector connector // overridable in tests
}

func (t *target) snapshot() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.status
	st.BytesSent = t.sent
	if t.cur != nil {
		st.BytesSent += t.cur.bytesSent()
	}
	return st
}

func (t *target) setState(state string, err error) {
	t.mu.Lock()
	changed := t.status.State != state
	if changed {
		t.status.State = state
		t.status.Since = time.Now()
	}
	if err != nil {
		t.status.LastError = scrub(err.Error(), t.secrets)
	}
	t.mu.Unlock()
	if changed && t.onState != nil {
		t.onState(t.snapshot())
	}
}

func (t *target) run(ctx context.Context) {
	connect := t.connector
	if connect == nil {
		c, err := connectorFor(t.rawURL)
		if err != nil {
			t.fail(permanent(err))
			return
		}
		connect = c
	}

	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		liveFor, err := t.attempt(ctx, connect)
		if ctx.Err() != nil {
			return
		}
		var perm *permanentError
		if errors.As(err, &perm) {
			t.fail(err)
			return
		}
		if errors.Is(err, errStreamEnded) {
			return
		}
		if liveFor >= resetAfter {
			backoff = minBackoff
		}

		t.mu.Lock()
		t.status.Reconnects++
		t.mu.Unlock()
		t.setState(StateRetrying, err)
		t.log.Warn("push failed, retrying", "err", scrub(err.Error(), t.secrets), "in", backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = nextBackoff(backoff)
		t.setState(StateConnecting, nil)
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	cur *= 2
	if cur > maxBackoff {
		cur = maxBackoff
	}
	if cur < minBackoff {
		cur = minBackoff
	}
	return cur
}

func (t *target) fail(err error) {
	t.setState(StateFailed, err)
	t.log.Error("push failed permanently", "err", scrub(err.Error(), t.secrets))
}

var errStreamEnded = errors.New("stream ended")

// streamEnded reports whether s has been unpublished. The hub closes every
// subscription when a stream ends, so a send that returned because its
// subscription closed is told apart from a dropped connection here.
func streamEnded(s *core.Stream) bool {
	probe := s.Subscribe("push-probe", 1)
	if probe == nil {
		return true
	}
	probe.Close()
	return false
}

func errOrEOF(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}

// attempt runs one connection and returns how long it was live.
func (t *target) attempt(ctx context.Context, connect connector) (time.Duration, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	sess, err := connect(cctx, t.rawURL, t.stream)
	cancel()
	if err != nil {
		return 0, err
	}

	sub := t.stream.Subscribe("push", subscriberBuffer)
	if sub == nil {
		sess.close()
		return 0, errStreamEnded
	}

	t.mu.Lock()
	t.cur = sess
	t.mu.Unlock()
	t.setState(StateLive, nil)
	t.log.Info("push live")
	start := time.Now()

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			sub.Close()
			sess.close()
		case <-done:
		}
	}()

	err = sess.send(sub)
	close(done)
	<-stopped
	sub.Close()
	sess.close()

	t.mu.Lock()
	t.sent += sess.bytesSent()
	t.cur = nil
	t.mu.Unlock()

	if ctx.Err() == nil && streamEnded(t.stream) {
		return time.Since(start), errStreamEnded
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = fmt.Errorf("connection closed: %w", errOrEOF(err))
	}
	return time.Since(start), err
}
