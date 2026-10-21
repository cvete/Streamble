package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/config"
)

// forEachDB runs a test against SQLite, and against PostgreSQL when
// STREAMBLE_TEST_POSTGRES is set to a DSN (CI sets it). PostgreSQL tests
// drop and recreate the public schema first.
func forEachDB(t *testing.T, fn func(t *testing.T, d *DB)) {
	t.Run("sqlite", func(t *testing.T) {
		d, err := Open("sqlite", filepath.Join(t.TempDir(), "sub", "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		fn(t, d)
	})
	dsn := os.Getenv("STREAMBLE_TEST_POSTGRES")
	t.Run("postgres", func(t *testing.T) {
		if dsn == "" {
			t.Skip("STREAMBLE_TEST_POSTGRES not set")
		}
		resetPostgres(t, dsn)
		d, err := Open("postgres", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		fn(t, d)
	})
}

func resetPostgres(t *testing.T, dsn string) {
	t.Helper()
	d, err := Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.SQL().Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
}

func TestQRebind(t *testing.T) {
	pg := &DB{postgres: true}
	if got := pg.Q(`SELECT a FROM t WHERE x = ? AND y = ?`); got != `SELECT a FROM t WHERE x = $1 AND y = $2` {
		t.Fatal(got)
	}
	if got := (&DB{}).Q(`x = ?`); got != `x = ?` {
		t.Fatal(got)
	}
}

func TestRuntimeRoundTrip(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		if _, seeded, err := d.LoadRuntime(ctx); err != nil || seeded {
			t.Fatalf("fresh database: seeded=%v err=%v", seeded, err)
		}

		cfg := config.Default()
		cfg.Apps = []config.App{
			{Name: "live", PublishKey: "k", StreamKeys: map[string]string{"tv1": "x"},
				Push: []config.Push{{Name: "yt", URL: "rtmp://a/b/KEY", Streams: []string{"tv1"}}}},
			{Name: "radio", Transcode: "radio"},
		}
		cfg.Transcode.Profiles = []config.Profile{{Name: "radio", GOP: config.Duration(2 * time.Second),
			Renditions: []config.Rendition{{Name: "mp3", AudioOnly: true, AudioCodec: "mp3"}}}}
		cfg.Sources = []config.Source{{Path: "live/relay", URL: "udp://239.1.1.1:5000"}}
		cfg.Webhooks = []config.Webhook{{Name: "ops", URL: "https://x", Events: []string{config.EventStreamEnded}}}
		cfg.Playback.RTMP = false

		if err := d.SaveRuntime(ctx, RuntimeOf(cfg)); err != nil {
			t.Fatal(err)
		}
		r, seeded, err := d.LoadRuntime(ctx)
		if err != nil || !seeded {
			t.Fatalf("seeded=%v err=%v", seeded, err)
		}
		got := config.Default()
		r.Apply(got)
		if len(got.Apps) != 2 || got.App("live").Push[0].URL != "rtmp://a/b/KEY" ||
			got.App("live").StreamKeys["tv1"] != "x" || got.Profile("radio").GOP.D() != 2*time.Second ||
			got.Source("live/relay") == nil || got.Webhook("ops") == nil || got.Playback.RTMP {
			t.Fatalf("round trip lost data: %+v", got)
		}

		// Remove one app and edit another: deletions and updates apply.
		cfg.Apps = cfg.Apps[1:]
		cfg.Apps[0].Description = "FM"
		if err := d.SaveRuntime(ctx, RuntimeOf(cfg)); err != nil {
			t.Fatal(err)
		}
		r, _, _ = d.LoadRuntime(ctx)
		if len(r.Apps) != 1 || r.Apps[0].Description != "FM" {
			t.Fatalf("update not applied: %+v", r.Apps)
		}
	})
}

func TestUsersSessionsTokensAudit(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		u := &User{Username: "Cvete", PasswordHash: "h", Role: RoleAdmin, MustChangePassword: true}
		if err := d.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := d.CreateUser(ctx, &User{Username: "cvete", PasswordHash: "h", Role: RoleViewer}); !errors.Is(err, ErrConflict) {
			t.Fatalf("case-insensitive duplicate must conflict, got %v", err)
		}
		got, err := d.GetUserByName(ctx, "CVETE")
		if err != nil || got.ID != u.ID || !got.MustChangePassword || got.Role != RoleAdmin {
			t.Fatalf("lookup: %+v %v", got, err)
		}
		got.MustChangePassword, got.Role = false, RoleOperator
		if err := d.UpdateUser(ctx, got); err != nil {
			t.Fatal(err)
		}
		if n, _ := d.CountUsers(ctx); n != 1 {
			t.Fatalf("count %d", n)
		}

		// Sessions: valid, expired, logout.
		now := time.Now()
		if err := d.CreateSession(ctx, "tok1", Session{UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now, IP: "1.2.3.4", UserAgent: strings.Repeat("x", 500)}); err != nil {
			t.Fatal(err)
		}
		d.CreateSession(ctx, "old", Session{UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(-time.Minute), LastSeenAt: now}) //nolint:errcheck
		if s, err := d.GetSession(ctx, "tok1"); err != nil || s.UserID != u.ID {
			t.Fatalf("session: %v", err)
		}
		if _, err := d.GetSession(ctx, "old"); !errors.Is(err, ErrNotFound) {
			t.Fatal("expired session must not be returned")
		}
		if err := d.TouchSession(ctx, "tok1"); err != nil {
			t.Fatal(err)
		}
		d.DeleteUserSessions(ctx, u.ID, "tok1") //nolint:errcheck
		if _, err := d.GetSession(ctx, "tok1"); err != nil {
			t.Fatal("kept session was deleted")
		}

		// API tokens are stored hashed and resolve to their user.
		tok, err := d.CreateAPIToken(ctx, u.ID, "ci", "stb_abcdefghijklmnop")
		if err != nil || tok.Prefix != "stb_abcdefgh" {
			t.Fatalf("token: %+v %v", tok, err)
		}
		var stored string
		d.QueryRow(ctx, `SELECT token_hash FROM api_tokens WHERE id = ?`, tok.ID).Scan(&stored) //nolint:errcheck
		if strings.Contains(stored, "abcdefgh") {
			t.Fatal("token stored in clear text")
		}
		if owner, err := d.UserByAPIToken(ctx, "stb_abcdefghijklmnop"); err != nil || owner.ID != u.ID {
			t.Fatalf("token lookup: %v", err)
		}
		if _, err := d.UserByAPIToken(ctx, "wrong"); !errors.Is(err, ErrNotFound) {
			t.Fatal("unknown token must not resolve")
		}

		// Audit log, newest first.
		d.AddAudit(ctx, "cvete", "app.create", "live", "") //nolint:errcheck
		time.Sleep(2 * time.Millisecond)
		d.AddAudit(ctx, "cvete", "app.delete", "radio", "") //nolint:errcheck
		entries, err := d.ListAudit(ctx, 10, time.Time{})
		if err != nil || len(entries) != 2 || entries[0].Action != "app.delete" {
			t.Fatalf("audit: %+v %v", entries, err)
		}

		// Deleting a user cascades to sessions and tokens.
		if err := d.DeleteUser(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := d.GetSession(ctx, "tok1"); !errors.Is(err, ErrNotFound) {
			t.Fatal("session survived user deletion")
		}
		if toks, _ := d.ListAPITokens(ctx, u.ID); len(toks) != 0 {
			t.Fatal("tokens survived user deletion")
		}
		if err := d.DeleteUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("expected ErrNotFound")
		}
	})
}

func TestMigrationsRunOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	d, err := Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open("sqlite", path) // second open must not re-run migrations
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	d.QueryRow(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n) //nolint:errcheck
	if n == 0 {
		t.Fatal("no migrations recorded")
	}
}
