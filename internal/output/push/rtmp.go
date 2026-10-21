package push

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/bluenviron/gortmplib"

	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/rtmpconv"
)

type rtmpSession struct {
	client    *gortmplib.Client
	written   atomic.Uint64
	stream    *core.Stream
	closeOnce sync.Once
}

// rtmpCarries reports whether rtmpconv can send at least one track.
func rtmpCarries(s *core.Stream) bool {
	for _, t := range s.Tracks {
		switch c := t.Codec.(type) {
		case *core.H264, *core.H265, *core.MPEGAudio, *core.AC3, *core.Opus:
			return true
		case *core.AAC:
			if c.Config != nil {
				return true
			}
		}
	}
	return false
}

func dialRTMP(ctx context.Context, rawURL string, s *core.Stream) (session, error) {
	if !rtmpCarries(s) {
		return nil, permanentf("stream has no track that can be sent over RTMP")
	}
	u, err := rtmpURL(rawURL)
	if err != nil {
		return nil, permanent(err)
	}
	sess := &rtmpSession{stream: s}
	sess.client = &gortmplib.Client{
		URL:         u,
		Publish:     true,
		DialContext: deadlineDialer(&sess.written),
	}
	if err := sess.client.Initialize(ctx); err != nil {
		return nil, err
	}
	// The server keeps sending small control messages (acknowledgements,
	// pings) that nothing needs. Discard them so they never fill the TCP
	// window, and close the session as soon as the server hangs up. Raw
	// reads do not touch the message writer used by Send.
	go func() {
		io.Copy(io.Discard, sess.client.NetConn()) //nolint:errcheck
		sess.close()
	}()
	return sess, nil
}

func (r *rtmpSession) send(sub *core.Subscription) error {
	err := rtmpconv.Send(r.client, r.stream, sub)
	if errors.Is(err, rtmpconv.ErrMPEGAudioLayer) {
		return permanent(err)
	}
	if errors.Is(err, rtmpconv.ErrNoTracks) {
		// Parameter sets may be missing from the first keyframe of a
		// stream that only carries them in-band; retrying is useful.
		return errors.New("no parameter sets in the first keyframe yet")
	}
	return err
}

func (r *rtmpSession) close() { r.closeOnce.Do(r.client.Close) }

func (r *rtmpSession) bytesSent() uint64 { return r.written.Load() }
