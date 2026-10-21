package hls

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/config"
)

func TestViewerTrackerCountries(t *testing.T) {
	v := newViewerTracker()
	lookups := 0
	lookup := func(c string) func() string {
		return func() string { lookups++; return c }
	}
	v.touchFrom("a", lookup("FR"))
	v.touchFrom("a", lookup("FR")) // known viewer: no second lookup
	v.touchFrom("b", lookup("MK"))
	v.touchFrom("c", lookup("FR"))
	v.touch("d") // no lookup function: unknown country
	if lookups != 3 {
		t.Fatalf("lookups = %d, want 3", lookups)
	}
	got := v.byCountry()
	if len(got) != 3 || got["FR"] != 2 || got["MK"] != 1 || got[unknownCountry] != 1 {
		t.Fatalf("by country = %v", got)
	}
	v.seen["b"] = time.Now().Add(-2 * viewerWindow)
	if got := v.byCountry(); got["MK"] != 0 || len(v.country) != 2 {
		t.Fatalf("stale viewer kept: %v %v", got, v.country)
	}
}

func TestViewersByCountry(t *testing.T) {
	m := NewManager(config.Default().HLS, "", slog.Default())
	if m.ViewersByCountry("live/a") != nil {
		t.Fatal("expected nil without Country")
	}
	m.Country = func(ip string) string {
		if ip == "203.0.113.7" {
			return "FR"
		}
		return "--"
	}
	if got := m.ViewersByCountry("live/a"); got == nil || len(got) != 0 {
		t.Fatalf("unknown stream: %v", got)
	}
	hs := &hlsStream{viewers: newViewerTracker()}
	m.streams["live/a"] = hs
	for _, addr := range []string{"203.0.113.7:1000", "10.0.0.1:2000", "203.0.113.7:1000"} {
		r := httptest.NewRequest(http.MethodGet, "/live/a/index.m3u8", nil)
		r.RemoteAddr = addr
		hs.viewers.touchFrom(clientID(r), func() string { return m.Country(remoteIP(r)) })
	}
	got := m.ViewersByCountry("live/a")
	if len(got) != 2 || got["FR"] != 1 || got["--"] != 1 || m.Viewers("live/a") != 2 {
		t.Fatalf("by country = %v", got)
	}
}
