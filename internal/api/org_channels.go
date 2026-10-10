package api

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/service"
)

// ── Org notification channels ─────────────────────────────────────────
//
// Admin-configured webhook/Teams/Slack/Jira/email destinations; org governance events are delivered there in
// addition to the deployment's channels. service.ChannelService owns the kinds, validation, redaction and the edit
// semantics (ADR 0021).

// channelOut is the client shape. The URL is redacted for everyone to "host/…" (for Slack and Teams the URL is the
// credential; without a scheme the listing is never a valid URL itself); the signing secret and Jira's API token
// never leave the server, hasSecret and hasToken say whether they are set.
type channelOut struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	HasSecret bool      `json:"hasSecret"`
	// Jira: the account's email and the project key; hasToken whether the API token is set (jira channels only).
	Email    string `json:"email,omitempty"`
	Project  string `json:"project,omitempty"`
	HasToken *bool  `json:"hasToken,omitempty"`
	// Email: the members (user ids) who receive it.
	Recipients []string `json:"recipients,omitempty"`
	// The event types the channel receives (the default set when it never chose).
	Events []string `json:"events"`
	// The stored URL or secrets cannot be decrypted (the encryption key changed): enter the URL again or remove it.
	Unreadable bool `json:"unreadable,omitempty"`
}

func toChannelOut(v service.ChannelView) channelOut {
	out := channelOut{ID: v.ID, Name: v.Name, Kind: v.Kind, URL: v.URL, Enabled: v.Enabled, CreatedAt: v.CreatedAt,
		HasSecret: v.HasSecret, Email: v.Values["email"], Project: v.Values["project"], Events: eventNames(v.Events),
		Unreadable: v.Unreadable}
	if set, ok := v.Secrets["token"]; ok {
		out.HasToken = &set
	}
	if r := v.Values["recipients"]; r != "" {
		out.Recipients = strings.Split(r, ",")
	}
	return out
}

// eventNames is a list of event types as the API names them.
func eventNames[T ~string](ts []T) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

type orgChannelsRequest struct {
	OrgID string `path:"id"`
}

// @Summary      List org notification channels
// @Description  Every channel of the org, to its members. url is redacted to "host/…" (no scheme) for everyone, admins included; hasSecret (and hasToken on jira) says whether a secret is set (never returned). events lists what the channel receives; unreadable marks a channel whose stored URL cannot be decrypted (listed without it: enter it again or remove the channel).
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {array} channelOut "Channels"
// @Router       /api/orgs/{id}/channels [get]
func (h *OrgHandler) listChannels(ctx context.Context, req orgChannelsRequest) ([]channelOut, error) {
	views, err := h.channels.List(ctx, req.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]channelOut, len(views))
	for i, v := range views {
		out[i] = toChannelOut(v)
	}
	return out, nil
}

// channelOptionsOut is what the channel form needs from the deployment. allowedHosts is PAD_NOTIFY_ALLOWED_HOSTS
// when the operator made it visible (PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE); otherwise it is absent and hostsRestricted
// alone says that a list applies.
type channelOptionsOut struct {
	Kinds           []string `json:"kinds"`
	Events          []string `json:"events"`
	DefaultEvents   []string `json:"defaultEvents"`
	HostsRestricted bool     `json:"hostsRestricted"`
	AllowedHosts    []string `json:"allowedHosts,omitempty"`
}

// @Summary      Org notification channel options
// @Description  To org admins: the channel kinds this deployment delivers (email only with mail settings), the events a channel can subscribe to and the default set, and whether channel hosts are restricted (PAD_NOTIFY_ALLOWED_HOSTS). The host list itself is included only when the operator set PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE, since it may name other customers' hosts.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} channelOptionsOut "Options"
// @Router       /api/orgs/{id}/channels/options [get]
func (h *OrgHandler) channelOptions(_ context.Context, _ orgChannelsRequest) (channelOptionsOut, error) {
	c := h.channels.Choices()
	return channelOptionsOut{Kinds: c.Kinds, Events: eventNames(c.Events), DefaultEvents: eventNames(c.DefaultEvents),
		HostsRestricted: c.HostsRestricted, AllowedHosts: c.AllowedHosts}, nil
}

// saveChannelRequest is a create ({kind, url, name?, secret?, enabled?, ...}) or an edit ({id, ...}). An edit keeps
// what it omits: name, enabled, secret and token ("" too), email, project, recipients, events, and the URL when url
// is empty or equals the URL the list shows, with or without urlUnchanged. urlUnchanged with another url, or any
// other url carrying the redaction marker "…", is a 400. clear: ["secret"] removes the secret.
type saveChannelRequest struct {
	OrgID        string   `json:"-" path:"id"`
	ID           string   `json:"id"`
	Name         *string  `json:"name"`
	Kind         string   `json:"kind"`
	URL          string   `json:"url"`
	URLUnchanged bool     `json:"urlUnchanged"`
	Secret       string   `json:"secret"`
	Clear        []string `json:"clear"`
	Enabled      *bool    `json:"enabled"`
	// Jira: the account's email, its API token (write-only) and the project key.
	Email   string `json:"email"`
	Token   string `json:"token"`
	Project string `json:"project"`
	// Email: the members who receive it, by user id; null keeps them.
	Recipients []string `json:"recipients"`
	// The event types the channel receives; null keeps them (a create receives the default set).
	Events []string `json:"events"`
}

// values are the request's kind fields beyond url and secret as the service takes them: a field the request does
// not carry is absent (kept); recipients are present when sent, even empty.
func (r saveChannelRequest) values() map[string]string {
	v := map[string]string{}
	for name, val := range map[string]string{"email": r.Email, "token": r.Token, "project": r.Project} {
		if val != "" {
			v[name] = val
		}
	}
	if r.Recipients != nil {
		v["recipients"] = strings.Join(r.Recipients, ",")
	}
	return v
}

// @Summary      Create or update an org notification channel
// @Description  Create: kind (webhook, slack, teams, teams-workflow, jira, email; GET .../channels/options lists what this deployment delivers) and its fields are required: url for every kind but email; jira adds email, token and project; email takes recipients, member user ids (members with a verified address only, never an address). Edit (id of a saved channel): every omitted field keeps its stored value; secret or token "" keeps it and clear ["secret"] removes the secret; url "" or the url the list shows (with or without urlUnchanged true) keeps the url; urlUnchanged with another url, or any other url containing the redaction marker "…", is refused. events chooses the event types the channel receives (drift, health_regression, new_rules; the test always). A field the kind does not have (a secret on teams or teams-workflow) or a target the deployment refuses (an internal address, a host off PAD_NOTIFY_ALLOWED_HOSTS) is a 400 INVALID_CHANNEL with details.field.
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} channelOut "Channel"
// @Failure      400 {object} render.ErrorResponse "Invalid channel"
// @Router       /api/orgs/{id}/channels [post]
func (h *OrgHandler) saveChannel(ctx context.Context, req saveChannelRequest) (channelOut, error) {
	saved, err := h.channels.Save(ctx, req.OrgID, service.ChannelSave{
		ID: req.ID, Name: req.Name, Kind: req.Kind, URL: req.URL, URLUnchanged: req.URLUnchanged,
		Secret: req.Secret, Clear: req.Clear, Enabled: req.Enabled, Values: req.values(), Events: req.Events,
	})
	if err != nil {
		return channelOut{}, err
	}
	meta := map[string]string{"kind": saved.Kind, "channelId": saved.ID, "enabled": strconv.FormatBool(saved.Enabled)}
	switch {
	case req.Secret != "":
		meta["secret"] = "set"
	case slices.Contains(req.Clear, "secret"):
		meta["secret"] = "cleared"
	}
	if req.Token != "" {
		meta["token"] = "set"
	}
	h.audit.Record(ctx, orgAuditEntry(audit.ActionOrgChannelSave, req.OrgID, meta))
	return toChannelOut(saved), nil
}

type orgChannelRequest struct {
	OrgID     string `path:"id"`
	ChannelID string `path:"channelId"`
}

// @Summary      Delete an org notification channel
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        channelId path string true "Channel ID"
// @Produce      json
// @Success      200 {object} statusResponse "Deleted"
// @Router       /api/orgs/{id}/channels/{channelId} [delete]
func (h *OrgHandler) deleteChannel(ctx context.Context, req orgChannelRequest) (statusResponse, error) {
	if err := h.channels.Delete(ctx, req.OrgID, req.ChannelID); err != nil {
		return statusResponse{}, err
	}
	h.audit.Record(ctx, orgAuditEntry(audit.ActionOrgChannelDelete, req.OrgID, map[string]string{"channelId": req.ChannelID}))
	return statusResponse{Status: "ok"}, nil
}

// @Summary      Test an org notification channel
// @Description  Delivers a synthetic "test" event to the channel and waits for the answer (up to 15 s; an email channel answers once its messages are queued). A failed delivery is a 502 CHANNEL_DELIVERY_FAILED whose details.reason says what failed (details.status: the endpoint's HTTP status); 503 CHANNEL_DELIVERIES_BUSY when the deliveries are saturated; a channel that no longer validates (an internal target, a host off PAD_NOTIFY_ALLOWED_HOSTS, no recipient still a member) is a 400 INVALID_CHANNEL, one whose stored URL cannot be decrypted a 400 CHANNEL_UNREADABLE, and neither is dialled.
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        channelId path string true "Channel ID"
// @Produce      json
// @Success      200 {object} statusResponse "Delivered"
// @Failure      502 {object} render.ErrorResponse "Delivery failed"
// @Router       /api/orgs/{id}/channels/{channelId}/test [post]
func (h *OrgHandler) testChannel(ctx context.Context, req orgChannelRequest) (statusResponse, error) {
	if err := h.channels.Test(ctx, req.OrgID, req.ChannelID); err != nil {
		return statusResponse{}, err
	}
	return statusResponse{Status: "delivered"}, nil
}

// registerChannelRoutes wires the org notification-channel sub-routes, which live in the server store. Reads:
// member. The form's options, writes and the test: admin.
func (h *OrgHandler) registerChannelRoutes(rb *route.Builder) {
	rb.Route("/channels", func(rb *route.Builder) {
		rb.With(h.requireOrg(service.OrgRead)).Get("/", Query(h.listChannels))
		admin := rb.With(h.requireOrg(service.OrgAdmin))
		admin.Get("/options", Query(h.channelOptions))
		admin.Post("/", JSON(h.saveChannel), route.Idempotent())
		// Dials the endpoint for up to 15 s, outside the request transaction, on its own rate-limit bucket: each call
		// makes the server POST to an admin-chosen URL (B13).
		admin.Post("/{channelId}/test", Query(h.testChannel), route.NoTx(), route.RateLimit(route.RateChannelTest))
		admin.Delete("/{channelId}", Query(h.deleteChannel))
	}, route.Needs(capability.ServerStore))
}
