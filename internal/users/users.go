// Package users authenticates panel and API users: passwords, sessions,
// API tokens, roles and login rate limiting.
package users

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cvete/streamble/internal/store"
)

// CookieName is the session cookie.
const CookieName = "streamble_session"

// CSRFHeader must be sent with every state-changing request that is
// authenticated by cookie. Browsers cannot add custom headers to cross-site
// form posts without a CORS preflight, which this server never allows, so
// the header proves the request came from the panel itself.
const CSRFHeader = "X-Streamble-Request"

// MinPasswordLength is enforced for new passwords.
const MinPasswordLength = 10

// API token prefix, so leaked tokens are easy to recognise and scan for.
const tokenPrefix = "stb_"

// Errors.
var (
	ErrInvalidLogin  = errors.New("invalid user name or password")
	ErrTooManyTries  = errors.New("too many failed logins; try again in a few minutes")
	ErrWeakPassword  = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrUnauthorized  = errors.New("login required")
	ErrForbidden     = errors.New("your role does not allow this")
	ErrMissingHeader = errors.New("missing " + CSRFHeader + " header")
)

// Principal is an authenticated caller.
type Principal struct {
	User     *store.User
	ViaToken bool   // API token or legacy token (no cookie, no CSRF check)
	Session  string // session token when logged in by cookie
}

// Can reports whether the caller has at least role.
func (p *Principal) Can(role string) bool {
	return p != nil && store.RoleRank(p.User.Role) >= store.RoleRank(role)
}

// Service authenticates requests.
type Service struct {
	DB            *store.DB
	TTL           time.Duration
	SecureCookies bool
	// LegacyToken is http.api_token from the config file: accepted as an
	// admin bearer token for scripts written for earlier versions.
	LegacyToken string
	Log         *slog.Logger

	limiter limiter
	dummy   []byte
	once    sync.Once
}

func (s *Service) init() {
	s.once.Do(func() {
		// Compared against when the user does not exist, so a login takes
		// the same time either way and names cannot be probed.
		s.dummy, _ = bcrypt.GenerateFromPassword([]byte("streamble-dummy-password"), bcrypt.DefaultCost)
		s.limiter.window = 15 * time.Minute
		s.limiter.max = 10
	})
}

// HashPassword checks the policy and hashes a password.
func HashPassword(pw string) (string, error) {
	if len(pw) < MinPasswordLength {
		return "", ErrWeakPassword
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// CheckPassword compares a password with a stored hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// RandomToken returns a URL-safe random string with n bytes of entropy.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// EnsureAdmin creates the first admin account when there are no users.
// The password comes from STREAMBLE_ADMIN_PASSWORD, or is generated and
// logged once; either way it must be changed at first login.
func (s *Service) EnsureAdmin(ctx context.Context) error {
	n, err := s.DB.CountUsers(ctx)
	if err != nil || n > 0 {
		return err
	}
	pw := os.Getenv("STREAMBLE_ADMIN_PASSWORD")
	generated := pw == ""
	if generated {
		pw = RandomToken(12)
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return fmt.Errorf("STREAMBLE_ADMIN_PASSWORD: %w", err)
	}
	u := &store.User{Username: "admin", PasswordHash: hash, Role: store.RoleAdmin, MustChangePassword: true}
	if err := s.DB.CreateUser(ctx, u); err != nil {
		return err
	}
	if generated {
		// Printed once on purpose: this is the only way to learn it.
		fmt.Fprintf(os.Stderr, "\n  Streamble control panel: first login\n    user:     admin\n    password: %s\n  You will be asked to change it.\n\n", pw)
	}
	s.Log.Info("created the first admin account", "user", "admin", "password_from_env", !generated)
	return nil
}

// Login checks credentials and creates a session. It returns the session
// token to put in the cookie.
func (s *Service) Login(ctx context.Context, username, password, ip, ua string) (string, *store.User, error) {
	s.init()
	if s.limiter.blocked(ip) {
		return "", nil, ErrTooManyTries
	}
	u, err := s.DB.GetUserByName(ctx, username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", nil, err
	}
	hash := s.dummy
	if u != nil {
		hash = []byte(u.PasswordHash)
	}
	ok := bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	if !ok || u == nil || u.Disabled {
		s.limiter.fail(ip)
		s.Log.Warn("failed login", "user", username, "ip", ip)
		return "", nil, ErrInvalidLogin
	}
	s.limiter.reset(ip)

	token := RandomToken(32)
	now := time.Now()
	if err := s.DB.CreateSession(ctx, token, store.Session{
		UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(s.TTL), LastSeenAt: now, IP: ip, UserAgent: ua,
	}); err != nil {
		return "", nil, err
	}
	s.DB.TouchLogin(ctx, u.ID)                                   //nolint:errcheck
	s.DB.DeleteExpiredSessions(ctx)                              //nolint:errcheck
	s.DB.AddAudit(ctx, u.Username, "auth.login", u.Username, ip) //nolint:errcheck
	return token, u, nil
}

// CreateAPIToken issues a token for a user. The token is returned once.
func (s *Service) CreateAPIToken(ctx context.Context, u *store.User, name string) (string, *store.APIToken, error) {
	token := tokenPrefix + RandomToken(32)
	t, err := s.DB.CreateAPIToken(ctx, u.ID, name, token)
	return token, t, err
}

// SetCookie stores the session token in the browser.
func (s *Service) SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/",
		MaxAge: int(s.TTL.Seconds()), HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie removes the session cookie.
func (s *Service) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// Authenticate identifies the caller from a bearer token or the session
// cookie. It returns ErrUnauthorized when neither is valid.
func (s *Service) Authenticate(r *http.Request) (*Principal, error) {
	ctx := r.Context()
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		bearer = strings.TrimSpace(bearer)
		if s.LegacyToken != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(s.LegacyToken)) == 1 {
			return &Principal{ViaToken: true, User: &store.User{ID: "legacy", Username: "api-token", Role: store.RoleAdmin}}, nil
		}
		u, err := s.DB.UserByAPIToken(ctx, bearer)
		if err != nil || u.Disabled {
			return nil, ErrUnauthorized
		}
		return &Principal{User: u, ViaToken: true}, nil
	}

	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, ErrUnauthorized
	}
	sess, err := s.DB.GetSession(ctx, c.Value)
	if err != nil {
		return nil, ErrUnauthorized
	}
	u, err := s.DB.GetUser(ctx, sess.UserID)
	if err != nil || u.Disabled {
		return nil, ErrUnauthorized
	}
	s.DB.TouchSession(ctx, c.Value) //nolint:errcheck
	return &Principal{User: u, Session: c.Value}, nil
}

// CheckCSRF enforces the custom header for cookie-authenticated requests
// that change state.
func CheckCSRF(r *http.Request, p *Principal) error {
	if p.ViaToken {
		return nil
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if r.Header.Get(CSRFHeader) == "" {
		return ErrMissingHeader
	}
	return nil
}

// ClientIP returns the caller's address without the port.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter blocks an IP after too many failed logins in a window.
type limiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	fails  map[string][]time.Time
}

func (l *limiter) prune(ip string, now time.Time) []time.Time {
	ts := l.fails[ip]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = ts
	}
	return ts
}

func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		return false
	}
	return len(l.prune(ip, time.Now())) >= l.max
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = make(map[string][]time.Time)
	}
	now := time.Now()
	l.fails[ip] = append(l.prune(ip, now), now)
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}
