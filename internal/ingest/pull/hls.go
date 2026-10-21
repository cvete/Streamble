package pull

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"

	"github.com/cvete/streamble/internal/core"
)

// clockToDuration converts a timestamp in units of 1/rate s without
// overflowing for long-running streams.
func clockToDuration(ts int64, rate int) time.Duration {
	r := int64(rate)
	return time.Duration(ts/r)*time.Second + time.Duration(ts%r)*time.Second/time.Duration(r)
}

// hlsPublisher serialises writes: gohlslib delivers every track from its
// own goroutine, but a hub stream has a single writer.
type hlsPublisher struct {
	mu     sync.Mutex
	stream *core.Stream
	wd     *watchdog
}

func (p *hlsPublisher) write(u *core.Unit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wd.Kick()
	if p.stream != nil {
		p.stream.WriteUnit(u)
	}
}

func (r *runner) pullHLS(ctx context.Context, httpClient *http.Client) error {
	pub := &hlsPublisher{}
	stopped := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopped) }) }
	pub.wd = newWatchdog(readTimeout, stop)
	defer pub.wd.Stop()

	debug := func(string) {}
	c := &gohlslib.Client{
		URI:                       r.src.URL,
		HTTPClient:                httpClient,
		OnDownloadPrimaryPlaylist: debug,
		OnDownloadStreamPlaylist:  debug,
		OnDownloadSegment:         debug,
		OnDownloadPart:            debug,
		OnDecodeError: func(err error) {
			r.m.Log.Debug("HLS decode error", "path", r.src.Path, "err", sanitizeError(err, r.src.URL))
		},
	}
	c.OnTracks = func(tracks []*gohlslib.Track) error {
		var hubTracks []*core.Track
		for _, ht := range tracks {
			if t := r.attachHLSTrack(c, ht, pub); t != nil {
				hubTracks = append(hubTracks, t)
			} else {
				r.m.Log.Warn("HLS track ignored: codec not supported", "path", r.src.Path,
					"codec", fmt.Sprintf("%T", ht.Codec))
			}
		}
		s, err := r.m.Hub.Publish(r.src.Path, hubTracks, r.sourceInfo())
		if err != nil {
			return err
		}
		pub.mu.Lock()
		pub.stream = s
		pub.mu.Unlock()
		r.markLive()
		return nil
	}

	if err := c.Start(); err != nil {
		return err
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- c.Wait2() }()

	var err error
	select {
	case err = <-waitErr:
	case <-ctx.Done():
		c.Close()
		err = <-waitErr
	case <-stopped:
		c.Close()
		<-waitErr
		err = errIdle
	}

	pub.mu.Lock()
	if pub.stream != nil {
		r.m.Hub.Unpublish(pub.stream)
		pub.stream = nil
	}
	pub.mu.Unlock()
	if err == nil {
		err = errors.New("HLS client stopped")
	}
	return err
}

func (r *runner) attachHLSTrack(c *gohlslib.Client, ht *gohlslib.Track, pub *hlsPublisher) *core.Track {
	rate := ht.ClockRate
	switch hc := ht.Codec.(type) {
	case *codecs.H264:
		t := core.NewTrack(&core.H264{SPS: hc.SPS, PPS: hc.PPS})
		k := core.NewParamKeeper(t.Codec)
		c.OnDataH26x(ht, func(pts, dts int64, au [][]byte) {
			r.writeHLSVideo(pub, t, k, clockToDuration(pts, rate), clockToDuration(dts, rate), au)
		})
		return t

	case *codecs.H265:
		t := core.NewTrack(&core.H265{VPS: hc.VPS, SPS: hc.SPS, PPS: hc.PPS})
		k := core.NewParamKeeper(t.Codec)
		c.OnDataH26x(ht, func(pts, dts int64, au [][]byte) {
			r.writeHLSVideo(pub, t, k, clockToDuration(pts, rate), clockToDuration(dts, rate), au)
		})
		return t

	case *codecs.MPEG4Audio:
		cfg := hc.Config
		t := core.NewTrack(&core.AAC{Config: &cfg})
		c.OnDataMPEG4Audio(ht, func(pts int64, aus [][]byte) {
			pub.write(&core.Unit{Track: t, NTP: time.Now(), PTS: clockToDuration(pts, rate), Payload: aus})
		})
		return t

	case *codecs.Opus:
		ch := hc.ChannelCount
		if ch <= 0 {
			ch = 2
		}
		t := core.NewTrack(&core.Opus{ChannelCount: ch})
		c.OnDataOpus(ht, func(pts int64, pkts [][]byte) {
			pub.write(&core.Unit{Track: t, NTP: time.Now(), PTS: clockToDuration(pts, rate), Payload: pkts})
		})
		return t
	}
	return nil
}

func (r *runner) writeHLSVideo(pub *hlsPublisher, t *core.Track, k *core.ParamKeeper,
	pts, dts time.Duration, au [][]byte,
) {
	// The keeper belongs to this track's goroutine only.
	au, key := k.Process(au)
	if au == nil {
		pub.wd.Kick()
		return
	}
	pub.write(&core.Unit{Track: t, NTP: time.Now(), PTS: pts, DTS: dts, Payload: au, RandomAccess: key})
}
