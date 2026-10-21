// Package events distributes server events (stream published or ended,
// restream/pull/transcode state changes) to webhooks and to the panel's
// live event stream.
package events

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/cvete/streamble/internal/core"
)

// Event is one thing that happened.
type Event struct {
	Type string         `json:"type"` // a config.Event* name
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data"`
}

// Bus fans events out to subscribers. Publish never blocks: each subscriber
// has its own queue, and a subscriber that falls behind loses events (and
// learns how many through Dropped).
type Bus struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewBus creates a bus.
func NewBus() *Bus { return &Bus{subs: make(map[*Subscription]struct{})} }

// Subscription receives events.
type Subscription struct {
	bus     *Bus
	c       chan Event
	mu      sync.Mutex
	dropped int
	closed  bool
}

// Subscribe registers a subscriber with a queue of size n.
func (b *Bus) Subscribe(n int) *Subscription {
	if n <= 0 {
		n = 256
	}
	s := &Subscription{bus: b, c: make(chan Event, n)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// C returns the event channel; it is closed by Close.
func (s *Subscription) C() <-chan Event { return s.c }

// Dropped returns and resets the number of events lost to a full queue.
func (s *Subscription) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.dropped
	s.dropped = 0
	return n
}

// Close unsubscribes. Safe to call more than once.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.c)
	}
}

// Publish sends an event to every subscriber. At is set when zero.
func (b *Bus) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		s.mu.Lock()
		if !s.closed {
			select {
			case s.c <- e:
			default:
				s.dropped++
			}
		}
		s.mu.Unlock()
	}
}

// StreamHook publishes stream.published and stream.ended. Add it to the hub.
type StreamHook struct{ Bus *Bus }

// OnPublish implements core.Hook.
func (h StreamHook) OnPublish(s *core.Stream) {
	var tracks []string
	for _, t := range s.Tracks {
		tracks = append(tracks, t.Codec.Name())
	}
	h.Bus.Publish(Event{Type: "stream.published", Data: map[string]any{
		"path":        s.Path,
		"protocol":    s.Source.Protocol,
		"remote_addr": s.Source.RemoteAddr,
		"tracks":      tracks,
	}})
}

// OnUnpublish implements core.Hook.
func (h StreamHook) OnUnpublish(s *core.Stream) {
	h.Bus.Publish(Event{Type: "stream.ended", Data: map[string]any{
		"path":             s.Path,
		"protocol":         s.Source.Protocol,
		"duration_seconds": int64(time.Since(s.StartedAt).Seconds()),
		"bytes_in":         s.BytesIn(),
		"bytes_out":        s.BytesOut.Load(),
	}})
}

// Data converts a status struct into event data using its JSON field names.
func Data(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	json.Unmarshal(b, &out) //nolint:errcheck
	return out
}
