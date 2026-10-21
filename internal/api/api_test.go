package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/output/hls"
)

func newTestAPI(token string) (*API, *core.Hub) {
	hub := core.NewHub(slog.Default())
	hm := hls.NewManager(config.Default().HLS, "*", slog.Default())
	hub.AddHook(hm)
	return New(hub, hm, token, "llhls"), hub
}

func get(t *testing.T, a *API, path, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body) //nolint:errcheck
	return rec.Code, body
}

func TestTokenProtectsEverythingButHealth(t *testing.T) {
	a, _ := newTestAPI("secret")
	defer a.Close()

	if code, _ := get(t, a, "/api/v1/health", ""); code != http.StatusOK {
		t.Fatalf("health must be public, got %d", code)
	}
	if code, _ := get(t, a, "/api/v1/streams", ""); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", code)
	}
	if code, _ := get(t, a, "/api/v1/streams", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", code)
	}
	if code, _ := get(t, a, "/api/v1/streams", "secret"); code != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", code)
	}
}

func TestStreamsAndStatusEndpoints(t *testing.T) {
	a, hub := newTestAPI("")
	defer a.Close()

	if _, err := hub.Publish("live/tv1", []*core.Track{core.NewTrack(&core.H264{})},
		core.SourceInfo{Protocol: "rtmp"}); err != nil {
		t.Fatal(err)
	}
	a.HLS.ABR = func(path string) []string {
		if path == "live/tv1" {
			return []string{"live/tv1_720p"}
		}
		return nil
	}
	a.Listeners = func(string) int { return 3 }
	a.AddStatus("pushes", func() any { return []string{"youtube"} })

	code, body := get(t, a, "/api/v1/streams/live/tv1", "")
	if code != http.StatusOK || body["path"] != "live/tv1" || body["viewers"] != float64(3) {
		t.Fatalf("unexpected stream info: %d %v", code, body)
	}
	if hlsInfo := body["hls"].(map[string]any); hlsInfo["abr"] != "/live/tv1/abr.m3u8" {
		t.Fatalf("missing ABR URL: %v", hlsInfo)
	}

	code, body = get(t, a, "/api/v1/pushes", "")
	if code != http.StatusOK || body["pushes"] == nil {
		t.Fatalf("status endpoint failed: %d %v", code, body)
	}
	if code, _ := get(t, a, "/api/v1/nope", ""); code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", code)
	}
	if code, _ := get(t, a, "/api/v1/streams/live/none", ""); code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown stream, got %d", code)
	}
}
