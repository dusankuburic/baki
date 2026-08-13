// Package notify delivers governance alerts (drift, health regressions) to
// external channels — a generic webhook and Microsoft Teams to start. It is the
// outbound sink for the continuous-governance loop: the periodic scanner detects
// a regression and hands an Event to a Dispatcher, which fans out to every
// configured channel.
//
// Delivery is best-effort: a failing or slow channel is logged and skipped, never
// fatal, and a panicking notifier can't take down the caller. The Dispatcher is
// safe to call on a nil receiver and degrades to a no-op when nothing is
// configured, so callers don't need to special-case "notifications disabled".
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pad-core/logger"
)

const (
	defaultTimeout = 10 * time.Second
	defaultRetries = 3
	defaultBackoff = 500 * time.Millisecond
)

// EventType enumerates the governance signals worth alerting on.
type EventType string

const (
	// EventDrift fires when new findings appear since a flow's accepted baseline.
	EventDrift EventType = "drift"
	// EventHealthRegression fires when a flow's health score drops vs. its prior run.
	EventHealthRegression EventType = "health_regression"
	// EventAnalysisComplete fires when a flow analysis run finishes. Unlike
	// drift/regression (which compare against history), this is the raw
	// "a flow was just analyzed" signal — useful for audit/announcement channels.
	EventAnalysisComplete EventType = "analysis_complete"
)

// AnalysisSummary carries the headline counts from an analysis report so a
// channel can render a rich card without re-serializing the whole report.
type AnalysisSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
	// HealthScore is the flow's 0-100 health score; omitted when metrics absent.
	HealthScore int `json:"healthScore,omitempty"`
}

// Event is a single governance alert. It is serialized as-is to the generic
// webhook and mapped to a card for richer channels.
type Event struct {
	Type        EventType `json:"type"`
	FlowID      string    `json:"flowId"`
	FlowName    string    `json:"flowName,omitempty"`
	Title       string    `json:"title"`
	Message     string    `json:"message"`
	NewErrors   int       `json:"newErrors,omitempty"`
	NewWarnings int       `json:"newWarnings,omitempty"`
	HealthScore int       `json:"healthScore,omitempty"`
	PrevHealth  int       `json:"prevHealth,omitempty"`
	// Analysis carries the full count breakdown for EventAnalysisComplete.
	// Nil for drift/regression events (those use NewErrors/NewWarnings).
	Analysis *AnalysisSummary `json:"analysis,omitempty"`
	At       time.Time        `json:"at"`
}

// AlertNotifier delivers a single event to one channel.
type AlertNotifier interface {
	Notify(ctx context.Context, ev Event) error
	Name() string
}

// EmailSender is the minimal email capability an EmailNotifier needs. The app's
// *mail.Service satisfies it (see SendAlert). Defined here so the notify package
// stays decoupled from the mail package — a test can inject a fake sender.
type EmailSender interface {
	SendAlert(ctx context.Context, to, subject, plainBody, htmlBody string) error
	Enabled() bool
}

// Config selects which channels are active. Empty URLs disable their channel.
type Config struct {
	WebhookURL    string
	WebhookSecret string // HMAC-SHA256 key for X-Baki-Signature header; empty = no signature
	TeamsURL      string
	// SlackURL is a Slack incoming-webhook URL. The Slack payload format is also
	// compatible with Discord, Mattermost, and Teams (via the Slack-compatible connector).
	SlackURL    string
	SlackSecret string // optional HMAC key for Slack payloads (most Slack webhooks ignore it)
	// EmailSender + EmailTo enable an email alert channel. A nil sender disables
	// the channel even when EmailTo is set.
	EmailSender EmailSender
	EmailTo     string
	// Jira: when all four are set, an issue is created per event on Jira Cloud.
	// JiraURL is the base (e.g. https://acme.atlassian.net); JiraEmail + JiraToken
	// form the Basic-auth credential (API token); JiraProject is the project key.
	JiraURL     string
	JiraEmail   string
	JiraToken   string
	JiraProject string
	Timeout     time.Duration // per-attempt timeout; 0 ⇒ defaultTimeout
	MaxAttempts int           // retry count; 0 ⇒ defaultRetries (3)
	Backoff     time.Duration // base exponential backoff; 0 ⇒ defaultBackoff
}

// Dispatcher fans an event out to every configured channel.
type Dispatcher struct {
	notifiers []AlertNotifier
	timeout   time.Duration
}

// validateAlertURL enforces HTTPS for outbound governance alert URLs. The
// payload carries flow names and finding counts (internal details an attacker
// couldn't otherwise enumerate); sending it over plaintext HTTP would expose
// them on any in-path host. http://localhost / 127.0.0.1 / [::1] are permitted
// for local development; everything else must be https://. Returns the URL
// unchanged when acceptable, or an explanatory error.
func validateAlertURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid alert URL %q: %w", raw, err)
	}
	host := u.Hostname()
	if u.Scheme == "https" {
		return raw, nil
	}
	if u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return raw, nil
	}
	return "", fmt.Errorf("alert URL %q must use https (or http://localhost for dev); governance payloads carry internal flow details and must not be sent in plaintext", raw)
}

// New builds a Dispatcher from cfg. Channels with empty URLs are omitted, so the
// returned dispatcher may have zero notifiers (Enabled() == false) — that's a
// valid, no-op configuration. Non-HTTPS URLs are rejected with an error so the
// caller can surface the misconfiguration rather than silently leaking alert
// payloads over plaintext.
func New(cfg Config) (*Dispatcher, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	client := &http.Client{Timeout: timeout}

	var notifiers []AlertNotifier
	if cfg.WebhookURL != "" {
		u, err := validateAlertURL(cfg.WebhookURL)
		if err != nil {
			return nil, err
		}
		notifiers = append(notifiers, &WebhookNotifier{
			URL:         u,
			Client:      client,
			Secret:      cfg.WebhookSecret,
			MaxAttempts: cfg.MaxAttempts,
			Backoff:     cfg.Backoff,
		})
	}
	if cfg.TeamsURL != "" {
		u, err := validateAlertURL(cfg.TeamsURL)
		if err != nil {
			return nil, err
		}
		notifiers = append(notifiers, &TeamsNotifier{
			URL:         u,
			Client:      client,
			MaxAttempts: cfg.MaxAttempts,
			Backoff:     cfg.Backoff,
		})
	}
	if cfg.SlackURL != "" {
		u, err := validateAlertURL(cfg.SlackURL)
		if err != nil {
			return nil, err
		}
		notifiers = append(notifiers, &SlackNotifier{
			URL:         u,
			Client:      client,
			Secret:      cfg.SlackSecret,
			MaxAttempts: cfg.MaxAttempts,
			Backoff:     cfg.Backoff,
		})
	}
	if cfg.EmailSender != nil && cfg.EmailTo != "" && cfg.EmailSender.Enabled() {
		notifiers = append(notifiers, &EmailNotifier{
			Sender: cfg.EmailSender,
			To:     cfg.EmailTo,
		})
	}
	if cfg.JiraURL != "" && cfg.JiraEmail != "" && cfg.JiraToken != "" && cfg.JiraProject != "" {
		base, err := validateAlertURL(cfg.JiraURL)
		if err != nil {
			return nil, err
		}
		notifiers = append(notifiers, &JiraNotifier{
			Base:        base,
			Email:       cfg.JiraEmail,
			Token:       cfg.JiraToken,
			Project:     cfg.JiraProject,
			Client:      client,
			MaxAttempts: cfg.MaxAttempts,
			Backoff:     cfg.Backoff,
		})
	}
	return &Dispatcher{notifiers: notifiers, timeout: timeout}, nil
}

// Enabled reports whether any channel is configured. Nil-safe.
func (d *Dispatcher) Enabled() bool {
	return d != nil && len(d.notifiers) > 0
}

// Dispatch delivers ev to every channel synchronously. Per-channel failures and
// panics are logged, not propagated, so one bad channel never blocks the others
// or the caller. Run it from a background goroutine if you don't want to wait.
// Nil-safe and a no-op when no channels are configured.
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) {
	if !d.Enabled() {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	for _, n := range d.notifiers {
		deliver(ctx, n, ev, d.timeout)
	}
}

// deliver runs one notifier with a timeout and panic recovery.
func deliver(ctx context.Context, n AlertNotifier, ev Event, timeout time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("notify: channel panicked", "channel", n.Name(), "recover", r)
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := n.Notify(cctx, ev); err != nil {
		logger.Warn("notify: delivery failed", "channel", n.Name(), "flowId", ev.FlowID, "err", err)
	}
}

// postJSON is the shared HTTP delivery helper: POST a JSON body and treat any
// non-2xx response as an error. Retries on transient failures (network error,
// 429, 5xx) with exponential backoff. When secret is non-empty, an
// X-Baki-Signature header (HMAC-SHA256 hex of the body) is set so receivers
// can verify authenticity.
func postJSON(ctx context.Context, client *http.Client, url string, payload any, secret string, maxAttempts int, backoff time.Duration) error {
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

		// HMAC signature for payload authenticity.
		if secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			req.Header.Set("X-Baki-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			logger.Warn("notify: HTTP error, will retry", "attempt", attempt+1, "err", err)
			continue
		}
		// 2xx → success
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			return nil
		}
		// 429 (rate-limited) and 5xx (server error) are retryable
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("status %d (retryable)", resp.StatusCode)
			logger.Warn("notify: retryable status, will retry", "attempt", attempt+1, "status", resp.StatusCode)
			continue
		}
		// 4xx (non-429) → permanent failure, don't retry
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// WebhookNotifier POSTs the raw Event JSON to a generic endpoint.
type WebhookNotifier struct {
	URL         string
	Client      *http.Client
	Secret      string
	MaxAttempts int
	Backoff     time.Duration
}

func (n *WebhookNotifier) Name() string { return "webhook" }

func (n *WebhookNotifier) Notify(ctx context.Context, ev Event) error {
	return postJSON(ctx, n.Client, n.URL, ev, n.Secret, n.MaxAttempts, n.Backoff)
}

// TeamsNotifier POSTs a legacy MessageCard to a Microsoft Teams incoming-webhook
// URL. MessageCard is the format Teams incoming webhooks render natively.
type TeamsNotifier struct {
	URL         string
	Client      *http.Client
	MaxAttempts int
	Backoff     time.Duration
}

func (n *TeamsNotifier) Name() string { return "teams" }

func (n *TeamsNotifier) Notify(ctx context.Context, ev Event) error {
	facts := []map[string]string{{"name": "Flow", "value": flowLabel(ev)}}
	if ev.Type == EventDrift {
		facts = append(facts, map[string]string{"name": "New findings", "value": fmt.Sprintf("%d error(s), %d warning(s)", ev.NewErrors, ev.NewWarnings)})
	}
	if ev.Type == EventHealthRegression {
		facts = append(facts, map[string]string{"name": "Health", "value": fmt.Sprintf("%d → %d", ev.PrevHealth, ev.HealthScore)})
	}
	if ev.Type == EventAnalysisComplete && ev.Analysis != nil {
		a := ev.Analysis
		facts = append(facts,
			map[string]string{"name": "Errors", "value": fmt.Sprintf("%d", a.Errors)},
			map[string]string{"name": "Warnings", "value": fmt.Sprintf("%d", a.Warnings)},
			map[string]string{"name": "Info", "value": fmt.Sprintf("%d", a.Info)},
		)
		if a.HealthScore > 0 {
			facts = append(facts, map[string]string{"name": "Health", "value": fmt.Sprintf("%d/100", a.HealthScore)})
		}
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
		}},
	}
	return postJSON(ctx, n.Client, n.URL, card, "", n.MaxAttempts, n.Backoff)
}

func flowLabel(ev Event) string {
	if ev.FlowName != "" {
		return ev.FlowName
	}
	return ev.FlowID
}

// themeColor maps an event to a Teams card accent color.
func themeColor(ev Event) string {
	if eventHasErrors(ev) {
		return "D13438" // red — errors present
	}
	return "FFB900" // amber — warnings / regressions
}

// eventHasErrors reports whether an event represents an error-level signal —
// either new errors in a drift, or errors in an analysis summary. Used to pick
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

// --- Slack channel ---

// SlackNotifier POSTs a Slack incoming-webhook attachment. The payload format
// is also rendered correctly by Discord, Mattermost, and Microsoft Teams (via
// the Slack-compatible webhook connector), so this one channel covers several
// chat platforms.
type SlackNotifier struct {
	URL         string
	Client      *http.Client
	Secret      string
	MaxAttempts int
	Backoff     time.Duration
}

func (n *SlackNotifier) Name() string { return "slack" }

func (n *SlackNotifier) Notify(ctx context.Context, ev Event) error {
	payload := map[string]any{
		"text": ev.Title,
		"attachments": []map[string]any{{
			"color":  slackColor(ev),
			"text":   ev.Message,
			"fields": slackFields(ev),
			"footer": "Baki PAD Flow Analyzer",
			"ts":     ev.At.Unix(),
		}},
	}
	return postJSON(ctx, n.Client, n.URL, payload, n.Secret, n.MaxAttempts, n.Backoff)
}

// slackColor maps an event to a Slack attachment side-stripe color. Slack
// accepts hex ("#D13438") or the named aliases "danger"/"warning"/"good".
func slackColor(ev Event) string {
	if eventHasErrors(ev) {
		return "danger"
	}
	if ev.NewWarnings > 0 || (ev.Analysis != nil && ev.Analysis.Warnings > 0) {
		return "warning"
	}
	return "good"
}

// slackFields builds the short fields row under a Slack attachment.
func slackFields(ev Event) []map[string]any {
	fields := []map[string]any{{"title": "Flow", "value": flowLabel(ev), "short": true}}
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
