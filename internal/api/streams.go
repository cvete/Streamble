package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/users"
)

// TrackInfo describes one track.
type TrackInfo struct {
	Codec string `json:"codec"`
	Type  string `json:"type"` // video or audio
}

// StreamInfo is the API representation of a live stream.
type StreamInfo struct {
	Path           string      `json:"path"`
	App            string      `json:"app"`
	Name           string      `json:"name"`
	Protocol       string      `json:"protocol"`
	RemoteAddr     string      `json:"remote_addr"`
	StartedAt      time.Time   `json:"started_at"`
	UptimeSeconds  int64       `json:"uptime_seconds"`
	Tracks         []TrackInfo `json:"tracks"`
	BytesIn        uint64      `json:"bytes_in"`
	BytesOut       uint64      `json:"bytes_out"`
	BitrateInKbps  int64       `json:"bitrate_in_kbps"`
	BitrateOutKbps int64       `json:"bitrate_out_kbps"`
	Viewers        int         `json:"viewers"`
	Kickable       bool        `json:"kickable"`
	// Parent is the source stream of a transcoder rendition.
	Parent string `json:"parent,omitempty"`
	// Renditions lists the transcoded renditions of a source stream.
	Renditions []string `json:"renditions,omitempty"`
	HLS        HLSURLs  `json:"hls"`
	// HTTP lists continuous HTTP outputs available for this stream's codecs
	// (relative to the server root): ts, mp3, aac.
	HTTP map[string]string `json:"http"`
}

// HLSURLs lists playback URLs relative to the server root.
type HLSURLs struct {
	Primary  string            `json:"primary"`
	Variants map[string]string `json:"variants"`
	// ABR is the adaptive bitrate playlist of a transcoded stream.
	ABR string `json:"abr,omitempty"`
}

func (a *API) info(s *core.Stream) StreamInfo {
	in, out := a.rates.get(s)
	app, name, _ := strings.Cut(s.Path, "/")
	info := StreamInfo{
		Path:           s.Path,
		App:            app,
		Name:           name,
		Protocol:       s.Source.Protocol,
		RemoteAddr:     s.Source.RemoteAddr,
		StartedAt:      s.StartedAt.UTC(),
		UptimeSeconds:  int64(time.Since(s.StartedAt).Seconds()),
		BytesIn:        s.BytesIn(),
		BytesOut:       s.BytesOut.Load(),
		BitrateInKbps:  in,
		BitrateOutKbps: out,
		Viewers:        a.Viewers(s.Path),
		Kickable:       s.Kickable(),
		HLS:            HLSURLs{Variants: map[string]string{}},
		HTTP:           map[string]string{},
	}
	if s.Source.Protocol == "transcode" {
		info.Parent = s.Source.RemoteAddr
	}

	var hasMP3, hasAAC bool
	for _, t := range s.Tracks {
		typ := "audio"
		if t.Codec.IsVideo() {
			typ = "video"
		}
		switch t.Codec.(type) {
		case *core.MPEGAudio:
			hasMP3 = true
		case *core.AAC:
			hasAAC = true
		}
		info.Tracks = append(info.Tracks, TrackInfo{Codec: t.Codec.Name(), Type: typ})
	}

	variants := a.HLS.Variants(s.Path)
	if len(variants) > 0 {
		info.HLS.Primary = "/" + s.Path + "/index.m3u8"
	}
	for _, v := range variants {
		info.HLS.Variants[v] = "/" + s.Path + "/" + v + "/index.m3u8"
	}
	if a.HLS.ABR != nil {
		if r := a.HLS.ABR(s.Path); len(r) > 0 {
			info.HLS.ABR = "/" + s.Path + "/abr.m3u8"
			info.Renditions = r
		}
	}

	pb := a.Settings.Get().Playback
	if pb.HTTPTS {
		info.HTTP["ts"] = "/" + s.Path + ".ts"
	}
	if pb.HTTPAudio && hasMP3 {
		info.HTTP["mp3"] = "/" + s.Path + ".mp3"
	}
	if pb.HTTPAudio && hasAAC {
		info.HTTP["aac"] = "/" + s.Path + ".aac"
	}
	return info
}

func (a *API) streamInfos() []StreamInfo {
	streams := a.Hub.Streams()
	out := make([]StreamInfo, 0, len(streams))
	for _, s := range streams {
		out = append(out, a.info(s))
	}
	return out
}

func (a *API) listStreams(w http.ResponseWriter, _ *http.Request, _ *users.Principal) {
	JSON(w, http.StatusOK, map[string]any{"streams": a.streamInfos()})
}

func (a *API) lookup(w http.ResponseWriter, r *http.Request) *core.Stream {
	s := a.Hub.Lookup(r.PathValue("app") + "/" + r.PathValue("stream"))
	if s == nil {
		Error(w, http.StatusNotFound, errors.New("stream is not live"))
	}
	return s
}

func (a *API) getStream(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	if s := a.lookup(w, r); s != nil {
		JSON(w, http.StatusOK, a.info(s))
	}
}

func (a *API) kickStream(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	s := a.lookup(w, r)
	if s == nil {
		return
	}
	if !s.Kick() {
		if restart := a.Restart[s.Source.Protocol]; restart != nil && restart(s.Path) {
			a.audit(r, p, "stream.restart", s.Path, "")
			JSON(w, http.StatusOK, map[string]any{"restarted": true})
			return
		}
		Error(w, http.StatusConflict, errors.New("this stream cannot be disconnected; stop its source instead"))
		return
	}
	a.audit(r, p, "stream.kick", s.Path, s.Source.Protocol+" "+s.Source.RemoteAddr)
	JSON(w, http.StatusOK, map[string]any{"kicked": true})
}

// Overview totals for the dashboard.
type Overview struct {
	Streams        int   `json:"streams"`
	Sources        int   `json:"sources"` // streams that are not renditions
	Viewers        int   `json:"viewers"`
	BitrateInKbps  int64 `json:"bitrate_in_kbps"`
	BitrateOutKbps int64 `json:"bitrate_out_kbps"`
}

func overview(infos []StreamInfo) Overview {
	var o Overview
	for _, s := range infos {
		o.Streams++
		if s.Parent == "" {
			o.Sources++
		}
		o.Viewers += s.Viewers
		o.BitrateInKbps += s.BitrateInKbps
		o.BitrateOutKbps += s.BitrateOutKbps
	}
	return o
}

func (a *API) server(w http.ResponseWriter, _ *http.Request, _ *users.Principal) {
	cfg := a.Settings.Get()
	JSON(w, http.StatusOK, map[string]any{
		"version":        Version,
		"source":         SourceURL,
		"uptime_seconds": int64(time.Since(a.started).Seconds()),
		"storage":        a.DB.Driver(),
		"listeners": map[string]any{
			"rtmp": map[string]any{"enabled": cfg.RTMP.Enabled, "listen": cfg.RTMP.Listen},
			"srt":  map[string]any{"enabled": cfg.SRT.Enabled, "listen": cfg.SRT.Listen, "latency_ms": cfg.SRT.LatencyMS},
			"http": map[string]any{"listen": cfg.HTTP.Listen},
		},
		"hls":       cfg.HLS,
		"playback":  cfg.Playback,
		"transcode": map[string]any{"ffmpeg": cfg.Transcode.FFmpeg},
		"geoip":     map[string]any{"enabled": cfg.GeoIP.Database != ""},
		"stats":     cfg.Stats,
		"overview":  overview(a.streamInfos()),
	})
}

// eventStream sends Server-Sent Events: a "snapshot" of every live stream
// with dashboard totals every 2 seconds, plus server events as they happen
// (stream.published, push.state, ...).
func (a *API) eventStream(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer
	w.WriteHeader(http.StatusOK)

	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		rc.SetWriteDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	snapshot := func() bool {
		infos := a.streamInfos()
		return send("snapshot", map[string]any{
			"streams":    infos,
			"overview":   overview(infos),
			"transcodes": a.status("transcodes"),
			"pushes":     a.status("pushes"),
			"sources":    a.status("sources"),
		})
	}

	var evC <-chan events.Event // nil (never ready) without a bus
	if a.Bus != nil {
		sub := a.Bus.Subscribe(64)
		defer sub.Close()
		evC = sub.C()
	}

	if !snapshot() {
		return
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.stop:
			return
		case <-tick.C:
			if !snapshot() {
				return
			}
		case e, ok := <-evC:
			if !ok {
				return
			}
			if !send("event", e) {
				return
			}
		}
	}
}
