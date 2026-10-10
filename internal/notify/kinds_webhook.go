package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// webhook: the generic JSON webhook, org and deployment (PAD_NOTIFY_WEBHOOK_URL/_SECRET). The body is the event's
// flat fields plus the envelope's (webhookBody); signed when the channel has a secret.
func init() {
	Register(KindSpec{
		ID:     "webhook",
		Scope:  ScopeOrg | ScopeDeployment,
		Fields: []FieldSpec{urlField(true), {Name: "secret", Secret: true}},
		Signed: true,
		Build: func(d Deps, v Values) (AlertNotifier, error) {
			return &WebhookNotifier{URL: v["url"], Client: d.Client, Secret: v["secret"], OrgID: d.OrgID,
				Envelope: d.Envelope, MaxAttempts: d.MaxAttempts, Backoff: d.Backoff}, nil
		},
		Public: listedURLValues,
	})
}

// webhookBody is the generic webhook's JSON, schema version 1: the envelope's fields, then the event's flat fields
// unchanged since before the envelope, so receivers that parse those keep working.
type webhookBody struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	DeliveryID    string `json:"deliveryId"`
	OrgID         string `json:"orgId,omitempty"`
	Event
}

// WebhookNotifier POSTs the event JSON to a generic endpoint.
type WebhookNotifier struct {
	URL    string
	Client *http.Client
	Secret string
	// OrgID is the organisation of an org channel (the body's orgId); "" on a deployment channel.
	OrgID       string
	Envelope    Envelope
	MaxAttempts int
	Backoff     time.Duration
}

func (n *WebhookNotifier) Name() string { return "webhook" }

func (n *WebhookNotifier) Notify(ctx context.Context, ev Event) error {
	ev = ev.Stamped(n.Envelope.now())
	deliveryID := n.Envelope.deliveryID()
	body, err := json.Marshal(webhookBody{SchemaVersion: SchemaVersion, ID: ev.ID, DeliveryID: deliveryID, OrgID: n.OrgID, Event: ev})
	if err != nil {
		return err
	}
	return newHTTPDelivery(n.Client, n.URL, body, ev, deliveryID, n.Secret, n.Envelope, n.MaxAttempts, n.Backoff).post(ctx)
}
