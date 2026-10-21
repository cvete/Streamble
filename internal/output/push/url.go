package push

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
)

const redacted = "REDACTED"

// selectPushes returns the push targets that apply to stream s, together
// with its app and stream names.
//
// Every target of the stream's app applies when its Streams list is empty
// or names the stream. Renditions produced by the transcoder (source
// protocol "transcode", e.g. tv1_720p) are only pushed when listed
// explicitly, so a target configured for "every stream" receives the
// original stream once instead of the whole ladder.
func selectPushes(cfg *config.Config, s *core.Stream) (app, name string, out []config.Push) {
	app, name, ok := strings.Cut(s.Path, "/")
	if !ok {
		return "", "", nil
	}
	a := cfg.App(app)
	if a == nil {
		return app, name, nil
	}
	transcoded := s.Source.Protocol == "transcode"
	for _, p := range a.Push {
		if len(p.Streams) == 0 {
			if !transcoded {
				out = append(out, p)
			}
			continue
		}
		for _, n := range p.Streams {
			if n == name {
				out = append(out, p)
				break
			}
		}
	}
	return app, name, out
}

// expand replaces the {app} and {stream} placeholders. App and stream names
// are restricted by config.ValidStreamName, so they are safe in URLs.
func expand(raw, app, stream string) string {
	return strings.NewReplacer("{app}", app, "{stream}", stream).Replace(raw)
}

// targetName returns the display name of a target.
func targetName(p config.Push, expanded string) string {
	if p.Name != "" {
		return p.Name
	}
	u, err := url.Parse(expanded)
	if err != nil || u.Scheme == "" {
		scheme, _, _ := strings.Cut(expanded, "://")
		return scheme
	}
	return u.Scheme + "://" + u.Host
}

// redactURL hides every secret a push URL may carry: user info is removed,
// query values are replaced, and for RTMP the last path segment (the stream
// key in platform URLs such as rtmp://a.rtmp.youtube.com/live2/KEY) and any
// fragment are replaced.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		scheme, _, ok := strings.Cut(raw, "://")
		if !ok {
			return redacted
		}
		return scheme + "://" + redacted
	}
	out := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	if isRTMP(u.Scheme) {
		out.Path = redactLastSegment(u.Path)
		if u.Fragment != "" {
			out.Fragment = redacted
		}
	}
	if u.RawQuery != "" {
		var parts []string
		for _, kv := range strings.Split(u.RawQuery, "&") {
			if kv == "" {
				continue
			}
			k, _, _ := strings.Cut(kv, "=")
			parts = append(parts, k+"="+redacted)
		}
		out.RawQuery = strings.Join(parts, "&")
	}
	return out.String()
}

// redactLastSegment replaces the last segment of paths with at least two
// segments ("/live2/KEY" -> "/live2/REDACTED"). A single segment is the
// RTMP app name and is kept.
func redactLastSegment(p string) string {
	trimmed := strings.TrimSuffix(p, "/")
	i := strings.LastIndex(trimmed, "/")
	if i <= 0 || i == len(trimmed)-1 {
		return p
	}
	return trimmed[:i+1] + redacted
}

// secretsOf lists the strings of a push URL that must never appear in logs
// or in the status API, so error messages from libraries can be scrubbed.
func secretsOf(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	var out []string
	add := func(s string) {
		if len(s) >= 3 { // avoid scrubbing trivial values like "1"
			out = append(out, s)
		}
	}
	if u.User != nil {
		add(u.User.Username())
		if p, ok := u.User.Password(); ok {
			add(p)
		}
	}
	for _, vs := range u.Query() {
		for _, v := range vs {
			add(v)
		}
	}
	if isRTMP(u.Scheme) {
		trimmed := strings.TrimSuffix(u.Path, "/")
		if i := strings.LastIndex(trimmed, "/"); i > 0 {
			add(trimmed[i+1:])
		}
		add(u.Fragment)
	}
	return out
}

// scrub removes secrets from an error message.
func scrub(msg string, secrets []string) string {
	for _, s := range secrets {
		msg = strings.ReplaceAll(msg, s, redacted)
		if e := url.QueryEscape(s); e != s {
			msg = strings.ReplaceAll(msg, e, redacted)
		}
	}
	return msg
}

var (
	rtmpStatusCode = regexp.MustCompile(`\{code ([A-Za-z.]+)\}`)
	rtmpStatusDesc = regexp.MustCompile(`\{description ([^}]*)\}`)
)

// describeError turns RTMP onStatus dumps from the client library
// ("bad result: &{5 ... {code NetStream.Publish.Unauthorized} ...}") into
// a sentence an operator can act on.
func describeError(err error) string {
	msg := err.Error()
	if !strings.Contains(msg, "bad result") {
		return msg
	}
	code := ""
	if m := rtmpStatusCode.FindStringSubmatch(msg); m != nil {
		code = m[1]
	}
	desc := ""
	if m := rtmpStatusDesc.FindStringSubmatch(msg); m != nil {
		desc = strings.TrimSpace(m[1])
	}
	var what string
	switch {
	case strings.HasSuffix(code, "Unauthorized"):
		what = "the target refused the stream key"
	case strings.HasSuffix(code, "BadName"):
		what = "the target refused the stream name, or it is already in use"
	case strings.HasPrefix(code, "NetConnection.Connect"):
		what = "the target refused the connection; check the app name in the URL"
	case code != "":
		what = "the target refused the stream"
	default:
		return msg
	}
	if desc != "" {
		return fmt.Sprintf("%s (%s: %s)", what, code, desc)
	}
	return fmt.Sprintf("%s (%s)", what, code)
}

func isRTMP(scheme string) bool { return scheme == "rtmp" || scheme == "rtmps" }

// rtmpURL converts a push URL into the form gortmplib.Client expects.
//
// gortmplib sends the whole path (and query) as the RTMP "app" and takes
// the stream key from the URL fragment. RTMP servers, including platforms
// and Streamble itself, expect the app to be the leading path and the
// stream key (the "publish" name) to be the last segment, optionally with a
// query string. So:
//
//	rtmp://a.rtmp.youtube.com/live2/KEY  -> app "live2", key "KEY"
//	rtmp://host:1935/live/tv1?key=secret -> app "live",  key "tv1?key=secret"
//
// A URL that already has a fragment is used as is (app = path, key =
// fragment), which allows unusual layouts.
func rtmpURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	if u.Fragment != "" {
		return u, nil
	}
	p := strings.TrimSuffix(u.Path, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return nil, errors.New("URL path must be /<app>/<stream key>")
	}
	key := p[i+1:]
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return &url.URL{
		Scheme:   u.Scheme,
		User:     u.User,
		Host:     u.Host,
		Path:     p[:i],
		Fragment: key,
	}, nil
}

// RedactURL hides credentials, query values and (for RTMP) the stream key
// of a push target URL, for display.
func RedactURL(raw string) string { return redactURL(raw) }
