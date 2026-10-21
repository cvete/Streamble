package httpstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/core"
)

func TestListenersByCountry(t *testing.T) {
	h, hub := newHandler(t, allOn)
	if h.ListenersByCountry("live/radio") != nil {
		t.Fatal("expected nil without Country")
	}
	var (
		mu      sync.Mutex
		lookups []string
	)
	h.Country = func(ip string) string {
		mu.Lock()
		defer mu.Unlock()
		lookups = append(lookups, ip)
		if ip == "127.0.0.1" {
			return "FR"
		}
		return ""
	}
	if _, err := hub.Publish("live/radio", []*core.Track{aacTrack()}, core.SourceInfo{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/live/radio.aac", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The listener is registered before the headers are sent.
	if got := h.ListenersByCountry("live/radio"); len(got) != 1 || got["FR"] != 1 {
		t.Fatalf("by country = %v", got)
	}
	mu.Lock()
	if len(lookups) != 1 || lookups[0] != "127.0.0.1" {
		t.Fatalf("lookups = %v", lookups)
	}
	mu.Unlock()

	// A second listener from an address without a country counts as "--".
	r := httptest.NewRequest(http.MethodGet, "/live/radio.aac", nil)
	r.RemoteAddr = "198.51.100.9:4000"
	c := h.listenerCountry(r)
	h.addListener("live/radio", c, 1)
	if got := h.ListenersByCountry("live/radio"); got["FR"] != 1 || got["--"] != 1 || h.Listeners("live/radio") != 2 {
		t.Fatalf("by country = %v", got)
	}
	h.addListener("live/radio", c, -1)

	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for h.Listeners("live/radio") != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.ListenersByCountry("live/radio"); len(got) != 0 || h.Listeners("live/radio") != 0 {
		t.Fatalf("after disconnect: %v", got)
	}
}
