package realip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler(t *testing.T) {
	trusted, err := Parse([]string{"10.0.0.0/8", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, peer, xff, realIP, wantAddr, wantProto string
	}{
		{"direct client keeps its address", "203.0.113.5:4000", "1.2.3.4", "", "203.0.113.5:4000", ""},
		{"trusted proxy", "10.0.0.2:5000", "198.51.100.7", "", "198.51.100.7:0", "https"},
		{"spoofed left entries ignored", "10.0.0.2:5000", "6.6.6.6, 198.51.100.7", "", "198.51.100.7:0", "https"},
		{"chain of trusted proxies", "127.0.0.1:5000", "198.51.100.7, 10.1.1.1", "", "198.51.100.7:0", "https"},
		{"X-Real-IP fallback", "10.0.0.2:5000", "", "198.51.100.9", "198.51.100.9:0", "https"},
		{"trusted proxy without headers", "10.0.0.2:5000", "", "", "10.0.0.2:5000", "https"},
		{"garbage header", "10.0.0.2:5000", "not-an-ip", "", "10.0.0.2:5000", "https"},
		{"ipv6 client", "10.0.0.2:5000", "2001:db8::1", "", "[2001:db8::1]:0", "https"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotAddr, gotProto string
			h := Handler(trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotAddr, gotProto = r.RemoteAddr, r.Header.Get("X-Forwarded-Proto")
			}))
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.peer
			r.Header.Set("X-Forwarded-Proto", "https")
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if c.realIP != "" {
				r.Header.Set("X-Real-IP", c.realIP)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if gotAddr != c.wantAddr || gotProto != c.wantProto {
				t.Fatalf("addr %q proto %q, want %q %q", gotAddr, gotProto, c.wantAddr, c.wantProto)
			}
		})
	}
}

func TestParse(t *testing.T) {
	if _, err := Parse([]string{"10.0.0.0/33"}); err == nil {
		t.Fatal("bad CIDR accepted")
	}
	if _, err := Parse([]string{"proxy.local"}); err == nil {
		t.Fatal("host name accepted")
	}
	p, err := Parse([]string{" 172.16.0.0/12 ", "", "::1"})
	if err != nil || len(p) != 2 {
		t.Fatalf("%v %v", p, err)
	}
}
