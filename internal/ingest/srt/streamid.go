package srt

import (
	"errors"
	"net/url"
	"strings"
)

// Mode is what an SRT caller wants to do.
type Mode int

// SRT caller modes.
const (
	ModePublish Mode = iota
	ModeRead
)

// StreamID is a parsed SRT stream ID.
type StreamID struct {
	Mode Mode
	Path string
	Key  string
}

var errEmptyStreamID = errors.New("empty stream ID")

// ParseStreamID understands the stream ID formats used by common encoders:
//
//	live/tv1                         publish (default mode)
//	live/tv1?key=secret              publish with key
//	publish:live/tv1?key=secret      explicit mode prefix ("read:" to play)
//	#!::r=live/tv1,m=publish,s=secret   SRT access control syntax
//
// In the access control syntax the key may be given as "s" (session) or
// "key"; mode "request" means read.
func ParseStreamID(sid string) (StreamID, error) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return StreamID{}, errEmptyStreamID
	}

	if strings.HasPrefix(sid, "#!::") {
		out := StreamID{Mode: ModePublish}
		for _, kv := range strings.Split(sid[4:], ",") {
			k, v, _ := strings.Cut(kv, "=")
			switch strings.TrimSpace(k) {
			case "r":
				out.Path = v
			case "m":
				switch v {
				case "publish":
					out.Mode = ModePublish
				case "request", "read", "play":
					out.Mode = ModeRead
				default:
					return StreamID{}, errors.New("unsupported mode " + v)
				}
			case "s", "key":
				out.Key = v
			}
		}
		if out.Path == "" {
			return StreamID{}, errors.New("stream ID has no resource (r=)")
		}
		return out, nil
	}

	out := StreamID{Mode: ModePublish}
	if rest, ok := strings.CutPrefix(sid, "publish:"); ok {
		sid = rest
	} else if rest, ok := strings.CutPrefix(sid, "read:"); ok {
		out.Mode, sid = ModeRead, rest
	}

	path, query, _ := strings.Cut(sid, "?")
	out.Path = path
	if query != "" {
		q, err := url.ParseQuery(query)
		if err != nil {
			return StreamID{}, err
		}
		out.Key = q.Get("key")
	}
	if out.Path == "" {
		return StreamID{}, errEmptyStreamID
	}
	return out, nil
}
