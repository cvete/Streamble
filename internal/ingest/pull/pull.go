// Package pull fetches streams from remote URLs and publishes them into the
// hub ("pull ingest"). Every configured source runs in its own goroutine,
// forever, reconnecting with exponential backoff when the source fails.
//
// Supported URLs:
//   - http(s):// MPEG-TS, Icecast/Shoutcast MP3 or AAC (ADTS), and HLS
//     (.m3u8), detected from Content-Type with the URL extension as fallback
//   - udp:// MPEG-TS, unicast or multicast ("udp://@:5000" listens on all
//     addresses, as in FFmpeg)
//   - srt:// caller mode, MPEG-TS
//   - rtmp:// and rtmps:// playback
//
// URLs often carry credentials (user:pass@, ?key=, ?passphrase=, a
// streamid with a key), so they are only ever logged or reported in
// redacted form; see RedactURL.
package pull

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
)

// Source states reported by Status.
const (
	StateConnecting = "connecting"
	StateLive       = "live"
	StateRetrying   = "retrying"
)

const (
	// readTimeout is how long a source may stay silent before the
	// connection is considered dead.
	readTimeout = 10 * time.Second
	// minBackoff and maxBackoff bound the delay between reconnects.
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	// stableAfter is how long a connection must stay live before the
	// backoff starts again from minBackoff.
	stableAfter = 30 * time.Second
)

// errIdle is returned when a source sent nothing for readTimeout.
var errIdle = errors.New("no data received for 10s")

// Manager pulls every configured source into the hub.
type Manager struct {
	Sources []config.Source
	Hub     *core.Hub
	Log     *slog.Logger

	mu      sync.Mutex
	status  []*Status
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// Status describes one pull source.
type Status struct {
	Path       string    `json:"path"`
	URL        string    `json:"url"` // redacted: no userinfo, query parameter values replaced with REDACTED
	State      string    `json:"state"`
	LastError  string    `json:"last_error,omitempty"`
	Since      time.Time `json:"since"` // when State last changed
	Reconnects int       `json:"reconnects"`
}

// Start launches one goroutine per source and returns immediately.
// Calling it more than once has no effect.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return
	}
	m.started = true
	if m.Log == nil {
		m.Log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	now := time.Now()
	for _, src := range m.Sources {
		st := &Status{Path: src.Path, URL: RedactURL(src.URL), State: StateConnecting, Since: now}
		m.status = append(m.status, st)
		r := &runner{m: m, src: src, st: st, redacted: st.URL}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			r.run(ctx)
		}()
	}
}

// Close stops every source and waits for their goroutines to exit. Their
// streams are unpublished. It is safe to call more than once, and without
// Start.
func (m *Manager) Close() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// Status returns the state of every source, sorted by path.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	out := make([]Status, 0, len(m.status))
	for _, st := range m.status {
		out = append(out, *st)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// runner pulls one source forever.
type runner struct {
	m        *Manager
	src      config.Source
	st       *Status // guarded by m.mu
	redacted string

	liveAt time.Time // set by markLive, read by run (same goroutine chain)
	liveMu sync.Mutex
}

func (r *runner) setState(state, lastErr string) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.st.State != state {
		r.st.State = state
		r.st.Since = time.Now()
	}
	if lastErr != "" {
		r.st.LastError = lastErr
	}
}

// markLive is called by a puller once its stream is published.
func (r *runner) markLive() {
	r.liveMu.Lock()
	r.liveAt = time.Now()
	r.liveMu.Unlock()
	r.setState(StateLive, "")
	r.m.Log.Info("pull source live", "path", r.src.Path, "url", r.redacted)
}

func (r *runner) liveFor() time.Duration {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.liveAt.IsZero() {
		return 0
	}
	return time.Since(r.liveAt)
}

func (r *runner) sourceInfo() core.SourceInfo {
	return core.SourceInfo{Protocol: "pull", RemoteAddr: r.redacted}
}

func (r *runner) run(ctx context.Context) {
	var b backoff
	for {
		r.liveMu.Lock()
		r.liveAt = time.Time{}
		r.liveMu.Unlock()
		r.setState(StateConnecting, "")

		err := r.pullOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if r.liveFor() >= stableAfter {
			b.Reset()
		}
		delay := b.Next()
		msg := "stream ended"
		if err != nil {
			msg = sanitizeError(err, r.src.URL)
		}
		r.setState(StateRetrying, msg)
		if errors.Is(err, core.ErrAlreadyPublishing) {
			r.m.Log.Info("pull source waiting: path is busy", "path", r.src.Path, "retry_in", delay)
		} else {
			r.m.Log.Warn("pull source failed", "path", r.src.Path, "url", r.redacted,
				"err", msg, "retry_in", delay)
		}

		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		r.m.mu.Lock()
		r.st.Reconnects++
		r.m.mu.Unlock()
	}
}

// pullOnce connects once and forwards media until the source fails or ctx
// is cancelled. The stream is unpublished before it returns.
func (r *runner) pullOnce(ctx context.Context) error {
	u, err := url.Parse(r.src.URL)
	if err != nil {
		return errors.New("invalid URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return r.pullHTTP(ctx, u)
	case "udp":
		return r.pullUDP(ctx, u)
	case "srt":
		return r.pullSRT(ctx, u)
	case "rtmp", "rtmps":
		return r.pullRTMP(ctx, u)
	}
	return errors.New("unsupported URL scheme " + u.Scheme)
}

// backoff yields 1s, 2s, 4s, ... capped at 30s.
type backoff struct{ next time.Duration }

// Next returns the delay to wait now and doubles the following one.
func (b *backoff) Next() time.Duration {
	if b.next < minBackoff {
		b.next = minBackoff
	}
	d := b.next
	b.next *= 2
	if b.next > maxBackoff {
		b.next = maxBackoff
	}
	return d
}

// Reset starts again from the minimum delay.
func (b *backoff) Reset() { b.next = 0 }

// RedactURL returns raw without user info, with every query parameter
// value (and any fragment) replaced by REDACTED, so it can be logged and
// shown in the API.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	u.User = nil
	if u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for i, p := range parts {
			if k, _, ok := strings.Cut(p, "="); ok {
				parts[i] = k + "=REDACTED"
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		u.Fragment, u.RawFragment = "REDACTED", ""
	}
	return u.String()
}

// sanitizeError renders err with every secret of raw (password, user
// name, query values, fragment and the full URL) replaced, because errors
// from net/http and other libraries often quote the URL.
func sanitizeError(err error, raw string) string {
	msg := err.Error()
	u, perr := url.Parse(raw)
	if perr != nil {
		return strings.ReplaceAll(msg, raw, "(URL)")
	}
	msg = strings.ReplaceAll(msg, raw, RedactURL(raw))

	var secrets []string
	if u.User != nil {
		secrets = append(secrets, u.User.Username())
		if p, ok := u.User.Password(); ok {
			secrets = append(secrets, p)
		}
	}
	for _, vals := range u.Query() {
		secrets = append(secrets, vals...)
	}
	secrets = append(secrets, u.Fragment)
	var all []string
	for _, s := range secrets {
		if len(s) < 3 {
			continue
		}
		all = append(all, s, url.QueryEscape(s), url.PathEscape(s))
	}
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	for _, s := range all {
		msg = strings.ReplaceAll(msg, s, "REDACTED")
	}
	return msg
}
