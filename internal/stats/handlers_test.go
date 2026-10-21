package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/api"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/settings"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/users"
)

type apiEnv struct {
	srv   *httptest.Server
	token string
	db    *store.DB
	rec   *Recorder
	live  *live
}

// newAPIEnv serves the stats routes with a viewer API token.
func newAPIEnv(t *testing.T, d *store.DB, geo *GeoIP) *apiEnv {
	t.Helper()
	ctx := context.Background()
	base := config.Default()
	set, err := settings.Open(ctx, base, d, discard)
	if err != nil {
		t.Fatal(err)
	}
	hub := newHub()
	hm := hls.NewManager(base.HLS, "", discard)
	hub.AddHook(hm)
	accounts := &users.Service{DB: d, TTL: time.Hour, Log: discard}
	hash, err := users.HashPassword("viewer-password")
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{Username: "watcher", PasswordHash: hash, Role: store.RoleViewer}
	if err := d.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	token, _, err := accounts.CreateAPIToken(ctx, u, "test")
	if err != nil {
		t.Fatal(err)
	}
	a := api.New(api.Options{Hub: hub, HLS: hm, Users: accounts, Settings: set, DB: d, Bus: events.NewBus(), Log: discard})
	t.Cleanup(a.Close)

	l := newLive()
	rec := &Recorder{DB: d, Hub: hub, Viewers: a.Viewers, Bitrates: a.Bitrates, Countries: l.Countries, Log: discard}
	Register(a, rec, geo)
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apiEnv{srv: srv, token: token, db: d, rec: rec, live: l}
}

func (e *apiEnv) get(t *testing.T, path string, q url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *apiEnv) must(t *testing.T, path string, q url.Values, v any) string {
	t.Helper()
	code, body := e.get(t, path, q)
	if code != http.StatusOK {
		t.Fatalf("GET %s?%s: %d %s", path, q.Encode(), code, body)
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatal(err)
	}
	return body
}

func rng(path string, from, to time.Time) url.Values {
	q := url.Values{"from": {from.Format(time.RFC3339)}, "to": {msString(to.UnixMilli())}}
	if path != "-" {
		q.Set("path", path)
	}
	return q
}

func msString(n int64) string { return fmt.Sprint(n) }

func insertMinute(t *testing.T, d *store.DB, ts time.Time, path string, v, vmax, in, out, bin, bout int64) {
	t.Helper()
	if _, err := d.Exec(context.Background(), `INSERT INTO stats_minute VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ts.UnixMilli(), path, v, vmax, in, out, bin, bout); err != nil {
		t.Fatal(err)
	}
}

func insertCountry(t *testing.T, d *store.DB, ts time.Time, path, c string, v int64) {
	t.Helper()
	if _, err := d.Exec(context.Background(), `INSERT INTO stats_country VALUES (?, ?, ?, ?)`,
		ts.UnixMilli(), path, c, v); err != nil {
		t.Fatal(err)
	}
}

type seriesResp struct {
	Path        string  `json:"path"`
	StepSeconds int64   `json:"step_seconds"`
	Points      []point `json:"points"`
}

func TestSeries(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		e := newAPIEnv(t, d, nil)
		// 20 minutes from 12:00 on live/a: viewers = minute index,
		// viewers_max = 2x, bitrates 100/200, 10 bytes in, 1000 out each.
		for i := int64(0); i < 20; i++ {
			insertMinute(t, d, t0.Add(time.Duration(i)*time.Minute), "live/a", i, 2*i, 100, 200, 10, 1000)
		}
		insertMinute(t, d, t0, ServerPath, 99, 99, 0, 0, 0, 0)
		insertMinute(t, d, t0.Add(-time.Minute), "live/a", 50, 50, 0, 0, 0, 0) // before the range

		// Up to 6 hours: one point per minute.
		var r seriesResp
		e.must(t, "/api/v1/stats/series", rng("live/a", t0, t0.Add(6*time.Hour)), &r)
		if r.Path != "live/a" || r.StepSeconds != 60 || len(r.Points) != 20 {
			t.Fatalf("1-minute series: %+v", r)
		}
		if p := r.Points[3]; !p.TS.Equal(t0.Add(3*time.Minute)) || p.Viewers != 3 || p.ViewersMax != 6 ||
			p.BitrateInKbps != 100 || p.BitrateOutKbps != 200 || p.BytesIn != 10 || p.BytesOut != 1000 {
			t.Fatalf("point 3: %+v", p)
		}

		// Up to 7 days: 15-minute buckets (0-14 and 15-19).
		e.must(t, "/api/v1/stats/series", rng("live/a", t0, t0.Add(7*24*time.Hour)), &r)
		if r.StepSeconds != 900 || len(r.Points) != 2 {
			t.Fatalf("15-minute series: %+v", r)
		}
		p0, p1 := r.Points[0], r.Points[1]
		if !p0.TS.Equal(t0) || p0.Viewers != 7 || p0.ViewersMax != 28 || p0.BytesOut != 15000 || p0.BytesIn != 150 ||
			p0.BitrateOutKbps != 200 {
			t.Fatalf("bucket 0: %+v", p0)
		}
		if !p1.TS.Equal(t0.Add(15*time.Minute)) || p1.Viewers != 17 || p1.ViewersMax != 38 || p1.BytesOut != 5000 {
			t.Fatalf("bucket 1: %+v", p1)
		}

		// Beyond: hourly.
		e.must(t, "/api/v1/stats/series", rng("live/a", t0, t0.Add(8*24*time.Hour)), &r)
		if r.StepSeconds != 3600 || len(r.Points) != 1 || r.Points[0].Viewers != 10 || r.Points[0].BytesOut != 20000 {
			t.Fatalf("hourly series: %+v", r)
		}

		// Whole server.
		e.must(t, "/api/v1/stats/series", rng("-", t0, t0.Add(time.Hour)), &r)
		if r.Path != "" || len(r.Points) != 1 || r.Points[0].Viewers != 99 {
			t.Fatalf("server series: %+v", r)
		}

		// Empty: an array, not null. Default range: the last hour.
		body := e.must(t, "/api/v1/stats/series", url.Values{"path": {"live/none"}}, &r)
		if !strings.Contains(body, `"points": []`) || r.StepSeconds != 60 {
			t.Fatalf("empty series: %s", body)
		}
		insertMinute(t, d, time.Now().Add(-30*time.Minute), "live/recent", 1, 1, 0, 0, 0, 0)
		insertMinute(t, d, time.Now().Add(-90*time.Minute), "live/recent", 1, 1, 0, 0, 0, 0)
		e.must(t, "/api/v1/stats/series", url.Values{"path": {"live/recent"}}, &r)
		if len(r.Points) != 1 {
			t.Fatalf("default range: %+v", r)
		}
	})
}

type summaryResp struct {
	PeakViewers   int64           `json:"peak_viewers"`
	ViewerMinutes int64           `json:"viewer_minutes"`
	BytesIn       int64           `json:"bytes_in"`
	BytesOut      int64           `json:"bytes_out"`
	Streams       []streamSummary `json:"streams"`
}

func TestSummary(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		e := newAPIEnv(t, d, nil)
		for i := 0; i < 3; i++ {
			ts := t0.Add(time.Duration(i) * time.Minute)
			insertMinute(t, d, ts, "live/a", 2, 3, 0, 0, 0, 100)
			insertMinute(t, d, ts, "live/b", 5, 9, 0, 0, 0, 10)
			insertMinute(t, d, ts, "live/c", 0, 0, 0, 0, 0, 1)
			insertMinute(t, d, ts, ServerPath, 7, 11, 0, 0, 40, 111)
		}
		insertMinute(t, d, t0.Add(-time.Hour), ServerPath, 1000, 1000, 0, 0, 0, 0) // outside

		var r summaryResp
		e.must(t, "/api/v1/stats/summary", rng("-", t0, t0.Add(time.Hour)), &r)
		if r.PeakViewers != 11 || r.ViewerMinutes != 21 || r.BytesIn != 120 || r.BytesOut != 333 {
			t.Fatalf("summary: %+v", r)
		}
		want := []streamSummary{
			{"live/b", 15, 9, 30},
			{"live/a", 6, 3, 300},
			{"live/c", 0, 0, 3},
		}
		if fmt.Sprint(r.Streams) != fmt.Sprint(want) {
			t.Fatalf("streams: %+v", r.Streams)
		}

		body := e.must(t, "/api/v1/stats/summary", rng("-", t0.Add(-10*time.Hour), t0.Add(-9*time.Hour)), &r)
		if !strings.Contains(body, `"streams": []`) || r.PeakViewers != 0 {
			t.Fatalf("empty summary: %s", body)
		}
	})
}

type countriesResp struct {
	Enabled   bool             `json:"enabled"`
	Countries []countryMinutes `json:"countries"`
}

type liveResp struct {
	Enabled   bool             `json:"enabled"`
	Countries []countryViewers `json:"countries"`
}

func TestCountries(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		geo, err := OpenGeoIP(writeTestMMDB(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { geo.Close() })
		e := newAPIEnv(t, d, geo)
		for i := 0; i < 2; i++ {
			ts := t0.Add(time.Duration(i) * time.Minute)
			insertCountry(t, d, ts, "live/a", "FR", 3)
			insertCountry(t, d, ts, "live/a", "MK", 4)
			insertCountry(t, d, ts, "live/a", "--", 1)
			insertCountry(t, d, ts, ServerPath, "DE", 1)
		}
		var r countriesResp
		e.must(t, "/api/v1/stats/countries", rng("live/a", t0, t0.Add(time.Hour)), &r)
		want := []countryMinutes{{"MK", 8}, {"FR", 6}, {"--", 2}}
		if !r.Enabled || fmt.Sprint(r.Countries) != fmt.Sprint(want) {
			t.Fatalf("countries: %+v", r)
		}
		e.must(t, "/api/v1/stats/countries", rng("-", t0, t0.Add(time.Hour)), &r)
		if len(r.Countries) != 1 || r.Countries[0] != (countryMinutes{"DE", 2}) {
			t.Fatalf("server countries: %+v", r)
		}
		body := e.must(t, "/api/v1/stats/countries", rng("live/b", t0, t0.Add(time.Hour)), &r)
		if !strings.Contains(body, `"countries": []`) {
			t.Fatalf("empty: %s", body)
		}

		// Live: per stream and summed over every live stream.
		publish(t, e.rec.Hub, "live/a", "rtmp")
		publish(t, e.rec.Hub, "live/b", "rtmp")
		e.live.set("live/a", 3, map[string]int{"FR": 2, "MK": 1})
		e.live.set("live/b", 2, map[string]int{"FR": 1, "--": 1, "DE": 0})
		var lr liveResp
		e.must(t, "/api/v1/stats/countries/live", url.Values{"path": {"live/a"}}, &lr)
		if !lr.Enabled || fmt.Sprint(lr.Countries) != fmt.Sprint([]countryViewers{{"FR", 2}, {"MK", 1}}) {
			t.Fatalf("live live/a: %+v", lr)
		}
		e.must(t, "/api/v1/stats/countries/live", nil, &lr)
		if fmt.Sprint(lr.Countries) != fmt.Sprint([]countryViewers{{"FR", 3}, {"--", 1}, {"MK", 1}}) {
			t.Fatalf("live server: %+v", lr)
		}
	})
}

func TestCountriesDisabled(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		e := newAPIEnv(t, d, nil)
		var r countriesResp
		body := e.must(t, "/api/v1/stats/countries", rng("-", t0, t0.Add(time.Hour)), &r)
		if r.Enabled || !strings.Contains(body, `"countries": []`) {
			t.Fatalf("disabled: %s", body)
		}
		var lr liveResp
		body = e.must(t, "/api/v1/stats/countries/live", nil, &lr)
		if lr.Enabled || !strings.Contains(body, `"countries": []`) {
			t.Fatalf("disabled live: %s", body)
		}
	})
}

func TestValidation(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		e := newAPIEnv(t, d, nil)
		bad := []url.Values{
			{"from": {"2026-01-02T00:00:00Z"}, "to": {"2026-01-01T00:00:00Z"}}, // from after to
			{"from": {"2026-01-01T00:00:00Z"}, "to": {"2026-01-01T00:00:00Z"}}, // empty range
			{"from": {"2024-01-01T00:00:00Z"}, "to": {"2026-01-01T00:00:00Z"}}, // > 400 days
			{"from": {"yesterday"}}, // bad time
			{"to": {"later"}},       // bad time
			{"path": {"../etc"}, "from": {"2026-01-01T00:00:00Z"}},                   // bad path
			{"path": {"live/a/b"}},                                                   // bad path
			{"path": {"live"}},                                                       // bad path
			{"path": {"live/a b"}, "to": {"2026-01-01T00:00:00Z"}},                   // bad path
			{"path": {"live/a"}, "from": {"1767225600000"}, "to": {"1735689600000"}}, // ms, reversed
		}
		for _, q := range bad {
			for _, p := range []string{"/api/v1/stats/series", "/api/v1/stats/countries"} {
				if code, body := e.get(t, p, q); code != http.StatusBadRequest || !strings.Contains(body, `"error"`) {
					t.Errorf("GET %s?%s: %d %s", p, q.Encode(), code, body)
				}
			}
		}
		if code, _ := e.get(t, "/api/v1/stats/summary", bad[0]); code != http.StatusBadRequest {
			t.Errorf("summary: %d", code)
		}
		if code, _ := e.get(t, "/api/v1/stats/countries/live", url.Values{"path": {"x"}}); code != http.StatusBadRequest {
			t.Errorf("countries/live: %d", code)
		}
		// Exactly 400 days is allowed.
		ok := url.Values{"from": {"2025-01-01T00:00:00Z"}}
		ok.Set("to", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(400*24*time.Hour).Format(time.RFC3339))
		if code, body := e.get(t, "/api/v1/stats/series", ok); code != http.StatusOK {
			t.Errorf("400 days: %d %s", code, body)
		}
		// Login required.
		resp, err := http.Get(e.srv.URL + "/api/v1/stats/summary")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous: %d", resp.StatusCode)
		}
	})
}

func TestStepFor(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		step int64
	}{
		{time.Minute, 60}, {6 * time.Hour, 60}, {6*time.Hour + time.Minute, 900},
		{7 * 24 * time.Hour, 900}, {7*24*time.Hour + time.Minute, 3600}, {maxRange, 3600},
	} {
		if got := stepFor(c.d.Milliseconds()); got != c.step {
			t.Errorf("stepFor(%v) = %d, want %d", c.d, got, c.step)
		}
	}
}
