package stats

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestGeoIP(t *testing.T) {
	g, err := OpenGeoIP(writeTestMMDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for ip, want := range map[string]string{
		"203.0.113.5":         "FR",
		"203.0.113.250:4567":  "FR",
		"198.51.100.1":        "MK",
		"::ffff:198.51.100.2": "MK",
		"[2001:db8::1]:443":   "DE",
		"192.0.2.1":           "--", // not in the database
		"127.0.0.1":           "--", // loopback
		"10.1.2.3":            "--", // private
		"192.168.1.10:80":     "--",
		"172.16.0.1":          "--",
		"::1":                 "--",
		"fd00::1":             "--", // unique local
		"fe80::1%eth0":        "--", // link local
		"0.0.0.0":             "--",
		"":                    "--",
		"not an ip":           "--",
	} {
		if got := g.Country(ip); got != want {
			t.Errorf("Country(%q) = %q, want %q", ip, got, want)
		}
	}

	// Concurrent lookups are safe.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if g.Country("203.0.113.9") != "FR" {
					t.Error("concurrent lookup failed")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestGeoIPNil(t *testing.T) {
	var g *GeoIP
	if got := g.Country("203.0.113.5"); got != UnknownCountry {
		t.Fatalf("nil GeoIP: %q", got)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenGeoIPErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenGeoIP(filepath.Join(dir, "missing.mmdb")); err == nil {
		t.Fatal("expected error for a missing file")
	}
	junk := filepath.Join(dir, "junk.mmdb")
	if err := os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGeoIP(junk); err == nil {
		t.Fatal("expected error for an invalid file")
	}
}
