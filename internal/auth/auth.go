// Package auth decides whether a client may publish to a stream path.
package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/cvete/streamble/internal/config"
)

// Errors returned by Authorize. Ingest modules map them to protocol
// specific rejections.
var (
	ErrBadPath      = errors.New("invalid stream path, expected <app>/<stream>")
	ErrUnknownApp   = errors.New("unknown app")
	ErrUnauthorized = errors.New("invalid publish key")
)

// Authorizer checks publish requests against the configured apps.
type Authorizer struct {
	cfg config.Getter
}

// New creates an Authorizer for a fixed configuration.
func New(cfg *config.Config) *Authorizer { return &Authorizer{cfg: config.Static(cfg)} }

// NewDynamic creates an Authorizer that reads the current configuration on
// every request, so key changes apply to the next connection.
func NewDynamic(get config.Getter) *Authorizer { return &Authorizer{cfg: get} }

// ParsePath splits "app/stream" (leading and trailing slashes ignored).
func ParsePath(p string) (app, stream string, err error) {
	p = strings.Trim(p, "/")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || !config.ValidStreamName(parts[0]) || !config.ValidStreamName(parts[1]) {
		return "", "", fmt.Errorf("%w: %q", ErrBadPath, p)
	}
	return parts[0], parts[1], nil
}

// AuthorizePublish validates a publish request for path with the supplied
// key and returns the normalised stream path ("app/stream") and its app.
func (a *Authorizer) AuthorizePublish(path, key string) (string, *config.App, error) {
	appName, stream, err := ParsePath(path)
	if err != nil {
		return "", nil, err
	}
	app := a.cfg().App(appName)
	if app == nil {
		return "", nil, fmt.Errorf("%w %q", ErrUnknownApp, appName)
	}

	expected := app.PublishKey
	if k, ok := app.StreamKeys[stream]; ok {
		expected = k
	}
	if expected != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(key)) != 1 {
		return "", nil, ErrUnauthorized
	}
	return appName + "/" + stream, app, nil
}

// AuthorizePlay validates a playback request (RTMP, SRT, HTTP MPEG-TS,
// HTTP audio) against the app's play_key and returns the normalised path.
// It does not check that the stream is live.
func (a *Authorizer) AuthorizePlay(path, key string) (string, *config.App, error) {
	appName, stream, err := ParsePath(path)
	if err != nil {
		return "", nil, err
	}
	app := a.cfg().App(appName)
	if app == nil {
		return "", nil, fmt.Errorf("%w %q", ErrUnknownApp, appName)
	}
	if app.PlayKey != "" && subtle.ConstantTimeCompare([]byte(app.PlayKey), []byte(key)) != 1 {
		return "", nil, ErrUnauthorized
	}
	return appName + "/" + stream, app, nil
}
