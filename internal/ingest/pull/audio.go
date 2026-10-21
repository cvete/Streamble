package pull

import (
	"bufio"
	"errors"
	"io"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/cvete/streamble/internal/core"
)

// maxResync is how many bytes of garbage a parser skips before giving up:
// a source that sends that much without a valid frame is not audio.
const maxResync = 1 << 20

var errNoSync = errors.New("no valid audio frame found")

// mp3Parser reads MPEG-1/2 Audio Layer II/III frames from a byte stream
// (an Icecast/Shoutcast body). It skips a leading ID3v2 tag and
// resynchronises byte by byte after garbage.
type mp3Parser struct {
	br      *bufio.Reader
	started bool // past the optional leading ID3v2 tag
	synced  bool // the previous frame was valid, so the next one should follow
}

func newMP3Parser(r io.Reader) *mp3Parser {
	return &mp3Parser{br: bufio.NewReaderSize(r, 64*1024)}
}

// mpegFrameLen returns the length in bytes of the frame described by h.
// mpeg1audio.FrameHeader.FrameLen uses the MPEG-1 formula; MPEG-2 Layer III
// frames carry half as many samples and are half as long.
func mpegFrameLen(h *mpeg1audio.FrameHeader) int {
	coef := 144
	if h.MPEG2 && h.Layer == 3 {
		coef = 72
	}
	n := coef * h.Bitrate / h.SampleRate
	if h.Padding {
		n++
	}
	return n
}

// parseMPEGHeader validates the 4-byte header at the start of buf.
func parseMPEGHeader(buf []byte) (mpeg1audio.FrameHeader, int, bool) {
	var h mpeg1audio.FrameHeader
	if len(buf) < 4 || buf[0] != 0xFF || buf[1]&0xE0 != 0xE0 {
		return h, 0, false
	}
	// FrameHeader.Unmarshal wants 5 bytes but only looks at 4.
	var tmp [5]byte
	copy(tmp[:], buf[:4])
	if err := h.Unmarshal(tmp[:]); err != nil {
		return h, 0, false
	}
	n := mpegFrameLen(&h)
	if n < 4 {
		return h, 0, false
	}
	return h, n, true
}

// skipID3 skips an ID3v2 tag at the current position, if there is one.
func skipID3(br *bufio.Reader) error {
	hdr, err := br.Peek(10)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, bufio.ErrBufferFull) {
			return nil // too short for a tag; let the frame parser decide
		}
		return err
	}
	if hdr[0] != 'I' || hdr[1] != 'D' || hdr[2] != '3' ||
		hdr[6]&0x80 != 0 || hdr[7]&0x80 != 0 || hdr[8]&0x80 != 0 || hdr[9]&0x80 != 0 {
		return nil
	}
	size := int64(hdr[6])<<21 | int64(hdr[7])<<14 | int64(hdr[8])<<7 | int64(hdr[9])
	size += 10
	if hdr[5]&0x10 != 0 { // footer present
		size += 10
	}
	_, err = io.CopyN(io.Discard, br, size)
	return err
}

// Next returns the next complete frame (header included).
func (p *mp3Parser) Next() ([]byte, mpeg1audio.FrameHeader, error) {
	if !p.started {
		p.started = true
		if err := skipID3(p.br); err != nil {
			return nil, mpeg1audio.FrameHeader{}, err
		}
		// The start of the stream is a frame boundary: trust it.
		p.synced = true
	}
	skipped := 0
	for {
		buf, err := p.br.Peek(4)
		if err != nil {
			return nil, mpeg1audio.FrameHeader{}, err
		}
		h, n, ok := parseMPEGHeader(buf)
		if ok && !p.synced {
			// After garbage, a lone 0xFFE pattern is easily a false
			// positive: also require the next frame header to be valid.
			next, err := p.br.Peek(n + 4)
			switch {
			case err == nil:
				_, _, ok = parseMPEGHeader(next[n:])
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				// end of stream: accept the last frame as is
			default:
				return nil, h, err
			}
		}
		if ok {
			frame := make([]byte, n)
			if _, err := io.ReadFull(p.br, frame); err != nil {
				return nil, h, err
			}
			p.synced = true
			return frame, h, nil
		}
		p.synced = false
		if _, err := p.br.Discard(1); err != nil {
			return nil, h, err
		}
		if skipped++; skipped > maxResync {
			return nil, h, errNoSync
		}
	}
}

// adtsFrame is one AAC access unit from an ADTS stream.
type adtsFrame struct {
	Config mpeg4audio.AudioSpecificConfig
	AU     []byte
}

// adtsParser reads AAC access units from an ADTS byte stream, resyncing on
// the 0xFFF syncword after garbage.
type adtsParser struct {
	br      *bufio.Reader
	started bool
	synced  bool
}

func newADTSParser(r io.Reader) *adtsParser {
	return &adtsParser{br: bufio.NewReaderSize(r, 64*1024)}
}

// adtsSampleRates maps sampling_frequency_index to Hz (ISO 14496-3).
var adtsSampleRates = [...]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350,
}

// parseADTSHeader validates the ADTS header at the start of buf (at least 7
// bytes) and returns the config, the header length and the frame length.
func parseADTSHeader(buf []byte) (mpeg4audio.AudioSpecificConfig, int, int, bool) {
	var c mpeg4audio.AudioSpecificConfig
	if len(buf) < 7 || buf[0] != 0xFF || buf[1]&0xF0 != 0xF0 {
		return c, 0, 0, false
	}
	if buf[1]&0x06 != 0 { // layer must be 0
		return c, 0, 0, false
	}
	hdrLen := 7
	if buf[1]&0x01 == 0 { // protection_absent == 0: 16-bit CRC follows
		hdrLen = 9
	}
	sri := (buf[2] >> 2) & 0x0F
	if int(sri) >= len(adtsSampleRates) {
		return c, 0, 0, false
	}
	chConf := (buf[2]&0x01)<<2 | buf[3]>>6
	frameLen := int(buf[3]&0x03)<<11 | int(buf[4])<<3 | int(buf[5])>>5
	if frameLen <= hdrLen || frameLen-hdrLen > mpeg4audio.MaxAccessUnitSize {
		return c, 0, 0, false
	}
	if buf[6]&0x03 != 0 { // more than one raw data block per frame
		return c, 0, 0, false
	}
	c.Type = mpeg4audio.ObjectType(buf[2]>>6 + 1)
	c.SampleRate = adtsSampleRates[sri]
	c.ChannelConfig = chConf
	switch {
	case chConf >= 1 && chConf <= 6:
		c.ChannelCount = int(chConf) //nolint:staticcheck
	case chConf == 7:
		c.ChannelCount = 8 //nolint:staticcheck
	}
	return c, hdrLen, frameLen, true
}

// Next returns the next access unit and the config from its header.
func (p *adtsParser) Next() (*adtsFrame, error) {
	if !p.started {
		p.started = true
		if err := skipID3(p.br); err != nil {
			return nil, err
		}
		p.synced = true
	}
	skipped := 0
	for {
		buf, err := p.br.Peek(7)
		if err != nil {
			return nil, err
		}
		cfg, hdrLen, frameLen, ok := parseADTSHeader(buf)
		if ok && !p.synced {
			next, err := p.br.Peek(frameLen + 7)
			switch {
			case err == nil:
				_, _, _, ok = parseADTSHeader(next[frameLen:])
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			default:
				return nil, err
			}
		}
		if ok {
			frame := make([]byte, frameLen)
			if _, err := io.ReadFull(p.br, frame); err != nil {
				return nil, err
			}
			p.synced = true
			return &adtsFrame{Config: cfg, AU: frame[hdrLen:]}, nil
		}
		p.synced = false
		if _, err := p.br.Discard(1); err != nil {
			return nil, err
		}
		if skipped++; skipped > maxResync {
			return nil, errNoSync
		}
	}
}

// sampleClock turns a running sample count into timestamps, tolerating
// sample rate changes.
type sampleClock struct {
	base    time.Duration
	samples int64
	rate    int
}

// Advance returns the timestamp of a frame of n samples at rate and moves
// past it.
func (c *sampleClock) Advance(n, rate int) time.Duration {
	if rate != c.rate {
		if c.rate > 0 {
			c.base += time.Duration(c.samples) * time.Second / time.Duration(c.rate)
		}
		c.rate, c.samples = rate, 0
	}
	pts := c.base + time.Duration(c.samples)*time.Second/time.Duration(rate)
	c.samples += int64(n)
	return pts
}

// readMP3 publishes an MP3 byte stream at the runner's path.
func (r *runner) readMP3(body io.Reader) error {
	p := newMP3Parser(body)
	frame, h, err := p.Next()
	if err != nil {
		return err
	}
	track := core.NewTrack(&core.MPEGAudio{})
	stream, err := r.m.Hub.Publish(r.src.Path, []*core.Track{track}, r.sourceInfo())
	if err != nil {
		return err
	}
	defer r.m.Hub.Unpublish(stream)
	r.markLive()

	var clock sampleClock
	for {
		pts := clock.Advance(h.SampleCount(), h.SampleRate)
		stream.WriteUnit(&core.Unit{Track: track, NTP: time.Now(), PTS: pts, Payload: [][]byte{frame}})
		if frame, h, err = p.Next(); err != nil {
			return err
		}
	}
}

// readADTS publishes an ADTS AAC byte stream at the runner's path.
func (r *runner) readADTS(body io.Reader) error {
	p := newADTSParser(body)
	f, err := p.Next()
	if err != nil {
		return err
	}
	cfg := f.Config
	track := core.NewTrack(&core.AAC{Config: &cfg})
	stream, err := r.m.Hub.Publish(r.src.Path, []*core.Track{track}, r.sourceInfo())
	if err != nil {
		return err
	}
	defer r.m.Hub.Unpublish(stream)
	r.markLive()

	var clock sampleClock
	for {
		if f.Config.SampleRate != cfg.SampleRate || f.Config.ChannelConfig != cfg.ChannelConfig ||
			f.Config.Type != cfg.Type {
			// Outputs were set up for the first config; start over.
			return errors.New("AAC configuration changed")
		}
		pts := clock.Advance(mpeg4audio.SamplesPerAccessUnit, f.Config.SampleRate)
		stream.WriteUnit(&core.Unit{Track: track, NTP: time.Now(), PTS: pts, Payload: [][]byte{f.AU}})
		if f, err = p.Next(); err != nil {
			return err
		}
	}
}
