package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// slack: a Slack incoming-webhook attachment, a format Discord, Mattermost and Teams' Slack-compatible connector
// render too, org and deployment (PAD_NOTIFY_SLACK_URL/_SECRET, the legacy PAD_WEBHOOK_URL/_SECRET). The URL is the
// credential. Signed when the channel has a secret (Slack itself ignores the headers). No host allow-list: the same
// kind posts to self-hosted Mattermost.
func init() {
	Register(KindSpec{
		ID:     "slack",
		Scope:  ScopeOrg | ScopeDeployment,
		Fields: []FieldSpec{urlField(true), {Name: "secret", Secret: true}},
		Signed: true,
		Build: func(d Deps, v Values) (AlertNotifier, error) {
			return &SlackNotifier{URL: v["url"], Client: d.Client, Secret: v["secret"], Envelope: d.Envelope,
				MaxAttempts: d.MaxAttempts, Backoff: d.Backoff}, nil
		},
		Public: listedURLValues,
	})
}

// SlackNotifier POSTs a Slack incoming-webhook attachment.
type SlackNotifier struct {
	URL         string
	Client      *http.Client
	Secret      string
	Envelope    Envelope
	MaxAttempts int
	Backoff     time.Duration
}

func (n *SlackNotifier) Name() string { return "slack" }

func (n *SlackNotifier) Notify(ctx context.Context, ev Event) error {
	ev = ev.Stamped(n.Envelope.now())
	payload := map[string]any{
		"text": slackText(ev.Title),
		"attachments": []map[string]any{{
			"color":  slackColor(ev),
			"text":   slackText(ev.Message),
			"fields": slackFields(ev),
			"footer": "Baki PAD Flow Analyzer",
			"ts":     ev.At.Unix(),
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return newHTTPDelivery(n.Client, n.URL, body, ev, n.Envelope.deliveryID(), n.Secret, n.Envelope, n.MaxAttempts, n.Backoff).post(ctx)
}

// slackText escapes the three characters Slack parses control sequences from (links, @channel and @here mentions) in
// every text it shows, as Slack documents; it displays them as typed. Titles, messages and facts carry tenant text
// (flow names), and the deployment's channels receive every tenant's events.
var slackText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

// slackColor maps an event to a Slack attachment side-stripe color. Slack
// accepts hex ("#D13438") or the named aliases "danger"/"warning"/"good".
func slackColor(ev Event) string {
	if eventHasErrors(ev) {
		return "danger"
	}
	if ev.NewWarnings > 0 || (ev.Analysis != nil && ev.Analysis.Warnings > 0) || ev.Type == EventNewRules {
		return "warning"
	}
	return "good"
}

// slackFields builds the short fields row under a Slack attachment.
func slackFields(ev Event) []map[string]any {
	var fields []map[string]any
	for _, f := range subjectFacts(ev) {
		fields = append(fields, map[string]any{"title": f.name, "value": slackText(f.value), "short": true})
	}
	if ev.Type == EventDrift {
		fields = append(fields,
			map[string]any{"title": "New errors", "value": fmt.Sprintf("%d", ev.NewErrors), "short": true},
			map[string]any{"title": "New warnings", "value": fmt.Sprintf("%d", ev.NewWarnings), "short": true},
		)
	}
	if ev.Type == EventHealthRegression {
		fields = append(fields, map[string]any{"title": "Health", "value": fmt.Sprintf("%d → %d", ev.PrevHealth, ev.HealthScore), "short": true})
	}
	if ev.Type == EventAnalysisComplete && ev.Analysis != nil {
		a := ev.Analysis
		fields = append(fields,
			map[string]any{"title": "Errors", "value": fmt.Sprintf("%d", a.Errors), "short": true},
			map[string]any{"title": "Warnings", "value": fmt.Sprintf("%d", a.Warnings), "short": true},
			map[string]any{"title": "Info", "value": fmt.Sprintf("%d", a.Info), "short": true},
		)
		if a.HealthScore > 0 {
			fields = append(fields, map[string]any{"title": "Health", "value": fmt.Sprintf("%d/100", a.HealthScore), "short": true})
		}
	}
	return fields
}

// fact is one name/value line of a card.
type fact struct{ name, value string }

// subjectFacts is what a card says the event is about: the flow, or for a new-rules notice the number of flows.
func subjectFacts(ev Event) []fact {
	if ev.Type == EventNewRules {
		return []fact{{"Flows", fmt.Sprintf("%d", ev.FlowCount)}}
	}
	return []fact{{"Flow", flowLabel(ev)}}
}

func flowLabel(ev Event) string {
	if ev.FlowName != "" {
		return ev.FlowName
	}
	return ev.FlowID
}

// eventHasErrors reports whether an event represents an error-level signal —
// new errors in a drift, a regression, or errors in an analysis summary. Picks
// the red vs amber accent across all card-rendering channels.
func eventHasErrors(ev Event) bool {
	if ev.NewErrors > 0 {
		return true
	}
	if ev.Type == EventHealthRegression {
		return true // a regression is always at least amber; treat as severe
	}
	return ev.Analysis != nil && ev.Analysis.Errors > 0
}
