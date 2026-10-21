package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/ingest/pull"
	"github.com/cvete/streamble/internal/output/push"
	"github.com/cvete/streamble/internal/settings"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/users"
)

// Masked replaces secrets in responses to viewers.
const Masked = "********"

var (
	errNotFound = errors.New("not found")
	errExists   = errors.New("an item with this name already exists")
	errRename   = errors.New("renaming is not supported; create a new item and delete the old one")
)

func mask(s string) string {
	if s == "" {
		return ""
	}
	return Masked
}

// redactApp hides keys and target URLs from viewers.
func redactApp(a config.App) config.App {
	a.PublishKey, a.PlayKey, a.SRTPassphrase = mask(a.PublishKey), mask(a.PlayKey), mask(a.SRTPassphrase)
	if a.AuthURL != "" {
		a.AuthURL = pull.RedactURL(a.AuthURL)
	}
	if len(a.StreamKeys) > 0 {
		keys := make(map[string]string, len(a.StreamKeys))
		for k, v := range a.StreamKeys {
			keys[k] = mask(v)
		}
		a.StreamKeys = keys
	}
	pushes := make([]config.Push, len(a.Push))
	for i, p := range a.Push {
		p.URL = push.RedactURL(p.URL)
		pushes[i] = p
	}
	a.Push = pushes
	return a
}

func canSeeSecrets(p *users.Principal) bool { return p.Can(store.RoleOperator) }

// change applies fn through the settings store and answers with respond's
// value, or the right error status.
func (a *API) change(w http.ResponseWriter, r *http.Request, p *users.Principal, code int,
	action, target string, fn func(c *config.Config) error, respond func(c *config.Config) any) {
	cfg, err := a.Settings.Update(r.Context(), fn)
	switch {
	case errors.Is(err, errNotFound):
		Error(w, http.StatusNotFound, err)
		return
	case errors.Is(err, errExists):
		Error(w, http.StatusConflict, err)
		return
	case errors.Is(err, settings.ErrInvalid), errors.Is(err, errRename):
		Error(w, http.StatusBadRequest, err)
		return
	case err != nil:
		Error(w, http.StatusInternalServerError, err)
		return
	}
	a.audit(r, p, action, target, "")
	JSON(w, code, respond(cfg))
}

// ---- apps ----

func (a *API) appOut(p *users.Principal, app config.App) config.App {
	if canSeeSecrets(p) {
		return app
	}
	return redactApp(app)
}

func (a *API) listApps(w http.ResponseWriter, _ *http.Request, p *users.Principal) {
	cfg := a.Settings.Get()
	out := make([]config.App, 0, len(cfg.Apps))
	for _, app := range cfg.Apps {
		out = append(out, a.appOut(p, app))
	}
	JSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (a *API) getApp(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	app := a.Settings.Get().App(r.PathValue("name"))
	if app == nil {
		Error(w, http.StatusNotFound, errNotFound)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"app": a.appOut(p, *app)})
}

func (a *API) createApp(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	var app config.App
	if err := ReadJSON(r, &app); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	a.change(w, r, p, http.StatusCreated, "app.create", app.Name, func(c *config.Config) error {
		if c.App(app.Name) != nil {
			return errExists
		}
		c.Apps = append(c.Apps, app)
		return nil
	}, func(c *config.Config) any { return map[string]any{"app": *c.App(app.Name)} })
}

func (a *API) updateApp(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	name := r.PathValue("name")
	var app config.App
	if err := ReadJSON(r, &app); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	if app.Name == "" {
		app.Name = name
	}
	a.change(w, r, p, http.StatusOK, "app.update", name, func(c *config.Config) error {
		if app.Name != name {
			return errRename
		}
		cur := c.App(name)
		if cur == nil {
			return errNotFound
		}
		keepMaskedSecrets(&app, cur)
		*cur = app
		return nil
	}, func(c *config.Config) any { return map[string]any{"app": *c.App(name)} })
}

// keepMaskedSecrets lets a client send back "********" for secrets it
// was never shown, without overwriting them.
func keepMaskedSecrets(next, cur *config.App) {
	if next.PublishKey == Masked {
		next.PublishKey = cur.PublishKey
	}
	if next.PlayKey == Masked {
		next.PlayKey = cur.PlayKey
	}
	if next.SRTPassphrase == Masked {
		next.SRTPassphrase = cur.SRTPassphrase
	}
	for k, v := range next.StreamKeys {
		if v == Masked {
			next.StreamKeys[k] = cur.StreamKeys[k]
		}
	}
}

func (a *API) deleteApp(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	name := r.PathValue("name")
	a.change(w, r, p, http.StatusOK, "app.delete", name, func(c *config.Config) error {
		for i := range c.Apps {
			if c.Apps[i].Name == name {
				c.Apps = append(c.Apps[:i], c.Apps[i+1:]...)
				return nil
			}
		}
		return errNotFound
	}, func(*config.Config) any { return map[string]any{"ok": true} })
}

// ---- transcode profiles ----

func (a *API) listProfiles(w http.ResponseWriter, _ *http.Request, _ *users.Principal) {
	cfg := a.Settings.Get()
	out := cfg.Transcode.Profiles
	if out == nil {
		out = []config.Profile{}
	}
	JSON(w, http.StatusOK, map[string]any{"profiles": out})
}

func (a *API) getProfile(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	pr := a.Settings.Get().Profile(r.PathValue("name"))
	if pr == nil {
		Error(w, http.StatusNotFound, errNotFound)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"profile": pr})
}

func (a *API) createProfile(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	var pr config.Profile
	if err := ReadJSON(r, &pr); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	a.change(w, r, p, http.StatusCreated, "profile.create", pr.Name, func(c *config.Config) error {
		if c.Profile(pr.Name) != nil {
			return errExists
		}
		c.Transcode.Profiles = append(c.Transcode.Profiles, pr)
		return nil
	}, func(c *config.Config) any { return map[string]any{"profile": c.Profile(pr.Name)} })
}

func (a *API) updateProfile(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	name := r.PathValue("name")
	var pr config.Profile
	if err := ReadJSON(r, &pr); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	if pr.Name == "" {
		pr.Name = name
	}
	a.change(w, r, p, http.StatusOK, "profile.update", name, func(c *config.Config) error {
		if pr.Name != name {
			return errRename
		}
		cur := c.Profile(name)
		if cur == nil {
			return errNotFound
		}
		*cur = pr
		return nil
	}, func(c *config.Config) any { return map[string]any{"profile": c.Profile(name)} })
}

func (a *API) deleteProfile(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	name := r.PathValue("name")
	a.change(w, r, p, http.StatusOK, "profile.delete", name, func(c *config.Config) error {
		ps := c.Transcode.Profiles
		for i := range ps {
			if ps[i].Name == name {
				c.Transcode.Profiles = append(ps[:i], ps[i+1:]...)
				return nil
			}
		}
		return errNotFound
	}, func(*config.Config) any { return map[string]any{"ok": true} })
}

// ---- pull sources ----

func (a *API) sourceOut(p *users.Principal, s config.Source) config.Source {
	if !canSeeSecrets(p) {
		s.URL = pull.RedactURL(s.URL)
	}
	return s
}

func (a *API) listSources(w http.ResponseWriter, _ *http.Request, p *users.Principal) {
	cfg := a.Settings.Get()
	out := make([]config.Source, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		out = append(out, a.sourceOut(p, s))
	}
	JSON(w, http.StatusOK, map[string]any{"sources": out})
}

func sourcePath(r *http.Request) string { return r.PathValue("app") + "/" + r.PathValue("stream") }

func (a *API) getSource(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	s := a.Settings.Get().Source(sourcePath(r))
	if s == nil {
		Error(w, http.StatusNotFound, errNotFound)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"source": a.sourceOut(p, *s)})
}

func (a *API) createSource(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	var s config.Source
	if err := ReadJSON(r, &s); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	a.change(w, r, p, http.StatusCreated, "source.create", s.Path, func(c *config.Config) error {
		if c.Source(s.Path) != nil {
			return errExists
		}
		c.Sources = append(c.Sources, s)
		return nil
	}, func(c *config.Config) any { return map[string]any{"source": c.Source(s.Path)} })
}

func (a *API) updateSource(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	path := sourcePath(r)
	var s config.Source
	if err := ReadJSON(r, &s); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	if s.Path == "" {
		s.Path = path
	}
	a.change(w, r, p, http.StatusOK, "source.update", path, func(c *config.Config) error {
		if s.Path != path {
			return errRename
		}
		cur := c.Source(path)
		if cur == nil {
			return errNotFound
		}
		*cur = s
		return nil
	}, func(c *config.Config) any { return map[string]any{"source": c.Source(path)} })
}

func (a *API) deleteSource(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	path := sourcePath(r)
	a.change(w, r, p, http.StatusOK, "source.delete", path, func(c *config.Config) error {
		for i := range c.Sources {
			if c.Sources[i].Path == path {
				c.Sources = append(c.Sources[:i], c.Sources[i+1:]...)
				return nil
			}
		}
		return errNotFound
	}, func(*config.Config) any { return map[string]any{"ok": true} })
}

// ---- webhooks (admin only, so secrets are visible) ----

func (a *API) listWebhooks(w http.ResponseWriter, _ *http.Request, _ *users.Principal) {
	cfg := a.Settings.Get()
	out := cfg.Webhooks
	if out == nil {
		out = []config.Webhook{}
	}
	JSON(w, http.StatusOK, map[string]any{"webhooks": out, "events": config.Events})
}

func (a *API) getWebhook(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
	wh := a.Settings.Get().Webhook(r.PathValue("name"))
	if wh == nil {
		Error(w, http.StatusNotFound, errNotFound)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"webhook": wh})
}

func (a *API) createWebhook(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	var wh config.Webhook
	if err := ReadJSON(r, &wh); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	a.change(w, r, p, http.StatusCreated, "webhook.create", wh.Name, func(c *config.Config) error {
		if c.Webhook(wh.Name) != nil {
			return errExists
		}
		c.Webhooks = append(c.Webhooks, wh)
		return nil
	}, func(c *config.Config) any { return map[string]any{"webhook": c.Webhook(wh.Name)} })
}

func (a *API) updateWebhook(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	name := r.PathValue("name")
	var wh config.Webhook
	if err := ReadJSON(r, &wh); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	if wh.Name == "" {
		wh.Name = name
	}
	a.change(w, r, p, http.StatusOK, "webhook.update", name, func(c *config.Config) error {
		if wh.Name != name {
			return errRename
		}
		cur := c.Webhook(name)
		if cur == nil {
			return errNotFound
		}
		*cur = wh
		return nil
	}, func(c *config.Config) any { return map[string]any{"webhook": c.Webhook(name)} })
}

func (a *API) deleteWebhook(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	name := r.PathValue("name")
	a.change(w, r, p, http.StatusOK, "webhook.delete", name, func(c *config.Config) error {
		for i := range c.Webhooks {
			if c.Webhooks[i].Name == name {
				c.Webhooks = append(c.Webhooks[:i], c.Webhooks[i+1:]...)
				return nil
			}
		}
		return errNotFound
	}, func(*config.Config) any { return map[string]any{"ok": true} })
}

// ---- playback toggles ----

func (a *API) getPlayback(w http.ResponseWriter, _ *http.Request, _ *users.Principal) {
	JSON(w, http.StatusOK, map[string]any{"playback": a.Settings.Get().Playback})
}

func (a *API) updatePlayback(w http.ResponseWriter, r *http.Request, p *users.Principal) {
	var pb config.Playback
	if err := ReadJSON(r, &pb); err != nil {
		Error(w, http.StatusBadRequest, err)
		return
	}
	a.change(w, r, p, http.StatusOK, "playback.update", "", func(c *config.Config) error {
		c.Playback = pb
		return nil
	}, func(c *config.Config) any { return map[string]any{"playback": c.Playback} })
}

// parseTime accepts RFC 3339 or Unix milliseconds; "" is the zero time.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms)
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// ParseTime is exported for packages that register their own routes.
func ParseTime(s string) time.Time { return parseTime(s) }
