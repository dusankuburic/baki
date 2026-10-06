package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ── Org notification channels ─────────────────────────────────────────
//
// Admin-configured webhook/Teams/Slack destinations; org governance events are
// delivered there in addition to the deployment-global channels (scanner.dispatchOrgChannels).

// channelOut is the client shape — the HMAC secret never leaves the server.
type channelOut struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// redactChannelURL keeps only scheme and host. For Slack and Teams the webhook URL IS the credential (anyone
// holding it can post into the channel), and generic webhooks often carry a token in the path or query, so
// members who cannot manage channels see where events go, not how to send there.
func redactChannelURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "…"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// @Summary      List org notification channels
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Channels"
// @Router       /api/orgs/{id}/channels [get]
func (h *OrgHandler) handleChannelList(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.JSON(w, []channelOut{})
		return
	}
	org := h.requireMember(w, r)
	if org == nil {
		return
	}
	// Only admins (the people who can edit the channel anyway) get the full URL; see redactChannelURL.
	callerID := h.security.CallerID(r)
	isAdmin := false
	for _, m := range org.Members {
		if m.UserID == callerID {
			isAdmin = m.Role == auth.RoleAdmin
			break
		}
	}
	channels, err := h.backend.ListOrgChannels(r.Context(), chi.URLParam(r, "id"), false)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]channelOut, len(channels))
	for i, c := range channels {
		u := c.URL
		if !isAdmin {
			u = redactChannelURL(u)
		}
		out[i] = channelOut{ID: c.ID, Name: c.Name, Kind: c.Kind, URL: u, Enabled: c.Enabled, CreatedAt: c.CreatedAt}
	}
	render.JSON(w, out)
}

// @Summary      Create/update an org notification channel
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Channel"
// @Router       /api/orgs/{id}/channels [post]
func (h *OrgHandler) handleChannelSave(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org channels require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		URL     string `json:"url"`
		Secret  string `json:"secret"`
		Enabled *bool  `json:"enabled"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !notify.ValidChannelKind(req.Kind) {
		render.Error(w, fmt.Errorf("kind must be one of webhook, teams, slack"), http.StatusBadRequest)
		return
	}
	// Fail fast on a bad URL so a typo never silently disables the channel.
	if err := notify.ValidateChannel(r.Context(), req.Kind, req.URL, h.notifyPolicy); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	id := req.ID
	if id == "" {
		id = uuid.NewString()
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	ch := &storageif.OrgChannel{
		ID: id, OrgID: org.ID, Name: req.Name, Kind: req.Kind,
		URL: req.URL, Secret: req.Secret, Enabled: enabled,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.backend.SaveOrgChannel(r.Context(), ch); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgChannelSave, "org", org.ID,
		map[string]string{"kind": req.Kind})
	render.JSON(w, channelOut{ID: ch.ID, Name: ch.Name, Kind: ch.Kind, URL: ch.URL, Enabled: ch.Enabled, CreatedAt: ch.CreatedAt})
}

// @Summary      Delete an org notification channel
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        channelId path string true "Channel ID"
// @Produce      json
// @Success      200 {object} map[string]string "Deleted"
// @Router       /api/orgs/{id}/channels/{channelId} [delete]
func (h *OrgHandler) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org channels require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	if err := h.backend.DeleteOrgChannel(r.Context(), org.ID, chi.URLParam(r, "channelId")); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgChannelDelete, "org", org.ID,
		map[string]string{"channelId": chi.URLParam(r, "channelId")})
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleChannelTest delivers a synthetic event to ONE channel so an admin
// verifies the destination; synchronous (the 4xx/5xx is the point).
// @Summary      Test an org notification channel
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        channelId path string true "Channel ID"
// @Produce      json
// @Success      200 {object} map[string]string "Delivered"
// @Router       /api/orgs/{id}/channels/{channelId}/test [post]
func (h *OrgHandler) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org channels require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	channelID := chi.URLParam(r, "channelId")
	channels, err := h.backend.ListOrgChannels(r.Context(), org.ID, false)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	var target *storageif.OrgChannel
	for _, c := range channels {
		if c.ID == channelID {
			target = c
			break
		}
	}
	if target == nil {
		render.Error(w, fmt.Errorf("channel not found"), http.StatusNotFound)
		return
	}
	n, err := notify.BuildChannelNotifierWith(r.Context(), h.channelClient, target.Kind, target.URL, target.Secret, h.notifyPolicy)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	done := make(chan error, 1)
	// #nosec G118 -- a detached, explicitly bounded context is correct here. The
	// channel test dials an operator-supplied endpoint; binding it to r.Context()
	// would let a client abort mid-dial and leave the delivery in an unknown
	// state, and the handler below already waits on `done` with its own deadline.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// A panicking notifier must not take the process down: convert the panic into the reported error.
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("org channel test panicked", "channel", target.Name, "kind", target.Kind, "recover", r)
				done <- fmt.Errorf("delivery panicked: %v", r)
			}
		}()
		done <- n.Notify(ctx, notify.Event{
			Type:    notify.EventTest,
			Title:   "baki channel test",
			Message: fmt.Sprintf("This is a test delivery to %q (%s). If you can read this, the channel works.", target.Name, target.Kind),
			At:      time.Now().UTC(),
		})
	}()
	if err := <-done; err != nil {
		render.Error(w, fmt.Errorf("delivery failed: %w", err), http.StatusBadGateway)
		return
	}
	render.JSON(w, map[string]string{"status": "delivered"})
}

// registerChannelRoutes wires the org notification-channel sub-routes.
func (h *OrgHandler) registerChannelRoutes(r chi.Router) {
	r.Route("/channels", func(r chi.Router) {
		r.Get("/", h.handleChannelList)
		r.Post("/", h.handleChannelSave)
		r.Post("/{channelId}/test", h.handleChannelTest)
		r.Delete("/{channelId}", h.handleChannelDelete)
	})
}
