package pull

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/gortmplib"
	gosrt "github.com/datarhei/gosrt"

	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/tsconv"
)

// udpReadBuffer is the socket receive buffer for udp:// sources. MPEG-TS
// over UDP is bursty; a small kernel buffer drops packets.
const udpReadBuffer = 4 << 20

// closeOnDone closes c when ctx is cancelled, which interrupts blocked
// reads. The returned function must be called once the connection is no
// longer used.
func closeOnDone(ctx context.Context, c interface{ Close() error }) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			c.Close()
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// timeoutErr maps deadline errors to errIdle.
func timeoutErr(err error) error {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return errIdle
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errIdle
	}
	return err
}

// udpListenAddr parses the host part of a udp:// URL. "@" or an empty host
// means every local address, as in FFmpeg.
func udpListenAddr(u *url.URL) (*net.UDPAddr, error) {
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return nil, fmt.Errorf("udp URL needs host:port: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("udp URL has an invalid port")
	}
	host = strings.TrimPrefix(host, "@")
	addr := &net.UDPAddr{Port: port}
	if host != "" {
		ip := net.ParseIP(host)
		if ip == nil {
			ips, err := net.LookupIP(host)
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("cannot resolve %s", host)
			}
			ip = ips[0]
		}
		addr.IP = ip
	}
	return addr, nil
}

func (r *runner) pullUDP(ctx context.Context, u *url.URL) error {
	// url.Parse treats "@" as empty user info; the host is what follows.
	addr, err := udpListenAddr(u)
	if err != nil {
		return err
	}

	var conn *net.UDPConn
	if addr.IP != nil && addr.IP.IsMulticast() {
		var ifi *net.Interface
		if name := u.Query().Get("interface"); name != "" {
			if ifi, err = net.InterfaceByName(name); err != nil {
				return err
			}
		}
		conn, err = net.ListenMulticastUDP("udp", ifi, addr)
	} else {
		conn, err = net.ListenUDP("udp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetReadBuffer(udpReadBuffer); err != nil {
		r.m.Log.Debug("cannot set UDP read buffer", "path", r.src.Path, "err", err)
	}
	defer closeOnDone(ctx, conn)()

	// Demux reads through a 64 KB buffer, so each datagram is read whole.
	err = r.demuxTS(tsconv.TimeoutConn(conn, readTimeout))
	return timeoutErr(err)
}

func (r *runner) pullSRT(ctx context.Context, u *url.URL) error {
	if m := u.Query().Get("mode"); m != "" && m != "caller" {
		return errors.New("only SRT caller mode is supported")
	}
	cfg := gosrt.DefaultConfig()
	addr, err := cfg.UnmarshalURL(r.src.URL)
	if err != nil {
		// The error may quote the URL; keep it generic.
		return errors.New("invalid srt URL")
	}
	dialCtx, cancel := context.WithTimeout(ctx, readTimeout)
	conn, err := gosrt.DialWithContext(dialCtx, "srt", addr, cfg)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	defer closeOnDone(ctx, conn)()

	// gosrt connections ignore read deadlines: close on silence instead.
	wd := newWatchdog(readTimeout, func() { conn.Close() })
	defer wd.Stop()
	return r.demuxTS(&idleReader{R: conn, WD: wd})
}

// rtmpClientURL splits rtmp://host/app/stream[?query] into the connect URL
// (rtmp://host/app) and the play path (stream[?query]), the way most RTMP
// servers expect it. gortmplib takes the play path from the fragment.
func rtmpClientURL(u *url.URL) *url.URL {
	nu := *u
	if nu.Fragment != "" {
		return &nu
	}
	p := strings.TrimPrefix(nu.Path, "/")
	app, stream, ok := strings.Cut(p, "/")
	if !ok || stream == "" {
		return &nu
	}
	nu.Path = "/" + app
	nu.RawPath = ""
	nu.Fragment = stream
	if nu.RawQuery != "" {
		nu.Fragment += "?" + nu.RawQuery
		nu.RawQuery = ""
	}
	return &nu
}

func (r *runner) pullRTMP(ctx context.Context, u *url.URL) error {
	c := &gortmplib.Client{URL: rtmpClientURL(u), Publish: false}
	initCtx, cancel := context.WithTimeout(ctx, readTimeout)
	err := c.Initialize(initCtx)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()
	nc := c.NetConn()
	defer closeOnDone(ctx, nc)()

	nc.SetReadDeadline(time.Now().Add(readTimeout)) //nolint:errcheck
	rd := &gortmplib.Reader{Conn: c}
	if err := rd.Initialize(); err != nil {
		return timeoutErr(err)
	}

	var stream *core.Stream
	tracks := rtmp.AttachTracks(rd, &stream, r.m.Log)
	stream, err = r.m.Hub.Publish(r.src.Path, tracks, r.sourceInfo())
	if err != nil {
		return err
	}
	defer r.m.Hub.Unpublish(stream)
	r.markLive()

	for {
		nc.SetReadDeadline(time.Now().Add(readTimeout)) //nolint:errcheck
		if err := rd.Read(); err != nil {
			return timeoutErr(err)
		}
	}
}
