package auth

import (
	"errors"
	"testing"

	"github.com/cvete/streamble/internal/config"
)

func TestAuthorizePublish(t *testing.T) {
	cfg := config.Default()
	cfg.Apps = []config.App{
		{Name: "open"},
		{Name: "live", PublishKey: "appkey", StreamKeys: map[string]string{"tv1": "tv1key"}},
	}
	a := New(cfg)

	cases := []struct {
		path, key string
		want      error
		wantPath  string
	}{
		{"open/anything", "", nil, "open/anything"},
		{"/live/tv2/", "appkey", nil, "live/tv2"},
		{"live/tv2", "wrong", ErrUnauthorized, ""},
		{"live/tv1", "appkey", ErrUnauthorized, ""}, // stream key overrides app key
		{"live/tv1", "tv1key", nil, "live/tv1"},
		{"nope/tv1", "", ErrUnknownApp, ""},
		{"live", "", ErrBadPath, ""},
		{"live/a/b", "", ErrBadPath, ""},
		{"live/../etc", "", ErrBadPath, ""},
		{"live/..", "", ErrBadPath, ""},
		{"live/.hidden", "", ErrBadPath, ""},
	}
	for _, c := range cases {
		p, _, err := a.AuthorizePublish(c.path, c.key)
		if !errors.Is(err, c.want) {
			t.Errorf("%s key=%q: got err %v, want %v", c.path, c.key, err, c.want)
		}
		if p != c.wantPath {
			t.Errorf("%s: got path %q, want %q", c.path, p, c.wantPath)
		}
	}
}
