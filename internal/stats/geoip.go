package stats

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// UnknownCountry is reported when an address has no country: private
// networks, addresses missing from the database, or GeoIP disabled.
const UnknownCountry = "--"

// GeoIP maps IP addresses to countries with a MaxMind DB file
// (GeoLite2-Country or any database with the same "country" layout).
// A nil *GeoIP is valid and reports every address as unknown.
type GeoIP struct {
	db *maxminddb.Reader
}

// OpenGeoIP opens a .mmdb file.
func OpenGeoIP(path string) (*GeoIP, error) {
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening GeoIP database: %w", err)
	}
	return &GeoIP{db: db}, nil
}

// Close releases the database.
func (g *GeoIP) Close() error {
	if g == nil || g.db == nil {
		return nil
	}
	return g.db.Close()
}

type geoRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

// Country returns the ISO 3166-1 alpha-2 code of ip ("1.2.3.4" or
// "1.2.3.4:5678"), or UnknownCountry. It is safe for concurrent use.
func (g *GeoIP) Country(ip string) string {
	if g == nil || g.db == nil {
		return UnknownCountry
	}
	addr, ok := parseIP(ip)
	if !ok || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return UnknownCountry
	}
	var rec geoRecord
	if err := g.db.Lookup(addr).Decode(&rec); err != nil {
		return UnknownCountry
	}
	code := rec.Country.ISOCode
	if code == "" {
		code = rec.RegisteredCountry.ISOCode
	}
	if len(code) != 2 {
		return UnknownCountry
	}
	return strings.ToUpper(code)
}

func parseIP(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	if i := strings.IndexByte(s, '%'); i >= 0 { // IPv6 zone
		s = s[:i]
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
