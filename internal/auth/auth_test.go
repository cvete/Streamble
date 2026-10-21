package auth

import (
	"errors"
	"testing"

	"github.com/cvete/streamble/internal/config"
)

func TestAuthorizePlay(t *testing.T) {
	cfg := config.Default()
	cfg.Apps = []config.App{{Name: "open", PublishKey: "pub"}, {Name: "paid", PlayKey: "viewer"}}
	a := New(cfg)
	if _, _, err := a.AuthorizePlay("open/x", ""); err != nil {
		t.Fatalf("open app should allow playback without key: %v", err)
	}
	if _, _, err := a.AuthorizePlay("paid/x", "nope"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if p, _, err := a.AuthorizePlay("/paid/x", "viewer"); err != nil || p != "paid/x" {
		t.Fatalf("got %q %v", p, err)
	}
	if _, _, err := a.AuthorizePlay("open/x", "pub"); err != nil {
		t.Fatal("publish key must not be needed for playback")
	}
}

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
