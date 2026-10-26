// Package realip restores the client address behind a reverse proxy.
//
// When Streamble runs behind Traefik, Caddy, nginx or a CDN, every request
// arrives from the proxy. Login rate limits, the audit log, viewer counts
// and viewer countries all need the real client address, so requests from
// a trusted proxy have their RemoteAddr replaced with the address the proxy
// reports in X-Forwarded-For (or X-Real-IP). Requests from anyone else keep
// their address and lose any X-Forwarded-* headers, so clients cannot spoof
// them.
package realip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Parse turns addresses and CIDR ranges ("10.0.0.0/8", "127.0.0.1") into
// prefixes.
func Parse(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", s, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: not an IP address or CIDR range", s)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// Handler rewrites r.RemoteAddr for requests from trusted proxies. With no
// trusted proxies it only strips forwarding headers.
func Handler(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, ok := addrOf(r.RemoteAddr)
		if !ok || !contains(trusted, peer) {
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Forwarded-Proto")
			r.Header.Del("X-Forwarded-Host")
			r.Header.Del("X-Real-Ip")
			next.ServeHTTP(w, r)
			return
		}
		if client, ok := clientFrom(r.Header, trusted); ok {
			r2 := r.Clone(r.Context())
			r2.RemoteAddr = net.JoinHostPort(client.String(), "0")
			r = r2
		}
		next.ServeHTTP(w, r)
	})
}

// clientFrom walks X-Forwarded-For from the right (the hop closest to us)
// and returns the first address that is not a trusted proxy. Entries to the
// left of it were written by the client and cannot be trusted.
func clientFrom(h http.Header, trusted []netip.Prefix) (netip.Addr, bool) {
	var hops []string
	for _, v := range h.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	var last netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := addrOf(strings.TrimSpace(hops[i]))
		if !ok {
			break
		}
		last = a
		if !contains(trusted, a) {
			return a, true
		}
	}
	if last.IsValid() {
		return last, true // every hop is a trusted proxy
	}
	if a, ok := addrOf(strings.TrimSpace(h.Get("X-Real-Ip"))); ok {
		return a, true
	}
	return netip.Addr{}, false
}

func addrOf(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func contains(list []netip.Prefix, a netip.Addr) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
