package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// teams-workflow: an Adaptive Card message for a Microsoft Teams Workflows (Power Automate) webhook, org and
// deployment (PAD_NOTIFY_TEAMS_WORKFLOW_URL): the replacement of the Office 365 connector webhooks Microsoft is
// retiring, which the teams kind posts to. The id is singular as the other kinds' are (a channel is one workflow) and
// matches the variable.
//
// The URL is the credential (its sig query parameter); a Workflows trigger cannot verify a signature, so the kind has
// no secret. No host allow-list: Microsoft serves these triggers from regional Logic Apps hosts
// (prod-NN.<region>.logic.azure.com), per-environment Power Platform hosts (<env>.environment.api.powerplatform.com)
// and their sovereign-cloud counterparts, and has changed the shape before; a list would lock tenants out, and a host
// shared by every Azure customer would restrict nothing. An operator who wants hosts pinned sets
// PAD_NOTIFY_ALLOWED_HOSTS.
func init() {
	Register(KindSpec{
		ID:     "teams-workflow",
		Scope:  ScopeOrg | ScopeDeployment,
		Fields: []FieldSpec{urlField(true)},
		Build: func(d Deps, v Values) (AlertNotifier, error) {
			return &TeamsWorkflowNotifier{URL: v["url"], Client: d.Client, Envelope: d.Envelope, MaxAttempts: d.MaxAttempts, Backoff: d.Backoff}, nil
		},
		Public: listedURLValues,
	})
}

// TeamsWorkflowNotifier POSTs a message with an Adaptive Card attachment, what the Teams Workflows template "Post to
// a channel when a webhook request is received" posts, to a Workflows trigger URL.
type TeamsWorkflowNotifier struct {
	URL         string
	Client      *http.Client
	Envelope    Envelope
	MaxAttempts int
	Backoff     time.Duration
}

func (n *TeamsWorkflowNotifier) Name() string { return "teams-workflow" }

// The Workflows webhook's body: a message whose one attachment is an Adaptive Card.
type (
	adaptiveMessage struct {
		Type        string               `json:"type"`
		Attachments []adaptiveAttachment `json:"attachments"`
	}
	adaptiveAttachment struct {
		ContentType string       `json:"contentType"`
		ContentURL  *string      `json:"contentUrl"`
		Content     adaptiveCard `json:"content"`
	}
	adaptiveCard struct {
		Schema  string          `json:"$schema"`
		Type    string          `json:"type"`
		Version string          `json:"version"`
		Body    []richTextBlock `json:"body"`
	}
	// richTextBlock is the card's only element. Teams renders Markdown (bold, italic, lists, links) in a TextBlock and
	// in a FactSet's titles and values, and honours a backslash escape unevenly (the mobile apps show it), so a flow
	// named "[Reset your password](https://evil.example)" would become a link there; a TextRun's text is never
	// Markdown (Adaptive Cards 1.2), so every string, the tenant's above all, is shown as typed.
	richTextBlock struct {
		Type    string    `json:"type"`
		Spacing string    `json:"spacing,omitempty"`
		Inlines []textRun `json:"inlines"`
	}
	textRun struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Weight string `json:"weight,omitempty"`
		Size   string `json:"size,omitempty"`
		Color  string `json:"color,omitempty"`
	}
)

func (n *TeamsWorkflowNotifier) Notify(ctx context.Context, ev Event) error {
	ev = ev.Stamped(n.Envelope.now())
	body, err := json.Marshal(adaptiveMessage{Type: "message", Attachments: []adaptiveAttachment{{
		ContentType: "application/vnd.microsoft.card.adaptive",
		Content: adaptiveCard{
			Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
			Type:    "AdaptiveCard",
			Version: "1.4",
			Body:    adaptiveBody(ev),
		},
	}}})
	if err != nil {
		return err
	}
	return newHTTPDelivery(n.Client, n.URL, body, ev, n.Envelope.deliveryID(), "", n.Envelope, n.MaxAttempts, n.Backoff).post(ctx)
}

// adaptiveBody is what the MessageCard of the teams kind carries, as rich text: the title in the accent colour, the
// message, and one "name: value" line per fact (teamsFacts). A fact without a value (a test event's flow) is left
// out: a TextRun needs text.
func adaptiveBody(ev Event) []richTextBlock {
	run := func(text string) textRun { return textRun{Type: "TextRun", Text: text} }
	body := []richTextBlock{}
	if ev.Title != "" {
		title := run(ev.Title)
		title.Weight, title.Size, title.Color = "Bolder", "Medium", adaptiveColor(ev)
		body = append(body, richTextBlock{Type: "RichTextBlock", Inlines: []textRun{title}})
	}
	if ev.Message != "" {
		body = append(body, richTextBlock{Type: "RichTextBlock", Inlines: []textRun{run(ev.Message)}})
	}
	first := true
	for _, f := range teamsFacts(ev) {
		if f.value == "" {
			continue
		}
		name := run(f.name + ": ")
		name.Weight = "Bolder"
		line := richTextBlock{Type: "RichTextBlock", Inlines: []textRun{name, run(f.value)}}
		if !first {
			line.Spacing = "None"
		}
		first = false
		body = append(body, line)
	}
	return body
}

// adaptiveColor is themeColor's accent in Adaptive Card colours: red for errors, amber otherwise.
func adaptiveColor(ev Event) string {
	if eventHasErrors(ev) {
		return "Attention"
	}
	return "Warning"
}
