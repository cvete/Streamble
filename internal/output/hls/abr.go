package hls

import (
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

// abrPlaylist is the file name of the adaptive bitrate master playlist.
const abrPlaylist = "abr.m3u8"

// serveABR combines the playlists of every live rendition of a transcoded
// stream into one multivariant playlist, so players switch quality on
// their own. depth is the number of "/" in the request path after
// trimming, used to build relative URIs that also work behind a
// reverse proxy with a path prefix.
func (m *Manager) serveABR(w http.ResponseWriter, r *http.Request, path, variant string, depth int) {
	rendPaths := m.ABR(path)
	if len(rendPaths) == 0 {
		http.Error(w, "stream is not transcoded", http.StatusNotFound)
		return
	}

	// From /app/stream/abr.m3u8 (depth 2) the app directory is "../";
	// from /app/stream/<variant>/abr.m3u8 (depth 3) it is "../../".
	up := strings.Repeat("../", depth-1)

	out := playlist.Multivariant{Version: 3, IndependentSegments: true}
	for _, rp := range rendPaths {
		m.mu.RLock()
		hs := m.streams[rp]
		m.mu.RUnlock()
		if hs == nil || hs.muxers[variant] == nil {
			continue // rendition not live yet
		}
		mv := fetchMultivariant(hs, variant)
		if mv == nil {
			continue // no segments yet
		}
		_, streamName, _ := strings.Cut(rp, "/")
		prefix := up + streamName + "/" + variant + "/"
		suffix := "_" + streamName

		if mv.Version > out.Version {
			out.Version = mv.Version
		}
		for _, rend := range mv.Renditions {
			rend.GroupID += suffix
			if rend.URI != nil {
				u := prefix + *rend.URI
				rend.URI = &u
			}
			out.Renditions = append(out.Renditions, rend)
		}
		for _, v := range mv.Variants {
			v.URI = prefix + v.URI
			if v.Audio != "" {
				v.Audio += suffix
			}
			if v.Video != "" {
				v.Video += suffix
			}
			out.Variants = append(out.Variants, v)
		}
		hs.viewers.touch(clientID(r))
	}

	if len(out.Variants) == 0 {
		http.Error(w, "renditions are not ready yet", http.StatusNotFound)
		return
	}
	buf, err := out.Marshal()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(buf) //nolint:errcheck
}

// fetchMultivariant asks a rendition's muxer for its own multivariant
// playlist. It returns nil until the muxer has produced segments.
func fetchMultivariant(hs *hlsStream, variant string) *playlist.Multivariant {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/index.m3u8", nil)
	hs.muxers[variant].Handle(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		return nil
	}
	var mv playlist.Multivariant
	if err := mv.Unmarshal(rec.Body.Bytes()); err != nil {
		return nil
	}
	return &mv
}
