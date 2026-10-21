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

// viewerTracker estimates concurrent viewers from playlist requests.
//
// Clients are identified by a hash of IP address and User-Agent; nothing
// identifying is stored. Behind a CDN, every request comes from a few edge
// addresses, so counts at the origin will be low; edge servers or CDN logs
// are the source of truth there.
type viewerTracker struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	lastPrune time.Time
}

func newViewerTracker() *viewerTracker {
	return &viewerTracker{seen: make(map[string]time.Time)}
}

func (v *viewerTracker) touch(id string) {
	now := time.Now()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen[id] = now
	if now.Sub(v.lastPrune) > viewerWindow {
		v.pruneLocked(now)
	}
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
		}
	}
	v.lastPrune = now
}

func clientID(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	sum := sha256.Sum256([]byte(host + "|" + r.UserAgent()))
	return hex.EncodeToString(sum[:8])
}
