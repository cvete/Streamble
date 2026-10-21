package stats

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cvete/streamble/internal/api"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/users"
)

// Query limits.
const (
	defaultRange = time.Hour
	maxRange     = 400 * 24 * time.Hour
)

// Register adds the GET /api/v1/stats/... routes (role viewer). r may be
// nil (no live data); geo may be nil (GeoIP disabled).
func Register(a *api.API, r *Recorder, geo *GeoIP) {
	h := &handlers{db: a.DB, rec: r, geo: geo, now: time.Now}
	view := store.RoleViewer
	a.Route("GET /api/v1/stats/series", view, h.series)
	a.Route("GET /api/v1/stats/summary", view, h.summary)
	a.Route("GET /api/v1/stats/countries", view, h.countries)
	a.Route("GET /api/v1/stats/countries/live", view, h.countriesLive)
}

type handlers struct {
	db  *store.DB
	rec *Recorder
	geo *GeoIP
	now func() time.Time
}

var (
	errBadPath  = errors.New(`path must be "app/stream" or empty for the whole server`)
	errBadTime  = errors.New("from and to must be RFC 3339 or Unix milliseconds")
	errBadRange = errors.New("from must be before to")
	errTooLong  = errors.New("the range is limited to 400 days")
)

// params reads path, from and to. The range is [from, to) in Unix ms.
func (h *handlers) params(r *http.Request, withPath bool) (path string, from, to int64, err error) {
	q := r.URL.Query()
	if withPath {
		path = strings.Trim(q.Get("path"), "/")
		if path != "" && !validPath(path) {
			return "", 0, 0, errBadPath
		}
	}
	end := h.now()
	if s := q.Get("to"); s != "" {
		if end = api.ParseTime(s); end.IsZero() {
			return "", 0, 0, errBadTime
		}
	}
	start := end.Add(-defaultRange)
	if s := q.Get("from"); s != "" {
		if start = api.ParseTime(s); start.IsZero() {
			return "", 0, 0, errBadTime
		}
	}
	if !start.Before(end) {
		return "", 0, 0, errBadRange
	}
	if end.Sub(start) > maxRange {
		return "", 0, 0, errTooLong
	}
	return path, start.UnixMilli(), end.UnixMilli(), nil
}

func validPath(p string) bool {
	app, stream, ok := strings.Cut(p, "/")
	return ok && config.ValidStreamName(app) && config.ValidStreamName(stream)
}

// stepFor picks the bucket size for a range.
func stepFor(rangeMS int64) int64 {
	switch {
	case rangeMS <= (6 * time.Hour).Milliseconds():
		return 60
	case rangeMS <= (7 * 24 * time.Hour).Milliseconds():
		return 900
	default:
		return 3600
	}
}

type point struct {
	TS             time.Time `json:"ts"`
	Viewers        int64     `json:"viewers"`
	ViewersMax     int64     `json:"viewers_max"`
	BitrateInKbps  int64     `json:"bitrate_in_kbps"`
	BitrateOutKbps int64     `json:"bitrate_out_kbps"`
	BytesIn        int64     `json:"bytes_in"`
	BytesOut       int64     `json:"bytes_out"`
}

func (h *handlers) series(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	path, from, to, err := h.params(r, true)
	if err != nil {
		api.Error(w, http.StatusBadRequest, err)
		return
	}
	step := stepFor(to - from)
	points, err := h.loadSeries(r.Context(), path, from, to, step)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]any{"path": path, "step_seconds": step, "points": points})
}

func (h *handlers) loadSeries(ctx context.Context, path string, from, to, step int64) ([]point, error) {
	// Bucket = ts / (step*1000) with integer division (BIGINT on both
	// SQLite and PostgreSQL); averages are computed in Go from the sums so
	// both databases round the same way.
	// The step is one of three constants, so it is written into the query
	// as a literal: PostgreSQL then sees the same expression in SELECT and
	// GROUP BY.
	stepMS := step * 1000
	bucket := "ts / " + strconv.FormatInt(stepMS, 10)
	rows, err := h.db.Query(ctx, `SELECT `+bucket+`, COUNT(*), CAST(SUM(viewers) AS BIGINT), MAX(viewers_max),
			CAST(SUM(bitrate_in_kbps) AS BIGINT), CAST(SUM(bitrate_out_kbps) AS BIGINT), CAST(SUM(bytes_in) AS BIGINT), CAST(SUM(bytes_out) AS BIGINT)
		FROM stats_minute WHERE path = ? AND ts >= ? AND ts < ?
		GROUP BY `+bucket+` ORDER BY `+bucket, path, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []point{}
	for rows.Next() {
		var bucket, n, vSum, vMax, inSum, outSum, bIn, bOut int64
		if err := rows.Scan(&bucket, &n, &vSum, &vMax, &inSum, &outSum, &bIn, &bOut); err != nil {
			return nil, err
		}
		points = append(points, point{
			TS:             time.UnixMilli(bucket * stepMS).UTC(),
			Viewers:        roundDiv(vSum, n),
			ViewersMax:     vMax,
			BitrateInKbps:  roundDiv(inSum, n),
			BitrateOutKbps: roundDiv(outSum, n),
			BytesIn:        bIn,
			BytesOut:       bOut,
		})
	}
	return points, rows.Err()
}

func roundDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return (a + b/2) / b
}

type streamSummary struct {
	Path          string `json:"path"`
	ViewerMinutes int64  `json:"viewer_minutes"`
	PeakViewers   int64  `json:"peak_viewers"`
	BytesOut      int64  `json:"bytes_out"`
}

func (h *handlers) summary(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	_, from, to, err := h.params(r, false)
	if err != nil {
		api.Error(w, http.StatusBadRequest, err)
		return
	}
	rows, err := h.db.Query(r.Context(), `SELECT path, CAST(SUM(viewers) AS BIGINT), MAX(viewers_max), CAST(SUM(bytes_in) AS BIGINT), CAST(SUM(bytes_out) AS BIGINT)
		FROM stats_minute WHERE ts >= ? AND ts < ? GROUP BY path`, from, to)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	out := map[string]any{"peak_viewers": int64(0), "viewer_minutes": int64(0),
		"bytes_in": int64(0), "bytes_out": int64(0)}
	streams := []streamSummary{}
	for rows.Next() {
		var path string
		var vm, peak, bIn, bOut int64
		if err := rows.Scan(&path, &vm, &peak, &bIn, &bOut); err != nil {
			api.Error(w, http.StatusInternalServerError, err)
			return
		}
		if path == ServerPath {
			out["peak_viewers"], out["viewer_minutes"], out["bytes_in"], out["bytes_out"] = peak, vm, bIn, bOut
			continue
		}
		streams = append(streams, streamSummary{Path: path, ViewerMinutes: vm, PeakViewers: peak, BytesOut: bOut})
	}
	if err := rows.Err(); err != nil {
		api.Error(w, http.StatusInternalServerError, err)
		return
	}
	sort.Slice(streams, func(i, j int) bool {
		a, b := streams[i], streams[j]
		if a.ViewerMinutes != b.ViewerMinutes {
			return a.ViewerMinutes > b.ViewerMinutes
		}
		if a.BytesOut != b.BytesOut {
			return a.BytesOut > b.BytesOut
		}
		return a.Path < b.Path
	})
	out["streams"] = streams
	api.JSON(w, http.StatusOK, out)
}

type countryMinutes struct {
	Country       string `json:"country"`
	ViewerMinutes int64  `json:"viewer_minutes"`
}

func (h *handlers) countries(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	path, from, to, err := h.params(r, true)
	if err != nil {
		api.Error(w, http.StatusBadRequest, err)
		return
	}
	rows, err := h.db.Query(r.Context(), `SELECT country, CAST(SUM(viewers) AS BIGINT) AS vm FROM stats_country
		WHERE path = ? AND ts >= ? AND ts < ? GROUP BY country`, path, from, to)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	list := []countryMinutes{}
	for rows.Next() {
		var c countryMinutes
		if err := rows.Scan(&c.Country, &c.ViewerMinutes); err != nil {
			api.Error(w, http.StatusInternalServerError, err)
			return
		}
		list = append(list, c)
	}
	if err := rows.Err(); err != nil {
		api.Error(w, http.StatusInternalServerError, err)
		return
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].ViewerMinutes != list[j].ViewerMinutes {
			return list[i].ViewerMinutes > list[j].ViewerMinutes
		}
		return list[i].Country < list[j].Country
	})
	api.JSON(w, http.StatusOK, map[string]any{"enabled": h.geo != nil, "countries": list})
}

type countryViewers struct {
	Country string `json:"country"`
	Viewers int64  `json:"viewers"`
}

func (h *handlers) countriesLive(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	path := strings.Trim(r.URL.Query().Get("path"), "/")
	if path != "" && !validPath(path) {
		api.Error(w, http.StatusBadRequest, errBadPath)
		return
	}
	list := []countryViewers{}
	enabled := h.geo != nil
	if enabled && h.rec != nil && h.rec.Countries != nil {
		totals := map[string]int64{}
		if path == ServerPath {
			for _, s := range h.rec.Hub.Streams() {
				for c, n := range h.rec.Countries(s.Path) {
					totals[c] += int64(n)
				}
			}
		} else {
			for c, n := range h.rec.Countries(path) {
				totals[c] += int64(n)
			}
		}
		for c, n := range totals {
			if n > 0 {
				list = append(list, countryViewers{Country: c, Viewers: n})
			}
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Viewers != list[j].Viewers {
				return list[i].Viewers > list[j].Viewers
			}
			return list[i].Country < list[j].Country
		})
	}
	api.JSON(w, http.StatusOK, map[string]any{"enabled": enabled, "countries": list})
}
