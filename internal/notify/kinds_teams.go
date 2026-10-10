package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// teams: a legacy Office 365 MessageCard for a Teams incoming webhook, org and deployment (PAD_NOTIFY_TEAMS_URL).
// The URL is the credential; Teams cannot verify a signature, so the kind has no secret. Microsoft is retiring the
// Office 365 connectors behind these webhooks: teams-workflow is their replacement (kinds_teams_workflow.go).
func init() {
	Register(KindSpec{
		ID:     "teams",
		Scope:  ScopeOrg | ScopeDeployment,
		Fields: []FieldSpec{urlField(true)},
		Build: func(d Deps, v Values) (AlertNotifier, error) {
			return &TeamsNotifier{URL: v["url"], Client: d.Client, Envelope: d.Envelope, MaxAttempts: d.MaxAttempts, Backoff: d.Backoff}, nil
		},
		Public: listedURLValues,
	})
}

// TeamsNotifier POSTs a legacy MessageCard (the format Teams incoming webhooks
// render natively) to a Microsoft Teams incoming-webhook URL.
type TeamsNotifier struct {
	URL         string
	Client      *http.Client
	Envelope    Envelope
	MaxAttempts int
	Backoff     time.Duration
}

func (n *TeamsNotifier) Name() string { return "teams" }

func (n *TeamsNotifier) Notify(ctx context.Context, ev Event) error {
	ev = ev.Stamped(n.Envelope.now())
	var facts []map[string]string
	for _, f := range teamsFacts(ev) {
		facts = append(facts, map[string]string{"name": f.name, "value": f.value})
	}

	card := map[string]any{
		"@type":      "MessageCard",
		"@context":   "http://schema.org/extensions",
		"themeColor": themeColor(ev),
		"summary":    ev.Title,
		"sections": []map[string]any{{
			"activityTitle": ev.Title,
			"text":          ev.Message,
			"facts":         facts,
			// The connector card renders Markdown in a section's title, text and facts: a flow named
			// "[click](https://evil.example)" became a link. Every text here is plain.
			"markdown": false,
		}},
	}
	body, err := json.Marshal(card)
	if err != nil {
		return err
	}
	return newHTTPDelivery(n.Client, n.URL, body, ev, n.Envelope.deliveryID(), "", n.Envelope, n.MaxAttempts, n.Backoff).post(ctx)
}

// teamsFacts are the facts a Teams card lists for ev, both kinds' cards alike: what the event is about, then its
// counts.
func teamsFacts(ev Event) []fact {
	facts := subjectFacts(ev)
	if ev.Type == EventDrift {
		facts = append(facts, fact{"New findings", fmt.Sprintf("%d error(s), %d warning(s)", ev.NewErrors, ev.NewWarnings)})
	}
	if ev.Type == EventHealthRegression {
		facts = append(facts, fact{"Health", fmt.Sprintf("%d → %d", ev.PrevHealth, ev.HealthScore)})
	}
	if ev.Type == EventAnalysisComplete && ev.Analysis != nil {
		a := ev.Analysis
		facts = append(facts,
			fact{"Errors", fmt.Sprintf("%d", a.Errors)},
			fact{"Warnings", fmt.Sprintf("%d", a.Warnings)},
			fact{"Info", fmt.Sprintf("%d", a.Info)},
		)
		if a.HealthScore > 0 {
			facts = append(facts, fact{"Health", fmt.Sprintf("%d/100", a.HealthScore)})
		}
	}
	return facts
}

// themeColor maps an event to a Teams card accent color.
func themeColor(ev Event) string {
	if eventHasErrors(ev) {
		return "D13438" // red — errors present
	}
	return "FFB900" // amber — warnings / regressions
}
