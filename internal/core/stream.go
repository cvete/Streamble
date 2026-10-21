package core

import (
	"sync"
	"sync/atomic"
	"time"
)

// Unit is one timestamped piece of media flowing through the hub.
//
// For video, Payload holds the NAL units of one access unit and DTS is set.
// For audio, Payload holds one or more consecutive audio frames (AAC access
// units or Opus packets) and PTS is the timestamp of the first one.
//
// Units are shared between all subscribers and must never be modified after
// being written to a stream.
type Unit struct {
	Track        *Track
	NTP          time.Time
	PTS          time.Duration
	DTS          time.Duration
	Payload      [][]byte
	RandomAccess bool // video keyframe (IDR / IRAP)
}

func (u *Unit) size() uint64 {
	var n uint64
	for _, p := range u.Payload {
		n += uint64(len(p))
	}
	return n
}

// SourceInfo describes who is publishing a stream.
type SourceInfo struct {
	Protocol   string // "rtmp", "srt", ...
	RemoteAddr string
}

// Stream is a live stream with exactly one publisher and any number of
// subscribers.
type Stream struct {
	Path      string
	Tracks    []*Track
	Source    SourceInfo
	StartedAt time.Time

	hasVideo bool

	mu     sync.RWMutex
	subs   map[*Subscription]struct{}
	closed bool

	bytesIn atomic.Uint64
	unitsIn atomic.Uint64
	// BytesOut is incremented by outputs (e.g. HLS HTTP responses).
	BytesOut atomic.Uint64
}

func newStream(path string, tracks []*Track, src SourceInfo) *Stream {
	s := &Stream{
		Path:      path,
		Tracks:    tracks,
		Source:    src,
		StartedAt: time.Now(),
		subs:      make(map[*Subscription]struct{}),
	}
	for _, t := range tracks {
		if t.Codec.IsVideo() {
			s.hasVideo = true
		}
	}
	return s
}

// HasVideo reports whether the stream contains a video track.
func (s *Stream) HasVideo() bool { return s.hasVideo }

// BytesIn returns the number of payload bytes received from the publisher.
func (s *Stream) BytesIn() uint64 { return s.bytesIn.Load() }

// UnitsIn returns the number of units received from the publisher.
func (s *Stream) UnitsIn() uint64 { return s.unitsIn.Load() }

// WriteUnit fans a unit out to all subscribers. It never blocks: a
// subscriber that cannot keep up drops units until the next keyframe.
//
// WriteUnit must be called from a single goroutine (the publisher's).
func (s *Stream) WriteUnit(u *Unit) {
	s.bytesIn.Add(u.size())
	s.unitsIn.Add(1)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	for sub := range s.subs {
		sub.push(u)
	}
}

// Subscribe registers a new subscriber with a queue of bufSize units.
// The subscription starts at the next keyframe if the stream has video, so
// outputs always begin with a decodable picture.
// It returns nil if the stream has already ended.
func (s *Stream) Subscribe(name string, bufSize int) *Subscription {
	if bufSize <= 0 {
		bufSize = 1024
	}
	sub := &Subscription{
		Name:   name,
		stream: s,
		c:      make(chan *Unit, bufSize),
	}
	sub.waitKey.Store(s.hasVideo)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.subs[sub] = struct{}{}
	return sub
}

// SubscriberCount returns the number of active subscribers.
func (s *Stream) SubscriberCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs)
}

func (s *Stream) unsubscribe(sub *Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		close(sub.c)
	}
}

// close ends the stream and closes every subscriber channel.
func (s *Stream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for sub := range s.subs {
		close(sub.c)
	}
	s.subs = nil
}

// Subscription is a consumer of a Stream.
type Subscription struct {
	Name   string
	stream *Stream
	c      chan *Unit

	waitKey atomic.Bool
	dropped atomic.Uint64
}

// C returns the channel of units. It is closed when the stream ends or the
// subscription is closed.
func (sub *Subscription) C() <-chan *Unit { return sub.c }

// Dropped returns how many units were dropped because the subscriber was
// too slow.
func (sub *Subscription) Dropped() uint64 { return sub.dropped.Load() }

// Close unregisters the subscription. It is safe to call more than once.
func (sub *Subscription) Close() { sub.stream.unsubscribe(sub) }

// push is called with the stream read lock held, from the publisher
// goroutine only.
func (sub *Subscription) push(u *Unit) {
	if sub.waitKey.Load() {
		if !(u.Track.Codec.IsVideo() && u.RandomAccess) {
			return
		}
		sub.waitKey.Store(false)
	}

	select {
	case sub.c <- u:
	default:
		// Queue full: drop and resynchronise on the next keyframe, so the
		// subscriber never receives a broken group of pictures.
		sub.dropped.Add(1)
		if sub.stream.hasVideo {
			sub.waitKey.Store(true)
		}
	}
}
