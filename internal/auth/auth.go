// Package auth decides whether a client may publish or play a stream path:
// keys first, then the app's optional auth callback (auth_url).
package auth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cvete/streamble/internal/config"
)

// Errors returned by Authorize. Ingest modules map them to protocol
// specific rejections.
var (
	ErrBadPath      = errors.New("invalid stream path, expected <app>/<stream>")
	ErrUnknownApp   = errors.New("unknown app")
	ErrUnauthorized = errors.New("invalid publish key")
	// ErrDenied means the app's auth callback refused the request, did not
	// answer in time or could not be reached (it fails closed).
	ErrDenied = errors.New("refused by the auth callback")
)

// CallbackTimeout bounds an auth callback request.
const CallbackTimeout = 3 * time.Second

// Authorizer checks publish and play requests against the configured apps.
type Authorizer struct {
	cfg config.Getter
	// Client sends auth callbacks. Optional; the default does not follow
	// redirects (a redirect answer refuses the request).
	Client *http.Client
	// Log receives callback decisions. Optional; defaults to slog.Default.
	Log *slog.Logger
}

// Request describes a publish or play attempt.
type Request struct {
	Action     string // "publish" or "play"; set by Publish and Play
	Path       string // as given by the client, "app/stream"
	Key        string // key given by the client
	Protocol   string // "rtmp", "srt" or "http"
	RemoteAddr string // client address, host:port
}

var defaultClient = &http.Client{
	Timeout: CallbackTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
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
// It is Publish without a context or client details.
func (a *Authorizer) AuthorizePublish(path, key string) (string, *config.App, error) {
	return a.Publish(context.Background(), Request{Path: path, Key: key})
}

// AuthorizePlay validates a playback request (RTMP, SRT, HTTP MPEG-TS,
// HTTP audio) and returns the normalised path. It does not check that the
// stream is live. It is Play without a context or client details.
func (a *Authorizer) AuthorizePlay(path, key string) (string, *config.App, error) {
	return a.Play(context.Background(), Request{Path: path, Key: key})
}

// Publish checks the publish key, then asks the app's auth callback when
// it has one. It returns the normalised stream path and its app.
func (a *Authorizer) Publish(ctx context.Context, r Request) (string, *config.App, error) {
	r.Action = "publish"
	path, app, err := a.checkPublishKey(r.Path, r.Key)
	if err != nil {
		return "", nil, err
	}
	if err := a.callback(ctx, app, path, r); err != nil {
		return "", nil, err
	}
	return path, app, nil
}

// Play checks the app's play_key, then asks the app's auth callback when
// it has one. It does not check that the stream is live.
func (a *Authorizer) Play(ctx context.Context, r Request) (string, *config.App, error) {
	r.Action = "play"
	path, app, err := a.checkPlayKey(r.Path, r.Key)
	if err != nil {
		return "", nil, err
	}
	if err := a.callback(ctx, app, path, r); err != nil {
		return "", nil, err
	}
	return path, app, nil
}

func (a *Authorizer) checkPublishKey(path, key string) (string, *config.App, error) {
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

func (a *Authorizer) checkPlayKey(path, key string) (string, *config.App, error) {
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

// callbackBody is the JSON sent to auth_url (docs/API.md, Auth callbacks).
type callbackBody struct {
	Action     string `json:"action"`
	App        string `json:"app"`
	Stream     string `json:"stream"`
	Path       string `json:"path"`
	Key        string `json:"key"`
	Protocol   string `json:"protocol"`
	RemoteAddr string `json:"remote_addr"`
}

// callback asks app.AuthURL whether the request is allowed. A 2xx answer
// allows it; anything else, including errors and timeouts, refuses it.
func (a *Authorizer) callback(ctx context.Context, app *config.App, path string, r Request) error {
	if app.AuthURL == "" {
		return nil
	}
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	appName, stream, _ := strings.Cut(path, "/")
	body, err := json.Marshal(callbackBody{
		Action: r.Action, App: appName, Stream: stream, Path: path, Key: r.Key,
		Protocol: r.Protocol, RemoteAddr: r.RemoteAddr,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, CallbackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, app.AuthURL, bytes.NewReader(body))
	if err != nil {
		log.Warn("auth callback refused the request: invalid auth_url", "action", r.Action, "path", path)
		return ErrDenied
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Streamble-Auth")

	client := a.Client
	if client == nil {
		client = defaultClient
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// Log the host only: the URL may carry a token.
		log.Warn("auth callback refused the request: no answer", "action", r.Action, "path", path,
			"protocol", r.Protocol, "remote", r.RemoteAddr, "host", req.URL.Host, "err", errCause(err))
		return ErrDenied
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Info("auth callback refused the request", "action", r.Action, "path", path,
			"protocol", r.Protocol, "remote", r.RemoteAddr, "status", resp.StatusCode)
		return ErrDenied
	}
	log.Debug("auth callback allowed the request", "action", r.Action, "path", path,
		"protocol", r.Protocol, "remote", r.RemoteAddr, "status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// errCause drops the URL that *url.Error puts in its message.
func errCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
