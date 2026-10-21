package webhooks

import (
	"errors"
	"net/http"

	"github.com/cvete/streamble/internal/api"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/users"
)

// Register adds the webhook delivery routes (admin only):
//
//	GET  /api/v1/config/webhooks/{name}/deliveries
//	POST /api/v1/config/webhooks/{name}/test
func Register(a *api.API, d *Dispatcher) {
	a.Route("GET /api/v1/config/webhooks/{name}/deliveries", store.RoleAdmin,
		func(w http.ResponseWriter, r *http.Request, _ *users.Principal) {
			name := r.PathValue("name")
			if d.Settings().Webhook(name) == nil {
				api.Error(w, http.StatusNotFound, ErrUnknownWebhook)
				return
			}
			list, err := d.Deliveries(r.Context(), name, 0)
			if err != nil {
				d.Log.Warn("cannot list webhook deliveries", "webhook", name, "err", err)
				api.Error(w, http.StatusInternalServerError, errors.New("cannot read deliveries"))
				return
			}
			api.JSON(w, http.StatusOK, map[string]any{"deliveries": list})
		})

	a.Route("POST /api/v1/config/webhooks/{name}/test", store.RoleAdmin,
		func(w http.ResponseWriter, r *http.Request, p *users.Principal) {
			name := r.PathValue("name")
			del, err := d.Test(r.Context(), name)
			if errors.Is(err, ErrUnknownWebhook) {
				api.Error(w, http.StatusNotFound, err)
				return
			}
			if err != nil {
				api.Error(w, http.StatusInternalServerError, err)
				return
			}
			if a.DB != nil {
				if err := a.DB.AddAudit(r.Context(), p.User.Username, "webhook.test", name, ""); err != nil {
					d.Log.Warn("cannot write audit log", "err", err)
				}
			}
			api.JSON(w, http.StatusOK, del)
		})
}
