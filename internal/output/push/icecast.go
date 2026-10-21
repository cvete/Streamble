package push

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/cvete/streamble/internal/core"
)

// errNoIcecastAudio explains how to get a stream Icecast can carry.
var errNoIcecastAudio = errors.New("stream has no MP3 or AAC audio track; " +
	"a transcode rendition with audio_codec mp3 or aac can produce one")

// icecastTrack picks the audio track to send: MP3 first (plays in every
// Icecast client), then AAC.
func icecastTrack(s *core.Stream) (*core.Track, string) {
	for _, t := range s.Tracks {
		if _, ok := t.Codec.(*core.MPEGAudio); ok {
			return t, "audio/mpeg"
		}
	}
	for _, t := range s.Tracks {
		if c, ok := t.Codec.(*core.AAC); ok && c.Config != nil && c.Config.Type >= 1 && c.Config.Type <= 4 {
			return t, "audio/aac"
		}
	}
	return nil, ""
}

// icecastTarget is a parsed icecast:// or icecasts:// URL.
type icecastTarget struct {
	tls      bool
	host     string // host:port
	hostname string
	path     string // mount, with query
	user     string
	pass     string
}

func parseIcecastURL(rawURL string) (*icecastTarget, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid icecast:// URL")
	}
	if u.Path == "" || u.Path == "/" {
		return nil, errors.New("icecast URL needs a mount point, e.g. icecast://source:pass@host:8000/live.mp3")
	}
	t := &icecastTarget{tls: u.Scheme == "icecasts", hostname: u.Hostname(), user: "source"}
	port := u.Port()
	if port == "" {
		port = "8000"
		if t.tls {
			port = "443"
		}
	}
	t.host = net.JoinHostPort(u.Hostname(), port)
	t.path = u.EscapedPath()
	if u.RawQuery != "" {
		t.path += "?" + u.RawQuery
	}
	if u.User != nil {
		if n := u.User.Username(); n != "" {
			t.user = n
		}
		t.pass, _ = u.User.Password()
	}
	return t, nil
}

// header returns the source request (Icecast 2.4+ HTTP PUT protocol). The
// body that follows is the raw audio stream, sent until the connection
// closes; Icecast does not accept chunked encoding.
func (t *icecastTarget) header(contentType, name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PUT %s HTTP/1.1\r\n", t.path)
	fmt.Fprintf(&b, "Host: %s\r\n", t.host)
	auth := base64.StdEncoding.EncodeToString([]byte(t.user + ":" + t.pass))
	fmt.Fprintf(&b, "Authorization: Basic %s\r\n", auth)
	fmt.Fprintf(&b, "User-Agent: Streamble\r\n")
	fmt.Fprintf(&b, "Content-Type: %s\r\n", contentType)
	fmt.Fprintf(&b, "Ice-Name: %s\r\n", name)
	fmt.Fprintf(&b, "Ice-Public: 0\r\n")
	fmt.Fprintf(&b, "Expect: 100-continue\r\n")
	b.WriteString("\r\n")
	return b.String()
}

type icecastSession struct {
	conn      net.Conn
	w         io.Writer
	track     *core.Track
	written   atomic.Uint64
	closeOnce sync.Once
}

func dialIcecast(ctx context.Context, rawURL string, s *core.Stream) (session, error) {
	track, contentType := icecastTrack(s)
	if track == nil {
		return nil, permanent(errNoIcecastAudio)
	}
	t, err := parseIcecastURL(rawURL)
	if err != nil {
		return nil, permanent(err)
	}

	sess := &icecastSession{track: track}
	conn, err := deadlineDialer(&sess.written)(ctx, "tcp", t.host)
	if err != nil {
		return nil, err
	}
	if t.tls {
		tc := tls.Client(conn, &tls.Config{ServerName: t.hostname})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tc
	}
	sess.conn = conn
	sess.w = conn

	// Abort the handshake when ctx ends.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	if _, err := io.WriteString(conn, t.header(contentType, s.Path)); err != nil {
		conn.Close()
		return nil, err
	}

	// Icecast answers once the source is authenticated (100 Continue or
	// 200 OK), before any audio. Wait for it so wrong passwords and taken
	// mount points are reported clearly.
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("no answer from icecast server: %w", ctx.Err())
		}
		return nil, fmt.Errorf("reading icecast answer: %w", err)
	}
	if resp.StatusCode != http.StatusContinue && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		conn.Close()
		return nil, fmt.Errorf("icecast server answered %s", resp.Status)
	}
	return sess, nil
}

func (r *icecastSession) send(sub *core.Subscription) error {
	var hdr []byte
	if c, ok := r.track.Codec.(*core.AAC); ok {
		var err error
		if hdr, err = adtsTemplate(c.Config); err != nil {
			return permanent(err)
		}
	}
	bw := bufio.NewWriterSize(r.w, 16*1024)
	for u := range sub.C() {
		if u.Track != r.track {
			continue
		}
		for _, frame := range u.Payload {
			if hdr != nil {
				setADTSLength(hdr, len(frame))
				bw.Write(hdr) //nolint:errcheck // reported by Flush
			}
			bw.Write(frame) //nolint:errcheck // reported by Flush
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
	return io.EOF
}

// adtsTemplate returns an ADTS header (ISO 14496-3, 1.A.2) for the
// track's config. setADTSLength fills in each access unit's length.
func adtsTemplate(cfg *mpeg4audio.AudioSpecificConfig) ([]byte, error) {
	pkt := &mpeg4audio.ADTSPacket{
		Type:          cfg.Type,
		SampleRate:    cfg.SampleRate,
		ChannelConfig: cfg.ChannelConfig,
		ChannelCount:  cfg.ChannelCount, //nolint:staticcheck
	}
	buf, err := mpeg4audio.ADTSPackets{pkt}.Marshal()
	if err != nil {
		return nil, fmt.Errorf("cannot write ADTS: %w", err)
	}
	return buf[:7], nil
}

// setADTSLength sets the 13-bit frame length (header included).
func setADTSLength(h []byte, auLen int) {
	n := auLen + 7
	h[3] = h[3]&0xfc | byte(n>>11)&0x03
	h[4] = byte(n >> 3)
	h[5] = h[5]&0x1f | byte(n&0x07)<<5
}

func (r *icecastSession) close() { r.closeOnce.Do(func() { r.conn.Close() }) }

func (r *icecastSession) bytesSent() uint64 { return r.written.Load() }
