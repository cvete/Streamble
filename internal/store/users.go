package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Roles, from least to most privileged.
const (
	RoleViewer   = "viewer"   // sees everything except secrets
	RoleOperator = "operator" // manages apps, streams, profiles, restreams, sources
	RoleAdmin    = "admin"    // also users, webhooks, settings
)

// Roles lists every role.
var Roles = []string{RoleViewer, RoleOperator, RoleAdmin}

// RoleRank orders roles for "at least" checks; unknown roles rank 0.
func RoleRank(role string) int {
	switch role {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// User is a panel/API account.
type User struct {
	ID                 string    `json:"id"`
	Username           string    `json:"username"`
	PasswordHash       string    `json:"-"`
	Role               string    `json:"role"`
	Disabled           bool      `json:"disabled"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	LastLoginAt        time.Time `json:"last_login_at,omitzero"`
}

const userCols = `id, username, password_hash, role, disabled, must_change_password, created_at, updated_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var disabled, must, created, updated, login int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &must, &created, &updated, &login)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Disabled, u.MustChangePassword = disabled != 0, must != 0
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = Time(created), Time(updated), Time(login)
	return &u, nil
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// CountUsers returns the number of users.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a user; ID and timestamps are filled in.
func (d *DB) CreateUser(ctx context.Context, u *User) error {
	// Logins match case-insensitively, so names must be unique that way too.
	if _, err := d.GetUserByName(ctx, u.Username); err == nil {
		return ErrConflict
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	now := Now()
	u.ID = NewID()
	u.CreatedAt, u.UpdatedAt = Time(now), Time(now)
	_, err := d.Exec(ctx, `INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		u.ID, u.Username, u.PasswordHash, u.Role, b2i(u.Disabled), b2i(u.MustChangePassword), now, now)
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// UpdateUser saves role, disabled, password hash and the must-change flag.
func (d *DB) UpdateUser(ctx context.Context, u *User) error {
	res, err := d.Exec(ctx, `UPDATE users SET role = ?, disabled = ?, password_hash = ?, must_change_password = ?, updated_at = ?
		WHERE id = ?`, u.Role, b2i(u.Disabled), u.PasswordHash, b2i(u.MustChangePassword), Now(), u.ID)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// TouchLogin records a successful login.
func (d *DB) TouchLogin(ctx context.Context, id string) error {
	_, err := d.Exec(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, Now(), id)
	return err
}

// DeleteUser removes a user with their sessions and tokens.
func (d *DB) DeleteUser(ctx context.Context, id string) error {
	res, err := d.Exec(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// GetUser finds a user by ID.
func (d *DB) GetUser(ctx context.Context, id string) (*User, error) {
	return scanUser(d.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// GetUserByName finds a user by user name (case-insensitive).
func (d *DB) GetUserByName(ctx context.Context, name string) (*User, error) {
	return scanUser(d.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE LOWER(username) = ?`, strings.ToLower(name)))
}

// ListUsers returns all users sorted by name.
func (d *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := d.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// HashToken returns the stored form of a session or API token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Session is a logged-in browser.
type Session struct {
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
}

// CreateSession stores a session under the hash of token.
func (d *DB) CreateSession(ctx context.Context, token string, s Session) error {
	_, err := d.Exec(ctx, `INSERT INTO sessions (id_hash, user_id, created_at, expires_at, last_seen_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, HashToken(token), s.UserID, s.CreatedAt.UnixMilli(), s.ExpiresAt.UnixMilli(),
		s.LastSeenAt.UnixMilli(), s.IP, truncate(s.UserAgent, 300))
	return err
}

// GetSession returns a valid (unexpired) session.
func (d *DB) GetSession(ctx context.Context, token string) (*Session, error) {
	var s Session
	var created, expires, seen int64
	err := d.QueryRow(ctx, `SELECT user_id, created_at, expires_at, last_seen_at, ip, user_agent FROM sessions
		WHERE id_hash = ? AND expires_at > ?`, HashToken(token), Now()).
		Scan(&s.UserID, &created, &expires, &seen, &s.IP, &s.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, s.ExpiresAt, s.LastSeenAt = Time(created), Time(expires), Time(seen)
	return &s, nil
}

// TouchSession records activity at most once a minute.
func (d *DB) TouchSession(ctx context.Context, token string) error {
	now := Now()
	_, err := d.Exec(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id_hash = ? AND last_seen_at < ?`,
		now, HashToken(token), now-60_000)
	return err
}

// DeleteSession logs a browser out.
func (d *DB) DeleteSession(ctx context.Context, token string) error {
	_, err := d.Exec(ctx, `DELETE FROM sessions WHERE id_hash = ?`, HashToken(token))
	return err
}

// DeleteUserSessions logs a user out everywhere, except keepToken if set.
func (d *DB) DeleteUserSessions(ctx context.Context, userID, keepToken string) error {
	_, err := d.Exec(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?`, userID, HashToken(keepToken))
	return err
}

// DeleteExpiredSessions removes stale sessions.
func (d *DB) DeleteExpiredSessions(ctx context.Context) error {
	_, err := d.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, Now())
	return err
}

// APIToken is a long-lived token for scripts and integrations.
type APIToken struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"` // first characters, to recognise it
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
}

// CreateAPIToken stores the hash of token.
func (d *DB) CreateAPIToken(ctx context.Context, userID, name, token string) (*APIToken, error) {
	t := &APIToken{ID: NewID(), UserID: userID, Name: name, Prefix: token[:min(len(token), 12)], CreatedAt: time.Now()}
	_, err := d.Exec(ctx, `INSERT INTO api_tokens (id, user_id, name, token_hash, prefix, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, userID, name, HashToken(token), t.Prefix, t.CreatedAt.UnixMilli())
	return t, err
}

// ListAPITokens returns a user's tokens.
func (d *DB) ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error) {
	rows, err := d.Query(ctx, `SELECT id, user_id, name, prefix, created_at, last_used_at FROM api_tokens
		WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		var t APIToken
		var created, used int64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &created, &used); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsedAt = Time(created), Time(used)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes a token of a user.
func (d *DB) DeleteAPIToken(ctx context.Context, userID, id string) error {
	res, err := d.Exec(ctx, `DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// UserByAPIToken resolves a bearer token and records its use (at most once
// a minute).
func (d *DB) UserByAPIToken(ctx context.Context, token string) (*User, error) {
	var id, userID string
	var used int64
	err := d.QueryRow(ctx, `SELECT id, user_id, last_used_at FROM api_tokens WHERE token_hash = ?`, HashToken(token)).
		Scan(&id, &userID, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if now := Now(); now-used > 60_000 {
		d.Exec(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, id) //nolint:errcheck
	}
	return d.GetUser(ctx, userID)
}

// AuditEntry records who changed what.
type AuditEntry struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Username string    `json:"username"`
	Action   string    `json:"action"` // e.g. "app.update"
	Target   string    `json:"target"` // e.g. "live"
	Detail   string    `json:"detail"`
}

// AddAudit appends to the audit log. Errors are returned but callers
// usually only log them.
func (d *DB) AddAudit(ctx context.Context, username, action, target, detail string) error {
	_, err := d.Exec(ctx, `INSERT INTO audit_log (id, at, username, action, target, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		NewID(), Now(), username, action, target, truncate(detail, 1000))
	return err
}

// ListAudit returns the newest entries first, before an optional time.
func (d *DB) ListAudit(ctx context.Context, limit int, before time.Time) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	b := Now() + 1
	if !before.IsZero() {
		b = before.UnixMilli()
	}
	rows, err := d.Query(ctx, `SELECT id, at, username, action, target, detail FROM audit_log
		WHERE at < ? ORDER BY at DESC LIMIT ?`, b, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Username, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = Time(at)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func mustAffect(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate key")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
