package push

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"time"
)

// deadlineConn sets a write deadline before every write, so a peer that
// stops reading fails the push instead of blocking it forever, and counts
// the bytes written.
type deadlineConn struct {
	net.Conn
	timeout time.Duration
	written *atomic.Uint64
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)) //nolint:errcheck
	n, err := c.Conn.Write(b)
	if c.written != nil {
		c.written.Add(uint64(n))
	}
	return n, err
}

// deadlineDialer returns a DialContext function whose connections use
// deadlineConn. When written is non-nil it receives the byte count.
func deadlineDialer(written *atomic.Uint64) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{KeepAlive: 15 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &deadlineConn{Conn: c, timeout: writeTimeout, written: written}, nil
	}
}

// watchdogWriter closes a connection when a single write takes longer than
// timeout. It is used for connections whose SetWriteDeadline is not
// effective (gosrt), as a second line of defence next to SRT's own peer
// idle timeout.
type watchdogWriter struct {
	w       io.Writer
	closer  io.Closer
	timeout time.Duration
	written atomic.Uint64
}

func (w *watchdogWriter) Write(b []byte) (int, error) {
	t := time.AfterFunc(w.timeout, func() { w.closer.Close() })
	n, err := w.w.Write(b)
	t.Stop()
	w.written.Add(uint64(n))
	return n, err
}
