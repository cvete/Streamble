package tsconv

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/cvete/streamble/internal/core"
)

// mp3Frame returns a valid MPEG-1 Layer III frame (128 kbit/s, 44.1 kHz).
func mp3Frame(t *testing.T) []byte {
	hdr := []byte{0xFF, 0xFB, 0x90, 0x64, 0x00}
	var h mpeg1audio.FrameHeader
	if err := h.Unmarshal(hdr); err != nil {
		t.Fatal(err)
	}
	f := make([]byte, h.FrameLen())
	copy(f, hdr)
	return f
}

func TestRoundTrip(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	vt := core.NewTrack(&core.H264{})
	at := core.NewTrack(&core.AAC{Config: &mpeg4audio.AudioSpecificConfig{
		Type: mpeg4audio.ObjectTypeAACLC, SampleRate: 48000, ChannelCount: 2,
	}})
	mt := core.NewTrack(&core.MPEGAudio{})
	ct := core.NewTrack(&core.AC3{SampleRate: 48000, ChannelCount: 2})

	pr, pw := io.Pipe()
	mux, err := NewMuxer(pw, []*core.Track{vt, at, mt, ct})
	if err != nil {
		t.Fatal(err)
	}

	hub := core.NewHub(log)
	gotTracks := make(chan []*core.Track, 1)
	units := make(chan *core.Unit, 1000)
	forwarded := make(chan struct{})
	hub.AddHook(hookFunc(func(s *core.Stream) {
		gotTracks <- s.Tracks
		sub := s.Subscribe("test", 1000)
		go func() {
			defer close(forwarded)
			for u := range sub.C() {
				units <- u
			}
		}()
	}))

	done := make(chan error, 1)
	go func() { done <- Demux(pr, hub, "live/rt", core.SourceInfo{Protocol: "test"}, log) }()

	sps := []byte{0x67, 0x42, 0xc0, 0x1f, 0xd9, 0x00, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20}
	pps := []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
	ac3Frame := make([]byte, 768)
	// syncword, CRC, fscod=48 kHz + frmsizecod=20 (192 kbit/s = 768 bytes), bsid=8
	copy(ac3Frame, []byte{0x0b, 0x77, 0x00, 0x00, 0x14, 0x40})
	mp3 := mp3Frame(t)

	go func() {
		for i := 0; i < 50; i++ {
			pts := time.Duration(i) * 40 * time.Millisecond
			key := i%25 == 0
			au := [][]byte{{0x41, 0x9a, byte(i)}}
			if key {
				au = [][]byte{sps, pps, {0x65, 0x88, byte(i)}}
			}
			mux.WriteUnit(&core.Unit{Track: vt, PTS: pts, DTS: pts, Payload: au, RandomAccess: key})       //nolint:errcheck
			mux.WriteUnit(&core.Unit{Track: at, PTS: pts, Payload: [][]byte{{0x21, 0x10, 0x04, byte(i)}}}) //nolint:errcheck
			mux.WriteUnit(&core.Unit{Track: mt, PTS: pts, Payload: [][]byte{mp3}})                         //nolint:errcheck
			mux.WriteUnit(&core.Unit{Track: ct, PTS: pts, Payload: [][]byte{ac3Frame}})                    //nolint:errcheck
		}
		pw.Close()
	}()

	select {
	case tracks := <-gotTracks:
		names := map[string]bool{}
		for _, tr := range tracks {
			names[tr.Codec.Name()] = true
		}
		for _, want := range []string{"H264", "AAC", "MP2/MP3", "AC3"} {
			if !names[want] {
				t.Errorf("track %s lost in round trip (got %v)", want, names)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream never published")
	}

	<-done
	<-forwarded
	close(units)
	counts := map[string]int{}
	for u := range units {
		counts[u.Track.Codec.Name()]++
	}
	// The demuxer emits a unit when the next one starts, so the tail may be
	// short by one; everything else must arrive.
	for _, name := range []string{"H264", "AAC", "MP2/MP3", "AC3"} {
		if counts[name] < 45 {
			t.Errorf("%s: only %d of 50 units arrived", name, counts[name])
		}
	}
}

func TestNewMuxerNoTracks(t *testing.T) {
	if _, err := NewMuxer(io.Discard, []*core.Track{core.NewTrack(&core.AAC{})}); err != ErrNoTracks {
		t.Fatalf("expected ErrNoTracks, got %v", err)
	}
}

func TestTickConversion(t *testing.T) {
	if DurationToTicks(time.Second) != 90000 || TicksToDuration(45000) != 500*time.Millisecond {
		t.Fatal("bad conversion")
	}
}

type hookFunc func(*core.Stream)

func (h hookFunc) OnPublish(s *core.Stream) { h(s) }
func (hookFunc) OnUnpublish(*core.Stream)   {}
