package pull

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cvete/streamble/internal/tsconv"
)

// Formats an http(s) source can carry.
const (
	formatUnknown = ""
	formatTS      = "mpegts"
	formatMP3     = "mp3"
	formatAAC     = "aac"
	formatHLS     = "hls"
)

// detectFormat picks the format from the Content-Type, falling back to the
// URL extension when the type is missing or generic.
func detectFormat(contentType, urlPath string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(contentType))
	}
	switch mt {
	case "video/mp2t", "video/mpeg2-ts", "video/mpegts":
		return formatTS
	case "audio/mpeg", "audio/mp3", "audio/mpeg3", "audio/x-mpeg":
		return formatMP3
	case "audio/aac", "audio/aacp", "audio/x-aac", "audio/x-aacp":
		return formatAAC
	case "application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl", "audio/x-mpegurl":
		return formatHLS
	}
	switch strings.ToLower(path.Ext(urlPath)) {
	case ".ts", ".mts", ".m2ts":
		return formatTS
	case ".mp3":
		return formatMP3
	case ".aac":
		return formatAAC
	case ".m3u8":
		return formatHLS
	}
	return formatUnknown
}

// sniffFormat guesses the format from the first bytes of a body.
func sniffFormat(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("#EXTM3U")):
		return formatHLS
	case len(b) >= 189 && b[0] == 0x47 && b[188] == 0x47:
		return formatTS
	}
	return formatUnknown
}

// watchdog calls onFire when Kick has not been called for d.
type watchdog struct {
	d     time.Duration
	timer *time.Timer
	fired atomic.Bool
}

func newWatchdog(d time.Duration, onFire func()) *watchdog {
	w := &watchdog{d: d}
	w.timer = time.AfterFunc(d, func() {
		w.fired.Store(true)
		onFire()
	})
	return w
}

// Kick postpones the deadline.
func (w *watchdog) Kick() { w.timer.Reset(w.d) }

// Stop disarms the watchdog.
func (w *watchdog) Stop() { w.timer.Stop() }

// Fired reports whether the source went silent.
func (w *watchdog) Fired() bool { return w.fired.Load() }

// idleReader kicks a watchdog on every successful read and reports errIdle
// once the watchdog has fired (and closed the source under the reader).
type idleReader struct {
	R  io.Reader
	WD *watchdog
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.R.Read(p)
	if n > 0 {
		r.WD.Kick()
	}
	if err != nil && r.WD.Fired() {
		return n, errIdle
	}
	return n, err
}

// icyConn makes SHOUTcast v1 replies ("ICY 200 OK") parseable by net/http
// by rewriting the status line to HTTP/1.0.
type icyConn struct {
	net.Conn
	once sync.Once
	pre  []byte
	err  error
}

func (c *icyConn) Read(p []byte) (int, error) {
	c.once.Do(func() {
		buf := make([]byte, 4)
		n, err := io.ReadFull(c.Conn, buf)
		buf = buf[:n]
		if bytes.Equal(buf, []byte("ICY ")) {
			buf = []byte("HTTP/1.0 ")
		}
		c.pre, c.err = buf, err
	})
	if len(c.pre) > 0 {
		n := copy(p, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	if c.err != nil {
		err := c.err
		c.err = nil
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, err
		}
		return 0, io.EOF
	}
	return c.Conn.Read(p)
}

func newHTTPClient(scheme string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = readTimeout
	tr.DisableCompression = true
	dialer := &net.Dialer{Timeout: readTimeout, KeepAlive: 30 * time.Second}
	if scheme == "http" {
		// Plain HTTP only: the rewrite must see the bytes before any TLS.
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &icyConn{Conn: c}, nil
		}
	}
	return &http.Client{Transport: tr}
}

func (r *runner) pullHTTP(ctx context.Context, u *url.URL) error {
	scheme := strings.ToLower(u.Scheme)
	client := newHTTPClient(scheme)
	defer client.CloseIdleConnections()

	if detectFormat("", u.Path) == formatHLS {
		return r.pullHLS(ctx, client)
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, r.src.URL, nil)
	if err != nil {
		return errors.New("invalid URL")
	}
	// No "Icy-MetaData: 1": Icecast then sends plain audio without inline
	// metadata blocks.
	req.Header.Set("User-Agent", "Streamble")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status %s", resp.Status)
	}

	wd := newWatchdog(readTimeout, cancel)
	defer wd.Stop()
	body := bufio.NewReaderSize(&idleReader{R: resp.Body, WD: wd}, 64*1024)

	format := detectFormat(resp.Header.Get("Content-Type"), u.Path)
	if format == formatUnknown {
		head, _ := body.Peek(189)
		format = sniffFormat(head)
	}

	switch format {
	case formatTS:
		return r.demuxTS(body)
	case formatMP3:
		return r.readMP3(body)
	case formatAAC:
		return r.readADTS(body)
	case formatHLS:
		resp.Body.Close()
		wd.Stop()
		return r.pullHLS(ctx, client)
	}
	return fmt.Errorf("unsupported content type %q", resp.Header.Get("Content-Type"))
}

// demuxTS publishes an MPEG-TS byte stream and marks the source live once
// the stream exists.
func (r *runner) demuxTS(rd io.Reader) error {
	// tsconv.Demux publishes after reading the PMT; detect that moment by
	// polling the hub from a helper goroutine.
	done := make(chan struct{})
	defer close(done)
	go r.watchPublish(done)
	return tsconv.Demux(rd, r.m.Hub, r.src.Path, r.sourceInfo(), r.m.Log)
}

// watchPublish marks the runner live when a stream published by this
// source shows up in the hub.
func (r *runner) watchPublish(done <-chan struct{}) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if s := r.m.Hub.Lookup(r.src.Path); s != nil &&
				s.Source.Protocol == "pull" && s.Source.RemoteAddr == r.redacted {
				r.markLive()
				return
			}
		}
	}
}
