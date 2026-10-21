package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/users"
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

func (a *API) login(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := ReadJSON(r, &body); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	token, u, err := a.Users.Login(r.Context(), strings.TrimSpace(body.Username), body.Password,
		users.ClientIP(r), r.UserAgent())
	switch {
	case errors.Is(err, users.ErrTooManyTries):
		Error(w, http.StatusTooManyRequests, err)
		return
	case errors.Is(err, users.ErrInvalidLogin):
		Error(w, http.StatusUnauthorized, err)
		return
	case err != nil:
		Error(w, http.StatusInternalServerError, err)
		return
	}
	a.Users.SetCookie(w, token)
	JSON(w, http.StatusOK, map[string]any{"user": u})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	if p.Session != "" {
		a.DB.DeleteSession(r.Context(), p.Session) //nolint:errcheck
	}
	a.Users.ClearCookie(w)
	JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) me(w http.ResponseWriter, _ *http.Request, p *users.Principal) {
	JSON(w, http.StatusOK, map[string]any{
		"user":        p.User,
		"csrf_header": users.CSRFHeader,
		"roles":       store.Roles,
	})
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	if p.User.ID == "legacy" {
		Error(w, http.StatusBadRequest, errors.New("the legacy API token has no password"))
		return
	}
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := ReadJSON(r, &body); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	if !users.CheckPassword(p.User.PasswordHash, body.Current) {
		Error(w, http.StatusBadRequest, errors.New("the current password is wrong"))
		return
	}
	if body.New == body.Current {
		Error(w, http.StatusBadRequest, errors.New("choose a password different from the current one"))
		return
	}
	hash, err := users.HashPassword(body.New)
	if err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	u := *p.User
	u.PasswordHash, u.MustChangePassword = hash, false
	if err := a.DB.UpdateUser(r.Context(), &u); err != nil {
		Error(w, http.StatusInternalServerError, err)
		return
	}
	// Other browsers logged in with the old password are signed out.
	a.DB.DeleteUserSessions(r.Context(), u.ID, p.Session) //nolint:errcheck
	a.audit(r, p, "user.password", u.Username, "")
	JSON(w, http.StatusOK, map[string]any{"user": &u})
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	toks, err := a.DB.ListAPITokens(r.Context(), p.User.ID)
	if err != nil {
		Error(w, http.StatusInternalServerError, err)
		return
	}
	if toks == nil {
		toks = []*store.APIToken{}
	}
	JSON(w, http.StatusOK, map[string]any{"tokens": toks})
}

func (a *API) createToken(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	if p.User.ID == "legacy" {
		Error(w, http.StatusBadRequest, errors.New("log in as a user to create tokens"))
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := ReadJSON(r, &body); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 64 {
		Error(w, http.StatusBadRequest, errors.New("give the token a name (up to 64 characters)"))
		return
	}
	token, meta, err := a.Users.CreateAPIToken(r.Context(), p.User, body.Name)
	if err != nil {
		Error(w, http.StatusInternalServerError, err)
		return
	}
	a.audit(r, p, "token.create", body.Name, meta.Prefix)
	JSON(w, http.StatusCreated, map[string]any{"token": token, "info": meta})
}

func (a *API) deleteToken(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	if err := a.DB.DeleteAPIToken(r.Context(), p.User.ID, r.PathValue("id")); err != nil {
		notFoundOr500(w, err)
		return
	}
	a.audit(r, p, "token.delete", r.PathValue("id"), "")
	JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	list, err := a.DB.ListUsers(r.Context())
	if err != nil {
		Error(w, http.StatusInternalServerError, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"users": list})
}

func validRole(role string) bool {
	for _, r := range store.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := ReadJSON(r, &body); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	if !usernameRe.MatchString(body.Username) {
		Error(w, http.StatusBadRequest, errors.New("user names are 1 to 64 letters, digits, '.', '_', '-' or '@'"))
		return
	}
	if !validRole(body.Role) {
		Error(w, http.StatusBadRequest, errors.New("role must be viewer, operator or admin"))
		return
	}
	hash, err := users.HashPassword(body.Password)
	if err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	u := &store.User{Username: body.Username, PasswordHash: hash, Role: body.Role, MustChangePassword: true}
	if err := a.DB.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			Error(w, http.StatusConflict, errors.New("a user with this name already exists"))
			return
		}
		Error(w, http.StatusInternalServerError, err)
		return
	}
	a.audit(r, p, "user.create", u.Username, u.Role)
	JSON(w, http.StatusCreated, map[string]any{"user": u})
}

// activeAdmins counts enabled admins other than except.
func (a *API) activeAdmins(ctx context.Context, except string) (int, error) {
	list, err := a.DB.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range list {
		if u.ID != except && u.Role == store.RoleAdmin && !u.Disabled {
			n++
		}
	}
	return n, nil
}

var errLastAdmin = errors.New("at least one active admin must remain")

func (a *API) updateUser(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	var body struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	if err := ReadJSON(r, &body); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	u, err := a.DB.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		notFoundOr500(w, err)
		return
	}
	var changes []string
	if body.Role != nil {
		if !validRole(*body.Role) {
			Error(w, http.StatusBadRequest, errors.New("role must be viewer, operator or admin"))
			return
		}
		changes = append(changes, "role "+u.Role+" -> "+*body.Role)
		u.Role = *body.Role
	}
	if body.Disabled != nil {
		changes = append(changes, map[bool]string{true: "disabled", false: "enabled"}[*body.Disabled])
		u.Disabled = *body.Disabled
	}
	if body.Password != nil {
		hash, err := users.HashPassword(*body.Password)
		if err != nil {
			Error(w, http.StatusBadRequest, err)
			return
		}
		u.PasswordHash, u.MustChangePassword = hash, true
		changes = append(changes, "password reset")
	}
	if u.ID == p.User.ID && (u.Role != store.RoleAdmin || u.Disabled) {
		Error(w, http.StatusBadRequest, errors.New("you cannot demote or disable yourself"))
		return
	}
	if u.Role != store.RoleAdmin || u.Disabled {
		if n, err := a.activeAdmins(r.Context(), u.ID); err == nil && n == 0 {
			Error(w, http.StatusBadRequest, errLastAdmin)
			return
		}
	}
	if err := a.DB.UpdateUser(r.Context(), u); err != nil {
		notFoundOr500(w, err)
		return
	}
	if u.Disabled || body.Password != nil {
		a.DB.DeleteUserSessions(r.Context(), u.ID, "") //nolint:errcheck
	}
	a.audit(r, p, "user.update", u.Username, strings.Join(changes, ", "))
	JSON(w, http.StatusOK, map[string]any{"user": u})
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	id := r.PathValue("id")
	if id == p.User.ID {
		Error(w, http.StatusBadRequest, errors.New("you cannot delete yourself"))
		return
	}
	u, err := a.DB.GetUser(r.Context(), id)
	if err != nil {
		notFoundOr500(w, err)
		return
	}
	if u.Role == store.RoleAdmin {
		if n, err := a.activeAdmins(r.Context(), u.ID); err == nil && n == 0 {
			Error(w, http.StatusBadRequest, errLastAdmin)
			return
		}
	}
	if err := a.DB.DeleteUser(r.Context(), id); err != nil {
		notFoundOr500(w, err)
		return
	}
	a.audit(r, p, "user.delete", u.Username, "")
	JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	entries, err := a.DB.ListAudit(r.Context(), 200, parseTime(r.URL.Query().Get("before")))
	if err != nil {
		Error(w, http.StatusInternalServerError, err)
		return
	}
	if entries == nil {
		entries = []*store.AuditEntry{}
	}
	JSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func notFoundOr500(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		Error(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	Error(w, http.StatusInternalServerError, err)
}
