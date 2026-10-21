// Package tsconv converts between MPEG-TS byte streams and hub units.
//
// Demux reads MPEG-TS (from SRT, UDP, HTTP, an FFmpeg pipe, ...) and
// publishes it into the hub. Muxer writes hub units as MPEG-TS (for HTTP
// .ts output, SRT playback, SRT restreaming, feeding FFmpeg, ...).
package tsconv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"

	"github.com/cvete/streamble/internal/core"
)

// TicksToDuration converts 90 kHz MPEG-TS ticks.
func TicksToDuration(t int64) time.Duration {
	return time.Duration(t) * time.Second / 90000
}

// DurationToTicks converts to 90 kHz MPEG-TS ticks without overflowing.
func DurationToTicks(d time.Duration) int64 {
	secs := int64(d / time.Second)
	rem := int64(d % time.Second)
	return secs*90000 + rem*90000/int64(time.Second)
}

// Demux reads MPEG-TS from r, publishes it at path, and forwards media
// until r returns an error. The stream is unpublished before returning.
// Tracks with codecs the hub does not support are skipped with a warning.
func Demux(r io.Reader, hub *core.Hub, path string, src core.SourceInfo, log *slog.Logger) error {
	tr := &mpegts.Reader{R: mpegts.NewBufferedReader(r)}
	if err := tr.Initialize(); err != nil {
		return fmt.Errorf("reading MPEG-TS: %w", err)
	}
	tr.OnDecodeError(func(err error) {
		log.Debug("MPEG-TS decode error", "path", path, "err", err)
	})

	var stream *core.Stream
	var tracks []*core.Track
	td := &mpegts.TimeDecoder{}
	td.Initialize()

	for _, mt := range tr.Tracks() {
		if t := attach(tr, mt, td, &stream); t != nil {
			tracks = append(tracks, t)
		} else {
			log.Warn("MPEG-TS track ignored: codec not supported", "path", path,
				"codec", fmt.Sprintf("%T", mt.Codec))
		}
	}

	stream, err := hub.Publish(path, tracks, src)
	if err != nil {
		return err
	}
	defer hub.Unpublish(stream)

	for {
		if err := tr.Read(); err != nil {
			return err
		}
	}
}

func attach(r *mpegts.Reader, mt *mpegts.Track, td *mpegts.TimeDecoder, stream **core.Stream) *core.Track {
	audio := func(t *core.Track) func(pts int64, frames [][]byte) error {
		return func(pts int64, frames [][]byte) error {
			(*stream).WriteUnit(&core.Unit{
				Track: t, NTP: time.Now(), PTS: TicksToDuration(td.Decode(pts)), Payload: frames,
			})
			return nil
		}
	}

	switch c := mt.Codec.(type) {
	case *mpegts.CodecH264:
		t := core.NewTrack(&core.H264{})
		k := core.NewParamKeeper(t.Codec)
		r.OnDataH264(mt, func(pts, dts int64, au [][]byte) error {
			writeVideo(*stream, t, k, td.Decode(pts), td.Decode(dts), au)
			return nil
		})
		return t

	case *mpegts.CodecH265:
		t := core.NewTrack(&core.H265{})
		k := core.NewParamKeeper(t.Codec)
		r.OnDataH265(mt, func(pts, dts int64, au [][]byte) error {
			writeVideo(*stream, t, k, td.Decode(pts), td.Decode(dts), au)
			return nil
		})
		return t

	case *mpegts.CodecMPEG4Audio:
		cfg := c.Config
		t := core.NewTrack(&core.AAC{Config: &cfg})
		r.OnDataMPEG4Audio(mt, audio(t))
		return t

	case *mpegts.CodecOpus:
		ch := 2
		if c.Desc != nil && c.Desc.ExplicitChannelCount > 0 {
			ch = c.Desc.ExplicitChannelCount
		} else if c.ChannelCount > 0 { //nolint:staticcheck
			ch = c.ChannelCount //nolint:staticcheck
		}
		t := core.NewTrack(&core.Opus{ChannelCount: ch})
		r.OnDataOpus(mt, audio(t))
		return t

	case *mpegts.CodecMPEG1Audio:
		t := core.NewTrack(&core.MPEGAudio{})
		r.OnDataMPEG1Audio(mt, audio(t))
		return t

	case *mpegts.CodecAC3:
		t := core.NewTrack(&core.AC3{SampleRate: c.SampleRate, ChannelCount: c.ChannelCount})
		r.OnDataAC3(mt, func(pts int64, frame []byte) error {
			(*stream).WriteUnit(&core.Unit{
				Track: t, NTP: time.Now(), PTS: TicksToDuration(td.Decode(pts)), Payload: [][]byte{frame},
			})
			return nil
		})
		return t
	}
	return nil
}

func writeVideo(stream *core.Stream, t *core.Track, k *core.ParamKeeper, pts, dts int64, au [][]byte) {
	au, key := k.Process(au)
	if au == nil {
		return
	}
	stream.WriteUnit(&core.Unit{
		Track:        t,
		NTP:          time.Now(),
		PTS:          TicksToDuration(pts),
		DTS:          TicksToDuration(dts),
		Payload:      au,
		RandomAccess: key,
	})
}

// ErrNoTracks is returned by NewMuxer when no track can be carried in
// MPEG-TS.
var ErrNoTracks = errors.New("no tracks can be carried in MPEG-TS")

// Muxer writes hub units as an MPEG-TS byte stream.
type Muxer struct {
	bw     *bufio.Writer
	w      *mpegts.Writer
	tracks map[*core.Track]*mpegts.Track
}

// NewMuxer creates a muxer for the given tracks. Every codec the hub knows
// can be carried in MPEG-TS; AAC tracks without a config are skipped.
func NewMuxer(w io.Writer, tracks []*core.Track) (*Muxer, error) {
	m := &Muxer{
		bw:     bufio.NewWriterSize(w, 64*1024),
		tracks: make(map[*core.Track]*mpegts.Track),
	}
	var tsTracks []*mpegts.Track
	for _, t := range tracks {
		var c mpegts.Codec
		switch tc := t.Codec.(type) {
		case *core.H264:
			c = &mpegts.CodecH264{}
		case *core.H265:
			c = &mpegts.CodecH265{}
		case *core.AAC:
			if tc.Config == nil {
				continue
			}
			c = &mpegts.CodecMPEG4Audio{Config: *tc.Config}
		case *core.Opus:
			c = &mpegts.CodecOpus{ChannelCount: tc.ChannelCount} //nolint:staticcheck
		case *core.MPEGAudio:
			c = &mpegts.CodecMPEG1Audio{}
		case *core.AC3:
			c = &mpegts.CodecAC3{SampleRate: tc.SampleRate, ChannelCount: tc.ChannelCount}
		default:
			continue
		}
		tt := &mpegts.Track{Codec: c}
		m.tracks[t] = tt
		tsTracks = append(tsTracks, tt)
	}
	if len(tsTracks) == 0 {
		return nil, ErrNoTracks
	}
	m.w = &mpegts.Writer{W: m.bw, Tracks: tsTracks}
	if err := m.w.Initialize(); err != nil {
		return nil, err
	}
	return m, nil
}

// WriteUnit writes one unit and flushes it to the underlying writer.
// Units of tracks the muxer does not carry are ignored.
func (m *Muxer) WriteUnit(u *core.Unit) error {
	tt := m.tracks[u.Track]
	if tt == nil || len(u.Payload) == 0 {
		return nil
	}
	pts := DurationToTicks(u.PTS)

	var err error
	switch u.Track.Codec.(type) {
	case *core.H264:
		err = m.w.WriteH264(tt, pts, DurationToTicks(u.DTS), u.Payload)
	case *core.H265:
		err = m.w.WriteH265(tt, pts, DurationToTicks(u.DTS), u.Payload)
	case *core.AAC:
		err = m.w.WriteMPEG4Audio(tt, pts, u.Payload)
	case *core.Opus:
		err = m.w.WriteOpus(tt, pts, u.Payload)
	case *core.MPEGAudio:
		err = m.w.WriteMPEG1Audio(tt, pts, u.Payload)
	case *core.AC3:
		for _, f := range u.Payload {
			if err = m.w.WriteAC3(tt, pts, f); err != nil {
				break
			}
		}
	}
	if err != nil {
		return err
	}
	return m.bw.Flush()
}

// SRTPayloadSize is the conventional SRT payload for MPEG-TS: 7 TS packets.
const SRTPayloadSize = 7 * 188

// PacketWriter splits writes into fixed-size chunks, so every SRT (or UDP)
// packet carries whole MPEG-TS packets. Strict receivers require this.
type PacketWriter struct {
	W    io.Writer
	Size int
}

// Write implements io.Writer.
func (p *PacketWriter) Write(b []byte) (int, error) {
	size := p.Size
	if size <= 0 {
		size = SRTPayloadSize
	}
	n := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > size {
			chunk = chunk[:size]
		}
		w, err := p.W.Write(chunk)
		n += w
		if err != nil {
			return n, err
		}
		b = b[len(chunk):]
	}
	return n, nil
}

// TimeoutReader extends a connection's read deadline before every read, so
// a silent source fails instead of hanging forever.
type TimeoutReader struct {
	Conn    interface{ SetReadDeadline(time.Time) error }
	R       io.Reader
	Timeout time.Duration
}

// Read implements io.Reader.
func (t *TimeoutReader) Read(p []byte) (int, error) {
	t.Conn.SetReadDeadline(time.Now().Add(t.Timeout)) //nolint:errcheck
	return t.R.Read(p)
}

// TimeoutConn is a convenience for net.Conn sources.
func TimeoutConn(c net.Conn, timeout time.Duration) io.Reader {
	return &TimeoutReader{Conn: c, R: c, Timeout: timeout}
}
