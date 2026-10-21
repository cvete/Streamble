package hls

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
	"time"
)

// viewerWindow is how long a client counts as watching after its last
// playlist request. Players refresh playlists every segment (a few seconds).
const viewerWindow = 30 * time.Second

// unknownCountry is reported for viewers whose country is not known.
const unknownCountry = "--"

// viewerTracker estimates concurrent viewers from playlist requests.
//
// Clients are identified by a hash of IP address and User-Agent; nothing
// identifying is stored. Behind a CDN, every request comes from a few edge
// addresses, so counts at the origin will be low; edge servers or CDN logs
// are the source of truth there.
type viewerTracker struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	country   map[string]string // viewer id -> country code, when known
	lastPrune time.Time
}

func newViewerTracker() *viewerTracker {
	return &viewerTracker{seen: make(map[string]time.Time), country: make(map[string]string)}
}

func (v *viewerTracker) touch(id string) { v.touchFrom(id, nil) }

// touchFrom records a request from viewer id. For a viewer not seen in the
// current window, country (when set) is called once to look up its country.
func (v *viewerTracker) touchFrom(id string, country func() string) {
	v.mu.Lock()
	_, known := v.seen[id]
	v.mu.Unlock()
	var code string
	if !known && country != nil {
		code = country() // outside the lock: may read a database file
	}

	now := time.Now()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen[id] = now
	if code != "" {
		v.country[id] = code
	}
	if now.Sub(v.lastPrune) > viewerWindow {
		v.pruneLocked(now)
	}
}

// byCountry returns the current viewers by country code; viewers whose
// country is unknown count as unknownCountry.
func (v *viewerTracker) byCountry() map[string]int {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked(time.Now())
	out := make(map[string]int)
	for id := range v.seen {
		c := v.country[id]
		if c == "" {
			c = unknownCountry
		}
		out[c]++
	}
	return out
}

func (v *viewerTracker) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked(time.Now())
	return len(v.seen)
}

func (v *viewerTracker) pruneLocked(now time.Time) {
	for id, t := range v.seen {
		if now.Sub(t) > viewerWindow {
			delete(v.seen, id)
			delete(v.country, id)
		}
	}
	v.lastPrune = now
}

// remoteIP returns the client address of r. X-Forwarded-For is not
// trusted: that would need a setting naming the trusted proxies.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func clientID(r *http.Request) string {
	host := remoteIP(r)
	sum := sha256.Sum256([]byte(host + "|" + r.UserAgent()))
	return hex.EncodeToString(sum[:8])
}
