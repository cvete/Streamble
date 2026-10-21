package core

import (
	"errors"
	"log/slog"
	"sort"
	"sync"
)

// ErrAlreadyPublishing is returned when a stream path is already in use.
var ErrAlreadyPublishing = errors.New("stream is already being published")

// Hook is notified when streams start and stop. Outputs such as the HLS
// muxer register a hook and subscribe to new streams from OnPublish.
// Hooks are called synchronously and must not block; long-running work
// belongs in a goroutine.
type Hook interface {
	OnPublish(s *Stream)
	OnUnpublish(s *Stream)
}

// Hub is the registry of live streams.
type Hub struct {
	log *slog.Logger

	mu      sync.RWMutex
	streams map[string]*Stream
	hooks   []Hook
}

// NewHub creates an empty hub.
func NewHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{log: log, streams: make(map[string]*Stream)}
}

// AddHook registers a hook. Hooks must be added before the first publish.
func (h *Hub) AddHook(hook Hook) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hooks = append(h.hooks, hook)
}

// Publish creates a new stream at path.
func (h *Hub) Publish(path string, tracks []*Track, src SourceInfo) (*Stream, error) {
	if len(tracks) == 0 {
		return nil, errors.New("stream has no supported tracks")
	}

	h.mu.Lock()
	if _, ok := h.streams[path]; ok {
		h.mu.Unlock()
		return nil, ErrAlreadyPublishing
	}
	s := newStream(path, tracks, src)
	h.streams[path] = s
	hooks := append([]Hook(nil), h.hooks...)
	h.mu.Unlock()

	attrs := []any{"path", path, "protocol", src.Protocol, "remote", src.RemoteAddr}
	for _, t := range tracks {
		attrs = append(attrs, "track", t.Codec.Name())
	}
	h.log.Info("stream published", attrs...)

	for _, hook := range hooks {
		hook.OnPublish(s)
	}
	return s, nil
}

// Unpublish ends a stream. It is safe to call more than once.
func (h *Hub) Unpublish(s *Stream) {
	h.mu.Lock()
	cur, ok := h.streams[s.Path]
	if !ok || cur != s {
		h.mu.Unlock()
		return
	}
	delete(h.streams, s.Path)
	hooks := append([]Hook(nil), h.hooks...)
	h.mu.Unlock()

	s.close()
	h.log.Info("stream ended", "path", s.Path, "bytes_in", s.BytesIn())

	for _, hook := range hooks {
		hook.OnUnpublish(s)
	}
}

// Lookup returns the live stream at path, or nil.
func (h *Hub) Lookup(path string) *Stream {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.streams[path]
}

// Streams returns all live streams sorted by path.
func (h *Hub) Streams() []*Stream {
	h.mu.RLock()
	out := make([]*Stream, 0, len(h.streams))
	for _, s := range h.streams {
		out = append(out, s)
	}
	h.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
