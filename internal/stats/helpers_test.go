package stats

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"

	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/store"
)

// pgSchema isolates these tests from the store tests, which drop and
// recreate the public schema of the same database.
const pgSchema = "stats_test"

// forEachDB runs a test against SQLite, and against PostgreSQL when
// STREAMBLE_TEST_POSTGRES is set to a DSN. PostgreSQL tests use their own
// schema, recreated for every test.
func forEachDB(t *testing.T, fn func(t *testing.T, d *store.DB)) {
	t.Run("sqlite", func(t *testing.T) {
		d, err := store.Open("sqlite", filepath.Join(t.TempDir(), "stats.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		fn(t, d)
	})
	dsn := os.Getenv("STREAMBLE_TEST_POSTGRES")
	t.Run("postgres", func(t *testing.T) {
		if dsn == "" {
			t.Skip("STREAMBLE_TEST_POSTGRES not set")
		}
		d, err := store.Open("postgres", resetSchema(t, dsn))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		fn(t, d)
	})
}

func resetSchema(t *testing.T, dsn string) string {
	t.Helper()
	d, err := store.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.SQL().Exec(`DROP SCHEMA IF EXISTS ` + pgSchema + ` CASCADE; CREATE SCHEMA ` + pgSchema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + pgSchema
}

// writeTestMMDB writes a GeoIP database mapping 203.0.113.0/24 to FR,
// 198.51.100.0/24 to MK and 2001:db8::/32 to DE.
func writeTestMMDB(t *testing.T) string {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "GeoLite2-Country",
		RecordSize:              24,
		IncludeReservedNetworks: true, // the documentation ranges are reserved
	})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, code := range map[string]string{
		"203.0.113.0/24": "FR", "198.51.100.0/24": "MK", "2001:db8::/32": "DE",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Insert(n, mmdbtype.Map{
			"country": mmdbtype.Map{"iso_code": mmdbtype.String(code)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "test.mmdb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.WriteTo(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// live is a fake source of current viewers, bitrates and countries.
type live struct {
	mu        sync.Mutex
	viewers   map[string]int
	rates     map[string][2]int64
	countries map[string]map[string]int
}

func newLive() *live {
	return &live{viewers: map[string]int{}, rates: map[string][2]int64{}, countries: map[string]map[string]int{}}
}

func (l *live) Viewers(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.viewers[path]
}

func (l *live) Bitrates(s *core.Stream) (int64, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rates[s.Path]
	return r[0], r[1]
}

func (l *live) Countries(path string) map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for c, n := range l.countries[path] {
		out[c] = n
	}
	return out
}

func (l *live) set(path string, viewers int, countries map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.viewers[path] = viewers
	l.countries[path] = countries
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newHub() *core.Hub { return core.NewHub(discard) }

func publish(t *testing.T, hub *core.Hub, path string, src string) *core.Stream {
	t.Helper()
	s, err := hub.Publish(path, []*core.Track{core.NewTrack(&core.H264{})}, core.SourceInfo{Protocol: src})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeBytes(s *core.Stream, n int) {
	s.WriteUnit(&core.Unit{Track: s.Tracks[0], Payload: [][]byte{make([]byte, n)}})
}

type minuteRow struct {
	viewers, viewersMax, in, out, bytesIn, bytesOut int64
}

func readMinute(t *testing.T, d *store.DB, ts int64, path string) (minuteRow, bool) {
	t.Helper()
	var r minuteRow
	err := d.QueryRow(context.Background(), `SELECT viewers, viewers_max, bitrate_in_kbps, bitrate_out_kbps,
		bytes_in, bytes_out FROM stats_minute WHERE ts = ? AND path = ?`, ts, path).
		Scan(&r.viewers, &r.viewersMax, &r.in, &r.out, &r.bytesIn, &r.bytesOut)
	if err != nil {
		return r, false
	}
	return r, true
}

func readCountries(t *testing.T, d *store.DB, ts int64, path string) map[string]int64 {
	t.Helper()
	rows, err := d.Query(context.Background(),
		`SELECT country, viewers FROM stats_country WHERE ts = ? AND path = ?`, ts, path)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var n int64
		if err := rows.Scan(&c, &n); err != nil {
			t.Fatal(err)
		}
		out[c] = n
	}
	return out
}

func count(t *testing.T, d *store.DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
