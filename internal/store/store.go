// Package store persists runtime settings, users, sessions, API tokens, the
// audit log, statistics and webhook deliveries.
//
// It supports SQLite (default: one file, no setup) and PostgreSQL with the
// same SQL. Queries are written with "?" placeholders and rewritten to
// "$1, $2, ..." for PostgreSQL; only portable types are used (TEXT, BIGINT;
// times are Unix milliseconds; booleans are 0/1).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	_ "modernc.org/sqlite"             // registers "sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a unique value (a user name) already exists.
var ErrConflict = errors.New("already exists")

// DB is an open database.
type DB struct {
	sql      *sql.DB
	postgres bool
}

// Migration is one schema change. Migrations run once, in ID order.
type Migration struct {
	ID  string // e.g. "0001_core"; sorted lexically
	SQL []string
}

var (
	migMu      sync.Mutex
	migrations []Migration
)

// Register adds migrations. Packages that own tables (stats, webhooks)
// call it from init, so the schema grows without a central file.
func Register(m ...Migration) {
	migMu.Lock()
	defer migMu.Unlock()
	migrations = append(migrations, m...)
}

// Open connects and applies pending migrations.
func Open(driver, dsn string) (*DB, error) {
	var (
		db  *sql.DB
		err error
	)
	switch driver {
	case "sqlite":
		if dir := filepath.Dir(dsn); dir != "." && dsn != ":memory:" {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("creating database directory: %w", err)
			}
		}
		// WAL lets the panel read while stats are written; busy_timeout
		// waits for locks instead of failing.
		q := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		db, err = sql.Open("sqlite", "file:"+dsn+sep+q)
		if err == nil {
			// SQLite allows one writer; a single connection avoids
			// "database is locked" between our own goroutines.
			db.SetMaxOpenConns(1)
		}
	case "postgres":
		db, err = sql.Open("pgx", dsn)
		if err == nil {
			db.SetMaxOpenConns(10)
		}
	default:
		return nil, fmt.Errorf("unknown storage driver %q", driver)
	}
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to %s: %w", driver, err)
	}

	d := &DB{sql: db, postgres: driver == "postgres"}
	if err := d.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// Driver returns "sqlite" or "postgres".
func (d *DB) Driver() string {
	if d.postgres {
		return "postgres"
	}
	return "sqlite"
}

// SQL exposes the connection pool for packages that own tables. Pass every
// query through Q.
func (d *DB) SQL() *sql.DB { return d.sql }

// Q rewrites "?" placeholders for the current driver.
func (d *DB) Q(query string) string {
	if !d.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Exec runs a statement.
func (d *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.sql.ExecContext(ctx, d.Q(query), args...)
}

// Query runs a query.
func (d *DB) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.sql.QueryContext(ctx, d.Q(query), args...)
}

// QueryRow runs a query that returns one row.
func (d *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return d.sql.QueryRowContext(ctx, d.Q(query), args...)
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (id TEXT PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}

	migMu.Lock()
	all := append([]Migration(nil), migrations...)
	migMu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	for _, m := range all {
		var exists int
		err := d.QueryRow(ctx, `SELECT 1 FROM schema_migrations WHERE id = ?`, m.ID).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range m.SQL {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback() //nolint:errcheck
				return fmt.Errorf("migration %s: %w", m.ID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, d.Q(`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`),
			m.ID, Now()); err != nil {
			tx.Rollback() //nolint:errcheck
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Now returns the current time in Unix milliseconds, the unit of every
// timestamp column.
func Now() int64 { return time.Now().UnixMilli() }

// Time converts a Unix-milliseconds column value; 0 becomes the zero time.
func Time(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// NewID returns a random 16-byte hex identifier.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func init() {
	Register(Migration{ID: "0001_core", SQL: []string{
		// Runtime settings: one JSON document per item.
		// kind: app, profile, source, webhook, setting.
		`CREATE TABLE runtime_items (
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			data TEXT NOT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (kind, name)
		)`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL,
			disabled BIGINT NOT NULL DEFAULT 0,
			must_change_password BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			last_login_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE sessions (
			id_hash TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at BIGINT NOT NULL,
			expires_at BIGINT NOT NULL,
			last_seen_at BIGINT NOT NULL,
			ip TEXT NOT NULL,
			user_agent TEXT NOT NULL
		)`,
		`CREATE INDEX sessions_user ON sessions (user_id)`,
		`CREATE TABLE api_tokens (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			prefix TEXT NOT NULL,
			created_at BIGINT NOT NULL,
			last_used_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX api_tokens_user ON api_tokens (user_id)`,
		`CREATE TABLE audit_log (
			id TEXT PRIMARY KEY,
			at BIGINT NOT NULL,
			username TEXT NOT NULL,
			action TEXT NOT NULL,
			target TEXT NOT NULL,
			detail TEXT NOT NULL
		)`,
		`CREATE INDEX audit_log_at ON audit_log (at)`,
		// Statistics, one row per minute per stream; path '' is the server.
		`CREATE TABLE stats_minute (
			ts BIGINT NOT NULL,
			path TEXT NOT NULL,
			viewers BIGINT NOT NULL,
			viewers_max BIGINT NOT NULL,
			bitrate_in_kbps BIGINT NOT NULL,
			bitrate_out_kbps BIGINT NOT NULL,
			bytes_in BIGINT NOT NULL,
			bytes_out BIGINT NOT NULL,
			PRIMARY KEY (ts, path)
		)`,
		`CREATE INDEX stats_minute_path ON stats_minute (path, ts)`,
		`CREATE TABLE stats_country (
			ts BIGINT NOT NULL,
			path TEXT NOT NULL,
			country TEXT NOT NULL,
			viewers BIGINT NOT NULL,
			PRIMARY KEY (ts, path, country)
		)`,
		`CREATE TABLE webhook_deliveries (
			id TEXT PRIMARY KEY,
			webhook TEXT NOT NULL,
			event TEXT NOT NULL,
			at BIGINT NOT NULL,
			status BIGINT NOT NULL,
			attempts BIGINT NOT NULL,
			error TEXT NOT NULL,
			duration_ms BIGINT NOT NULL,
			payload TEXT NOT NULL
		)`,
		`CREATE INDEX webhook_deliveries_at ON webhook_deliveries (webhook, at)`,
	}})
}
