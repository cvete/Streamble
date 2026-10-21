package settings

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func openDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := store.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestFirstStartImportsThenDatabaseWins(t *testing.T) {
	ctx := context.Background()
	db, path := openDB(t)

	base := config.Default()
	base.Apps = []config.App{{Name: "tv", PublishKey: "k1"}}
	s, err := Open(ctx, base, db, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().App("tv") == nil {
		t.Fatal("first start must import apps from the file")
	}

	if _, err := s.Update(ctx, func(c *config.Config) error {
		c.Apps = append(c.Apps, config.App{Name: "radio"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Restart with a file that no longer lists "radio": the database wins.
	db2, err := store.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s2, err := Open(ctx, base, db2, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Get().App("radio") == nil || s2.Get().App("tv").PublishKey != "k1" {
		t.Fatalf("database settings not restored: %+v", s2.Get().Apps)
	}
}

func TestUpdateValidatesNotifiesAndKeepsServerSettings(t *testing.T) {
	ctx := context.Background()
	db, _ := openDB(t)
	base := config.Default()
	base.HTTP.Listen = ":7000"
	s, err := Open(ctx, base, db, quiet)
	if err != nil {
		t.Fatal(err)
	}

	var calls int
	var sawOld, sawNew *config.Config
	s.Subscribe(func(old, cur *config.Config) { calls++; sawOld, sawNew = old, cur })

	before := s.Get()
	_, err = s.Update(ctx, func(c *config.Config) error {
		c.Apps = append(c.Apps, config.App{Name: "bad/name"})
		return nil
	})
	if !errors.Is(err, ErrInvalid) || calls != 0 || s.Get() != before {
		t.Fatalf("invalid update must be rejected without side effects: %v calls=%d", err, calls)
	}

	cur, err := s.Update(ctx, func(c *config.Config) error {
		c.HTTP.Listen = ":9999" // server setting: ignored
		c.Playback.RTMP = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cur.HTTP.Listen != ":7000" || cur.Playback.RTMP {
		t.Fatalf("server settings must not change at runtime: %+v", cur.HTTP)
	}
	if calls != 1 || sawOld != before || sawNew != cur || s.Get() != cur {
		t.Fatal("subscriber not called with old and new snapshots")
	}
	if !before.Playback.RTMP {
		t.Fatal("the old snapshot was modified")
	}

	fnErr := errors.New("nope")
	if _, err := s.Update(ctx, func(*config.Config) error { return fnErr }); !errors.Is(err, fnErr) {
		t.Fatal("fn error must be returned")
	}
}
