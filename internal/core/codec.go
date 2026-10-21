// Package core contains the stream hub: the in-memory registry of live
// streams and the fan-out of media units from one publisher to many outputs.
//
// Ingest modules (RTMP, SRT, ...) publish streams into the hub. Output
// modules (HLS, restreaming, recording, ...) subscribe to them. Neither side
// knows about the other; they only share the types defined in this package.
package core

import (
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
)

// Codec describes the codec of a track. Implementations are plain value
// holders and must be treated as immutable once a stream is published.
type Codec interface {
	// Name returns a short human readable codec name, e.g. "H264".
	Name() string
	// IsVideo reports whether the codec is a video codec.
	IsVideo() bool
}

// H264 is the H.264 / AVC video codec.
// SPS and PPS may be nil if they were not known when the stream was
// published; in that case they are carried in-band in the access units.
type H264 struct {
	SPS []byte
	PPS []byte
}

// Name implements Codec.
func (*H264) Name() string { return "H264" }

// IsVideo implements Codec.
func (*H264) IsVideo() bool { return true }

// H265 is the H.265 / HEVC video codec.
type H265 struct {
	VPS []byte
	SPS []byte
	PPS []byte
}

// Name implements Codec.
func (*H265) Name() string { return "H265" }

// IsVideo implements Codec.
func (*H265) IsVideo() bool { return true }

// AAC is the MPEG-4 Audio (AAC) codec.
type AAC struct {
	Config *mpeg4audio.AudioSpecificConfig
}

// Name implements Codec.
func (*AAC) Name() string { return "AAC" }

// IsVideo implements Codec.
func (*AAC) IsVideo() bool { return false }

// Opus is the Opus audio codec.
type Opus struct {
	ChannelCount int
}

// Name implements Codec.
func (*Opus) Name() string { return "Opus" }

// IsVideo implements Codec.
func (*Opus) IsVideo() bool { return false }

// Track is one elementary stream (video or audio) inside a Stream.
type Track struct {
	// Codec of the track.
	Codec Codec
	// ClockRate is the timestamp resolution expected by outputs:
	// 90000 for video, the sample rate for AAC, 48000 for Opus.
	ClockRate int
}

// NewTrack creates a track with the conventional clock rate for its codec.
func NewTrack(c Codec) *Track {
	return &Track{Codec: c, ClockRate: clockRateFor(c)}
}

func clockRateFor(c Codec) int {
	switch c := c.(type) {
	case *AAC:
		if c.Config != nil && c.Config.SampleRate > 0 {
			return c.Config.SampleRate
		}
		return 48000
	case *Opus:
		return 48000
	default:
		return 90000
	}
}
