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
	cfg *config.Config
}

// New creates an Authorizer.
func New(cfg *config.Config) *Authorizer { return &Authorizer{cfg: cfg} }

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
	app := a.cfg.App(appName)
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
