package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// --- Email channel ---

// EmailNotifier renders an event as a transactional email and sends it to a
// configured recipient (typically an ops alias or team mailing list). It depends
// only on the EmailSender interface, so the notify package stays decoupled from
// the mail package — main.go wires *mail.Service in.
type EmailNotifier struct {
	Sender EmailSender
	To     string
}

func (n *EmailNotifier) Name() string { return "email" }

func (n *EmailNotifier) Notify(ctx context.Context, ev Event) error {
	subject := fmt.Sprintf("[%s] %s", strings.ToUpper(string(ev.Type)), ev.Title)
	plain := emailPlainBody(ev)
	html := emailHTMLBody(ev)
	return n.Sender.SendAlert(ctx, n.To, subject, plain, html)
}

// emailPlainBody builds a readable plain-text summary of an event.
func emailPlainBody(ev Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Flow: %s\n", flowLabel(ev))
	fmt.Fprintf(&b, "Event: %s\n", string(ev.Type))
	if ev.Message != "" {
		fmt.Fprintf(&b, "\n%s\n", ev.Message)
	}
	switch ev.Type {
	case EventDrift:
		fmt.Fprintf(&b, "\nNew errors: %d\nNew warnings: %d\n", ev.NewErrors, ev.NewWarnings)
	case EventHealthRegression:
		fmt.Fprintf(&b, "\nHealth: %d → %d\n", ev.PrevHealth, ev.HealthScore)
	case EventAnalysisComplete:
		if ev.Analysis != nil {
			a := ev.Analysis
			fmt.Fprintf(&b, "\nErrors: %d\nWarnings: %d\nInfo: %d\n", a.Errors, a.Warnings, a.Info)
			if a.HealthScore > 0 {
				fmt.Fprintf(&b, "Health: %d/100\n", a.HealthScore)
			}
		}
	}
	fmt.Fprintf(&b, "\nTime: %s\n", ev.At.Format(time.RFC3339))
	return b.String()
}

// emailHTMLBody builds a minimal HTML rendering. Values come from the event
// (flow name is content-controlled), so they're HTML-escaped defensively.
func emailHTMLBody(ev Event) string {
	var b strings.Builder
	b.WriteString("<table cellpadding=\"0\" cellspacing=\"0\" style=\"font-family:sans-serif;font-size:14px\">")
	emailRow(&b, "Flow", flowLabel(ev))
	emailRow(&b, "Event", string(ev.Type))
	switch ev.Type {
	case EventDrift:
		emailRow(&b, "New errors", fmt.Sprintf("%d", ev.NewErrors))
		emailRow(&b, "New warnings", fmt.Sprintf("%d", ev.NewWarnings))
	case EventHealthRegression:
		emailRow(&b, "Health", fmt.Sprintf("%d → %d", ev.PrevHealth, ev.HealthScore))
	case EventAnalysisComplete:
		if ev.Analysis != nil {
			a := ev.Analysis
			emailRow(&b, "Errors", fmt.Sprintf("%d", a.Errors))
			emailRow(&b, "Warnings", fmt.Sprintf("%d", a.Warnings))
			emailRow(&b, "Info", fmt.Sprintf("%d", a.Info))
			if a.HealthScore > 0 {
				emailRow(&b, "Health", fmt.Sprintf("%d/100", a.HealthScore))
			}
		}
	}
	b.WriteString("</table>")
	if ev.Message != "" {
		fmt.Fprintf(&b, "<p>%s</p>", htmlEscape(ev.Message))
	}
	return b.String()
}

func emailRow(b *strings.Builder, k, v string) {
	fmt.Fprintf(b, "<tr><td style=\"color:#666;padding:2px 12px 2px 0\">%s</td><td>%s</td></tr>",
		htmlEscape(k), htmlEscape(v))
}

// htmlEscape does the minimal escaping needed for event fields in email HTML.
// (We don't import html/template for a handful of values.)
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	return s
}

// --- Jira channel ---

// JiraNotifier creates a Jira issue (REST API v3) for each event. Auth is HTTP
// Basic with an API token (the standard Jira Cloud automation credential). The
// issue type is "Bug" when the event carries errors, otherwise "Task". The issue
// is never deduped per-event — each alert is a distinct issue; dedup happens at
// the scanner level (shouldAlert) so a persistent regression isn't re-filed every
// tick.
type JiraNotifier struct {
	Base        string // e.g. https://acme.atlassian.net (no trailing slash)
	Email       string // Basic-auth user (the API-token email)
	Token       string // Basic-auth password (the API token)
	Project     string // project key, e.g. "PAD"
	Client      *http.Client
	MaxAttempts int
	Backoff     time.Duration
}

func (n *JiraNotifier) Name() string { return "jira" }

func (n *JiraNotifier) Notify(ctx context.Context, ev Event) error {
	url := strings.TrimRight(n.Base, "/") + "/rest/api/3/issue"
	payload := map[string]any{
		"fields": map[string]any{
			"project":     map[string]string{"key": n.Project},
			"summary":     ev.Title,
			"description": jiraDescription(ev),
			"issuetype":   map[string]string{"name": jiraIssueType(ev)},
		},
	}
	// Jira uses Basic auth with email:apiToken. The token is a credential, so the
	// request carries it in the standard Authorization header (not the URL).
	return postJSONWithAuth(ctx, n.Client, url, payload, n.Email, n.Token, n.MaxAttempts, n.Backoff)
}

// jiraIssueType picks "Bug" for error-bearing events (errors / regressions) and
// "Task" otherwise, matching how a team typically triages quality signals.
func jiraIssueType(ev Event) string {
	if eventHasErrors(ev) {
		return "Bug"
	}
	return "Task"
}

// jiraDescription builds an Atlassian Document-like plain description (we use a
// simple string rather than the full ADF to keep the payload portable across
// Jira versions that accept plain-text bodies).
func jiraDescription(ev Event) string {
	return emailPlainBody(ev) // same structure reads well as a Jira description
}

// postJSONWithAuth mirrors postJSON but adds a Basic-auth header instead of an
// HMAC signature (Jira's API-token auth is Basic, not HMAC).
func postJSONWithAuth(ctx context.Context, client *http.Client, url string, payload any, user, pass string, maxAttempts int, backoff time.Duration) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultRetries
	}
	if backoff <= 0 {
		backoff = defaultBackoff
	}
	cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff << (attempt - 1)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Basic "+cred)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("status %d (retryable)", resp.StatusCode)
			continue
		}
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}
