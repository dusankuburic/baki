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
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pad-core/logger"
)

const defaultTimeout = 10 * time.Second

// EventType enumerates the governance signals worth alerting on.
type EventType string

const (
	// EventDrift fires when new findings appear since a flow's accepted baseline.
	EventDrift EventType = "drift"
	// EventHealthRegression fires when a flow's health score drops vs. its prior run.
	EventHealthRegression EventType = "health_regression"
)

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
	At          time.Time `json:"at"`
}

// AlertNotifier delivers a single event to one channel.
type AlertNotifier interface {
	Notify(ctx context.Context, ev Event) error
	Name() string
}

// Config selects which channels are active. Empty URLs disable their channel.
type Config struct {
	WebhookURL string
	TeamsURL   string
	Timeout    time.Duration // per-delivery timeout; 0 ⇒ defaultTimeout
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
		notifiers = append(notifiers, &WebhookNotifier{URL: u, Client: client})
	}
	if cfg.TeamsURL != "" {
		u, err := validateAlertURL(cfg.TeamsURL)
		if err != nil {
			return nil, err
		}
		notifiers = append(notifiers, &TeamsNotifier{URL: u, Client: client})
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
// non-2xx response as an error.
func postJSON(ctx context.Context, client *http.Client, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// WebhookNotifier POSTs the raw Event JSON to a generic endpoint.
type WebhookNotifier struct {
	URL    string
	Client *http.Client
}

func (n *WebhookNotifier) Name() string { return "webhook" }

func (n *WebhookNotifier) Notify(ctx context.Context, ev Event) error {
	return postJSON(ctx, n.Client, n.URL, ev)
}

// TeamsNotifier POSTs a legacy MessageCard to a Microsoft Teams incoming-webhook
// URL. MessageCard is the format Teams incoming webhooks render natively.
type TeamsNotifier struct {
	URL    string
	Client *http.Client
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
	return postJSON(ctx, n.Client, n.URL, card)
}

func flowLabel(ev Event) string {
	if ev.FlowName != "" {
		return ev.FlowName
	}
	return ev.FlowID
}

// themeColor maps an event to a Teams card accent color.
func themeColor(ev Event) string {
	if ev.Type == EventDrift && ev.NewErrors > 0 {
		return "D13438" // red — new errors
	}
	return "FFB900" // amber — warnings / regressions
}
