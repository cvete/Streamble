package users

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/store"
)

func newService(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Service{DB: db, TTL: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func addUser(t *testing.T, s *Service, name, pw, role string) *store.User {
	t.Helper()
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{Username: name, PasswordHash: h, Role: role}
	if err := s.DB.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPasswordPolicy(t *testing.T) {
	if _, err := HashPassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatal("short password accepted")
	}
	h, _ := HashPassword("long enough pw")
	if !CheckPassword(h, "long enough pw") || CheckPassword(h, "wrong password") {
		t.Fatal("password check broken")
	}
}

func TestEnsureAdminFromEnv(t *testing.T) {
	s := newService(t)
	t.Setenv("STREAMBLE_ADMIN_PASSWORD", "admin-password-123")
	ctx := context.Background()
	if err := s.EnsureAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureAdmin(ctx); err != nil { // second call: no-op
		t.Fatal(err)
	}
	u, err := s.DB.GetUserByName(ctx, "admin")
	if err != nil || u.Role != store.RoleAdmin || !u.MustChangePassword {
		t.Fatalf("admin: %+v %v", u, err)
	}
	if _, _, err := s.Login(ctx, "admin", "admin-password-123", "1.1.1.1", "test"); err != nil {
		t.Fatal(err)
	}
}

func TestLoginSessionAndCookieAuth(t *testing.T) {
	s := newService(t)
	addUser(t, s, "op", "operator-password", store.RoleOperator)
	ctx := context.Background()

	if _, _, err := s.Login(ctx, "op", "bad password!", "2.2.2.2", ""); !errors.Is(err, ErrInvalidLogin) {
		t.Fatal("bad password accepted")
	}
	if _, _, err := s.Login(ctx, "nobody", "whatever-pass", "2.2.2.2", ""); !errors.Is(err, ErrInvalidLogin) {
		t.Fatal("unknown user must look like a bad password")
	}
	token, u, err := s.Login(ctx, "OP", "operator-password", "2.2.2.2", "ua")
	if err != nil || u.Username != "op" {
		t.Fatalf("login: %v", err)
	}

	rec := httptest.NewRecorder()
	s.SetCookie(rec, httptest.NewRequest(http.MethodPost, "/", nil), token)
	cookie := rec.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure {
		t.Fatal("cookie must be HttpOnly and SameSite=Lax, and not Secure over plain HTTP")
	}
	https := httptest.NewRequest(http.MethodPost, "/", nil)
	https.Header.Set("X-Forwarded-Proto", "https")
	rec2 := httptest.NewRecorder()
	s.SetCookie(rec2, https, token)
	if !rec2.Result().Cookies()[0].Secure {
		t.Fatal("cookie must be Secure behind an HTTPS proxy")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", nil)
	req.AddCookie(cookie)
	p, err := s.Authenticate(req)
	if err != nil || p.User.Username != "op" || p.ViaToken || !p.Can(store.RoleOperator) || p.Can(store.RoleAdmin) {
		t.Fatalf("authenticate: %+v %v", p, err)
	}
	if err := CheckCSRF(req, p); !errors.Is(err, ErrMissingHeader) {
		t.Fatal("cookie POST without the header must be rejected")
	}
	req.Header.Set(CSRFHeader, "1")
	if err := CheckCSRF(req, p); err != nil {
		t.Fatal(err)
	}

	// Disabled users lose access immediately.
	u.Disabled = true
	s.DB.UpdateUser(ctx, u) //nolint:errcheck
	if _, err := s.Authenticate(req); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("disabled user still authenticated")
	}
}

func TestBearerTokens(t *testing.T) {
	s := newService(t)
	s.LegacyToken = "legacy-secret"
	u := addUser(t, s, "ci", "ci-password-123", store.RoleViewer)
	tok, meta, err := s.CreateAPIToken(context.Background(), u, "grafana")
	if err != nil || !strings.HasPrefix(tok, "stb_") || meta.Prefix != tok[:12] {
		t.Fatalf("token: %q %+v %v", tok, meta, err)
	}

	for _, c := range []struct {
		token string
		role  string
		ok    bool
	}{
		{tok, store.RoleViewer, true},
		{"legacy-secret", store.RoleAdmin, true},
		{"stb_nope", "", false},
	} {
		req := httptest.NewRequest(http.MethodDelete, "/x", nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		p, err := s.Authenticate(req)
		if (err == nil) != c.ok {
			t.Fatalf("%s: err=%v", c.token, err)
		}
		if c.ok && (p.User.Role != c.role || !p.ViaToken || CheckCSRF(req, p) != nil) {
			t.Fatalf("%s: %+v", c.token, p)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	s := newService(t)
	addUser(t, s, "victim", "victim-password", store.RoleAdmin)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		s.Login(ctx, "victim", "guess-number-x", "6.6.6.6", "") //nolint:errcheck
	}
	if _, _, err := s.Login(ctx, "victim", "victim-password", "6.6.6.6", ""); !errors.Is(err, ErrTooManyTries) {
		t.Fatalf("expected rate limit, got %v", err)
	}
	// Another address is not affected.
	if _, _, err := s.Login(ctx, "victim", "victim-password", "7.7.7.7", ""); err != nil {
		t.Fatal(err)
	}
}
