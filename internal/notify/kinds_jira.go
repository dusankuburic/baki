package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"pad-analyzer/internal/egress"
)

// jira: a Jira Cloud issue per event, for an organisation (its base URL, the account's email, the API token and the
// project key, which an admin sets) and the deployment (PAD_NOTIFY_JIRA_BASE_URL, _EMAIL, _API_TOKEN, _PROJECT). The
// API token is a Basic-auth credential sent in the Authorization header, never in the URL; an organisation's is
// sealed at rest (org_channels.secrets) and never returned. The base URL is checked like every channel URL (https,
// no internal target, PAD_NOTIFY_ALLOWED_HOSTS for an organisation's).
func init() {
	Register(KindSpec{
		ID:    "jira",
		Scope: ScopeOrg | ScopeDeployment,
		Fields: []FieldSpec{
			urlField(false),
			{Name: "email", Required: true, Check: checkJiraUser, CheckScope: ScopeOrg},
			{Name: "token", Secret: true, Required: true},
			{Name: "project", Required: true, Check: checkJiraProject, CheckScope: ScopeOrg},
		},
		Build: func(d Deps, v Values) (AlertNotifier, error) {
			return &JiraNotifier{Base: v["url"], Email: v["email"], Token: v["token"], Project: v["project"],
				Client: d.Client, Envelope: d.Envelope, MaxAttempts: d.MaxAttempts, Backoff: d.Backoff}, nil
		},
		Public: func(v Values) Values {
			return Values{"url": ListedURL(v["url"]), "email": v["email"], "project": v["project"]}
		},
	})
}

// JiraNotifier creates a Jira issue (REST API v3) for each event. Auth is HTTP
// Basic with an API token (the standard Jira Cloud automation credential). The
// issue type is "Bug" when the event carries errors, otherwise "Task". The issue
// is never deduped per-event — each alert is a distinct issue; dedup happens at
// the scanner level (one delivery per inbox alert).
type JiraNotifier struct {
	Base        string // e.g. https://acme.atlassian.net (no trailing slash)
	Email       string // Basic-auth user (the API-token email)
	Token       string // Basic-auth password (the API token)
	Project     string // project key, e.g. "PAD"
	Client      *http.Client
	Envelope    Envelope
	MaxAttempts int
	Backoff     time.Duration
}

func (n *JiraNotifier) Name() string { return "jira" }

func (n *JiraNotifier) Notify(ctx context.Context, ev Event) error {
	ev = ev.Stamped(n.Envelope.now())
	payload := map[string]any{
		"fields": map[string]any{
			"project":     map[string]string{"key": n.Project},
			"summary":     ev.Title,
			"description": jiraDescription(ev),
			"issuetype":   map[string]string{"name": jiraIssueType(ev)},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	d := newHTTPDelivery(n.Client, strings.TrimRight(n.Base, "/")+"/rest/api/3/issue", body, ev, n.Envelope.deliveryID(), "", n.Envelope, n.MaxAttempts, n.Backoff)
	d.header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(n.Email+":"+n.Token)))
	return d.post(ctx)
}

// jiraIssueType picks "Bug" for error-bearing events (errors / regressions) and
// "Task" otherwise, matching how a team typically triages quality signals.
func jiraIssueType(ev Event) string {
	if eventHasErrors(ev) {
		return "Bug"
	}
	return "Task"
}

// adfNode is an Atlassian Document Format node.
type adfNode struct {
	Type    string    `json:"type"`
	Version int       `json:"version,omitempty"`
	Text    string    `json:"text,omitempty"`
	Content []adfNode `json:"content,omitempty"`
}

// jiraDescription is the issue's description in Atlassian Document Format, which REST API v3 requires (a plain
// string is refused, B5): one paragraph per line of the alert email's plain body (or the title and message, should
// the template fail). ADF has no empty text node, so blank lines are left out. Not verified against a live Jira
// (ADR 0021).
func jiraDescription(ev Event) adfNode {
	doc := adfNode{Type: "doc", Version: 1}
	text := ev.Title + "\n" + ev.Message
	if msg, err := renderAlertEmail(ev); err == nil {
		text = msg.Text
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		doc.Content = append(doc.Content, adfNode{Type: "paragraph", Content: []adfNode{{Type: "text", Text: line}}})
	}
	return doc
}

// jiraProjectKey is what Jira accepts as a project key: a letter, then letters, digits and underscores.
var jiraProjectKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,254}$`)

// checkJiraProject is the project field's Check.
func checkJiraProject(_ context.Context, v string, _ egress.Policy, _ EgressSpec) error {
	if !jiraProjectKey.MatchString(v) {
		return errors.New("a Jira project key is a letter followed by letters, digits or underscores (PAD, not the project's name)")
	}
	return nil
}

// checkJiraUser is the email field's Check: the Basic-auth user, which RFC 7617 forbids a colon in.
func checkJiraUser(_ context.Context, v string, _ egress.Policy, _ EgressSpec) error {
	if len(v) > 254 || strings.ContainsRune(v, ':') || strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return errors.New("the Jira account's email has no colon or control character and is at most 254 characters")
	}
	return nil
}
