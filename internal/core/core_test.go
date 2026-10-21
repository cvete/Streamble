package core

import (
	"errors"
	"testing"
	"time"
)

type recordingHook struct {
	published, unpublished []string
}

func (h *recordingHook) OnPublish(s *Stream)   { h.published = append(h.published, s.Path) }
func (h *recordingHook) OnUnpublish(s *Stream) { h.unpublished = append(h.unpublished, s.Path) }

func videoTrack() *Track { return NewTrack(&H264{}) }

func TestHubPublishLifecycle(t *testing.T) {
	h := NewHub(nil)
	hook := &recordingHook{}
	h.AddHook(hook)

	s, err := h.Publish("live/a", []*Track{videoTrack()}, SourceInfo{Protocol: "rtmp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Publish("live/a", []*Track{videoTrack()}, SourceInfo{}); !errors.Is(err, ErrAlreadyPublishing) {
		t.Fatalf("expected ErrAlreadyPublishing, got %v", err)
	}
	if h.Lookup("live/a") != s {
		t.Fatal("lookup failed")
	}
	if n := len(h.Streams()); n != 1 {
		t.Fatalf("expected 1 stream, got %d", n)
	}

	h.Unpublish(s)
	h.Unpublish(s) // idempotent
	if h.Lookup("live/a") != nil {
		t.Fatal("stream still registered")
	}
	if len(hook.published) != 1 || len(hook.unpublished) != 1 {
		t.Fatalf("hook calls: %+v", hook)
	}

	// The path is free again.
	if _, err := h.Publish("live/a", []*Track{videoTrack()}, SourceInfo{}); err != nil {
		t.Fatal(err)
	}
}

func TestPublishRequiresTracks(t *testing.T) {
	if _, err := NewHub(nil).Publish("x", nil, SourceInfo{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestSubscriptionStartsAtKeyframe(t *testing.T) {
	h := NewHub(nil)
	vt := videoTrack()
	at := NewTrack(&Opus{ChannelCount: 2})
	s, _ := h.Publish("live/k", []*Track{vt, at}, SourceInfo{})
	sub := s.Subscribe("test", 16)

	s.WriteUnit(&Unit{Track: at, PTS: 1})
	s.WriteUnit(&Unit{Track: vt, PTS: 2})
	s.WriteUnit(&Unit{Track: vt, PTS: 3, RandomAccess: true})
	s.WriteUnit(&Unit{Track: at, PTS: 4})

	got := drain(sub, 2)
	if got[0].PTS != 3 || got[1].PTS != 4 {
		t.Fatalf("unexpected units: %v %v", got[0].PTS, got[1].PTS)
	}
}

func TestAudioOnlyStreamDoesNotWaitForKeyframe(t *testing.T) {
	h := NewHub(nil)
	at := NewTrack(&Opus{ChannelCount: 2})
	s, _ := h.Publish("radio/a", []*Track{at}, SourceInfo{})
	sub := s.Subscribe("test", 4)
	s.WriteUnit(&Unit{Track: at, PTS: 1})
	if got := drain(sub, 1); got[0].PTS != 1 {
		t.Fatal("audio unit not delivered")
	}
}

func TestSlowSubscriberResyncsOnKeyframe(t *testing.T) {
	h := NewHub(nil)
	vt := videoTrack()
	s, _ := h.Publish("live/slow", []*Track{vt}, SourceInfo{})
	sub := s.Subscribe("slow", 2)

	s.WriteUnit(&Unit{Track: vt, PTS: 1, RandomAccess: true})
	s.WriteUnit(&Unit{Track: vt, PTS: 2})
	s.WriteUnit(&Unit{Track: vt, PTS: 3}) // dropped: queue full
	s.WriteUnit(&Unit{Track: vt, PTS: 4}) // dropped: waiting for keyframe

	got := drain(sub, 2)
	if got[0].PTS != 1 || got[1].PTS != 2 {
		t.Fatal("unexpected head of queue")
	}
	s.WriteUnit(&Unit{Track: vt, PTS: 5})                     // still waiting
	s.WriteUnit(&Unit{Track: vt, PTS: 6, RandomAccess: true}) // resync
	if got := drain(sub, 1); got[0].PTS != 6 {
		t.Fatalf("expected resync at keyframe 6, got %v", got[0].PTS)
	}
	if sub.Dropped() != 1 {
		t.Fatalf("expected 1 drop, got %d", sub.Dropped())
	}
}

func TestUnpublishClosesSubscriptions(t *testing.T) {
	h := NewHub(nil)
	s, _ := h.Publish("live/c", []*Track{videoTrack()}, SourceInfo{})
	sub := s.Subscribe("a", 1)
	sub2 := s.Subscribe("b", 1)
	sub2.Close()
	sub2.Close() // idempotent
	h.Unpublish(s)

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed")
	}
	if s.Subscribe("late", 1) != nil {
		t.Fatal("subscribe after close should return nil")
	}
	sub.Close() // after stream close: must not panic
}

func drain(sub *Subscription, n int) []*Unit {
	out := make([]*Unit, 0, n)
	for len(out) < n {
		select {
		case u := <-sub.C():
			out = append(out, u)
		case <-time.After(time.Second):
			panic("timeout waiting for units")
		}
	}
	return out
}

var (
	sps = []byte{0x67, 1, 2}
	pps = []byte{0x68, 3}
	idr = []byte{0x65, 9}
	non = []byte{0x41, 9}
)

func TestParamKeeperH264(t *testing.T) {
	k := NewParamKeeper(&H264{})

	// Parameter-only access unit (RTMP sequence header) is absorbed.
	if au, _ := k.Process([][]byte{sps, pps}); au != nil {
		t.Fatal("parameter-only AU should be absorbed")
	}
	// Keyframe without parameters gets them prepended.
	au, key := k.Process([][]byte{idr})
	if !key || len(au) != 3 || au[0][0] != 0x67 || au[1][0] != 0x68 {
		t.Fatalf("expected SPS+PPS+IDR, got %d NALUs key=%v", len(au), key)
	}
	// Keyframe that already has them is untouched.
	if au, _ := k.Process([][]byte{sps, pps, idr}); len(au) != 3 {
		t.Fatal("duplicate parameter sets added")
	}
	// Non-keyframe passes through.
	if au, key := k.Process([][]byte{non}); key || len(au) != 1 {
		t.Fatal("non-IDR modified")
	}
}

func TestParamKeeperH265(t *testing.T) {
	vps := []byte{32 << 1, 1}
	hsps := []byte{33 << 1, 1}
	hpps := []byte{34 << 1, 1}
	irap := []byte{19 << 1, 1} // IDR_W_RADL
	trail := []byte{1 << 1, 1}

	k := NewParamKeeper(&H265{VPS: vps, SPS: hsps, PPS: hpps})
	au, key := k.Process([][]byte{irap})
	if !key || len(au) != 4 {
		t.Fatalf("expected VPS+SPS+PPS+IRAP, got %d", len(au))
	}
	if au, key := k.Process([][]byte{trail}); key || len(au) != 1 {
		t.Fatal("trailing picture modified")
	}
}

func TestClockRates(t *testing.T) {
	if NewTrack(&H264{}).ClockRate != 90000 || NewTrack(&Opus{}).ClockRate != 48000 {
		t.Fatal("unexpected clock rate")
	}
}
