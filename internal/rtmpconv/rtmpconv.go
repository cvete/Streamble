// Package rtmpconv sends hub streams over RTMP connections. It is shared by
// RTMP playback (server side) and RTMP restreaming (client side).
package rtmpconv

import (
	"errors"
	"io"
	"time"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"

	"github.com/cvete/streamble/internal/core"
)

// ErrNoTracks is returned when no track of the stream can be sent over RTMP.
var ErrNoTracks = errors.New("no tracks can be sent over RTMP")

// Send forwards a stream to conn until the subscription ends (returns
// io.EOF) or a write fails.
//
// RTMP needs video parameter sets in the stream header, but streams from
// SRT or MPEG-TS sources only carry them in-band. Since a subscription
// always starts at a keyframe, and ParamKeeper puts parameter sets in front
// of every keyframe, Send takes them from the first video unit.
func Send(conn gortmplib.Conn, s *core.Stream, sub *core.Subscription) error {
	first, ok := <-sub.C()
	if !ok {
		return io.EOF
	}

	tracks := make(map[*core.Track]*gortmplib.Track)
	var list []*gortmplib.Track
	for _, t := range s.Tracks {
		var unit *core.Unit
		if first.Track == t {
			unit = first
		}
		rt := toRTMPTrack(t, unit)
		if rt == nil {
			continue
		}
		tracks[t] = rt
		list = append(list, rt)
	}
	if len(list) == 0 {
		return ErrNoTracks
	}

	w := &gortmplib.Writer{Conn: conn, Tracks: list}
	if err := w.Initialize(); err != nil {
		return err
	}

	if err := write(w, tracks, first); err != nil {
		return err
	}
	for u := range sub.C() {
		if err := write(w, tracks, u); err != nil {
			return err
		}
	}
	return io.EOF
}

func toRTMPTrack(t *core.Track, first *core.Unit) *gortmplib.Track {
	switch c := t.Codec.(type) {
	case *core.H264:
		sps, pps := c.SPS, c.PPS
		if first != nil {
			for _, n := range first.Payload {
				if len(n) == 0 {
					continue
				}
				switch h264.NALUType(n[0] & 0x1f) {
				case h264.NALUTypeSPS:
					sps = n
				case h264.NALUTypePPS:
					pps = n
				}
			}
		}
		if sps == nil || pps == nil {
			return nil
		}
		return &gortmplib.Track{Codec: &codecs.H264{SPS: sps, PPS: pps}}

	case *core.H265:
		vps, sps, pps := c.VPS, c.SPS, c.PPS
		if first != nil {
			for _, n := range first.Payload {
				if len(n) < 2 {
					continue
				}
				switch h265.NALUType((n[0] >> 1) & 0x3f) {
				case h265.NALUType_VPS_NUT:
					vps = n
				case h265.NALUType_SPS_NUT:
					sps = n
				case h265.NALUType_PPS_NUT:
					pps = n
				}
			}
		}
		if vps == nil || sps == nil || pps == nil {
			return nil
		}
		return &gortmplib.Track{Codec: &codecs.H265{VPS: vps, SPS: sps, PPS: pps}}

	case *core.AAC:
		if c.Config == nil {
			return nil
		}
		return &gortmplib.Track{Codec: &codecs.MPEG4Audio{Config: c.Config}}

	case *core.MPEGAudio:
		return &gortmplib.Track{Codec: &codecs.MPEG1Audio{}}

	case *core.AC3:
		return &gortmplib.Track{Codec: &codecs.AC3{SampleRate: c.SampleRate, ChannelCount: c.ChannelCount}}

	case *core.Opus:
		family := uint8(0)
		if c.ChannelCount > 2 {
			family = 1
		}
		return &gortmplib.Track{Codec: &codecs.Opus{IDHeader: &opus.IDHeader{
			Version:              1,
			ChannelCount:         uint8(c.ChannelCount),
			PreSkip:              312,
			InputSampleRate:      48000,
			ChannelMappingFamily: family,
		}}}
	}
	return nil
}

// write sends one unit. Audio units may hold several frames; RTMP carries
// one frame per message, so each frame gets its own timestamp.
func write(w *gortmplib.Writer, tracks map[*core.Track]*gortmplib.Track, u *core.Unit) error {
	rt := tracks[u.Track]
	if rt == nil {
		return nil
	}

	switch c := u.Track.Codec.(type) {
	case *core.H264:
		return w.WriteH264(rt, u.PTS, u.DTS, u.Payload)

	case *core.H265:
		return w.WriteH265(rt, u.PTS, u.DTS, u.Payload)

	case *core.AAC:
		frameDur := time.Duration(mpeg4audio.SamplesPerAccessUnit) * time.Second / time.Duration(c.Config.SampleRate)
		for i, au := range u.Payload {
			if err := w.WriteMPEG4Audio(rt, u.PTS+time.Duration(i)*frameDur, au); err != nil {
				return err
			}
		}

	case *core.MPEGAudio:
		pts := u.PTS
		for _, f := range u.Payload {
			if err := w.WriteMPEG1Audio(rt, pts, f); err != nil {
				return err
			}
			var h mpeg1audio.FrameHeader
			if h.Unmarshal(f) == nil && h.SampleRate > 0 {
				pts += time.Duration(h.SampleCount()) * time.Second / time.Duration(h.SampleRate)
			}
		}

	case *core.AC3:
		const samplesPerFrame = 1536
		rate := c.SampleRate
		if rate <= 0 {
			rate = 48000
		}
		for i, f := range u.Payload {
			pts := u.PTS + time.Duration(i*samplesPerFrame)*time.Second/time.Duration(rate)
			if err := w.WriteAC3(rt, pts, f); err != nil {
				return err
			}
		}

	case *core.Opus:
		pts := u.PTS
		for _, p := range u.Payload {
			if err := w.WriteOpus(rt, pts, p); err != nil {
				return err
			}
			pts += opus.PacketDuration(p)
		}
	}
	return nil
}
