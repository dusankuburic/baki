// Package notify delivers governance alerts (drift, health regressions) to
// external channels: the outbound sink for the continuous-governance loop —
// the scanner hands an Event to a Dispatcher, which fans out best-effort to
// every configured channel (failures logged, never fatal; nil-safe no-op
// when nothing is configured).
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
	"sync"
	"time"

	"pad-core/logger"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/errreport"
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
	// EventAnalysisComplete fires when a flow analysis run finishes — the raw
	// signal (unlike drift/regression, which compare against history), for
	// audit/announcement channels.
	EventAnalysisComplete EventType = "analysis_complete"
	// EventTest is a synthetic probe for the channel "test" button (R2-3):
	// org admins verify a newly configured channel without a real signal.
	EventTest EventType = "test"
)

// Valid channel kinds for per-org channels (R2-3). Email/Jira stay
// deployment-global (they need SMTP creds / API tokens, not just a URL).
var ValidChannelKinds = []string{"webhook", "teams", "slack"}

// ValidChannelKind reports whether kind is an org-channel kind.
func ValidChannelKind(kind string) bool {
	switch kind {
	case "webhook", "teams", "slack":
		return true
	}
	return false
}

// ValidateChannel checks an org channel's kind and URL (the same validation
// BuildChannelNotifierWith applies) without building a client or transport —
// what a channel save needs. The host lookup honours ctx.
func ValidateChannel(ctx context.Context, kind, rawURL string, policy TargetPolicy) error {
	if !ValidChannelKind(kind) {
		return fmt.Errorf("unknown channel kind %q", kind)
	}
	_, err := validateAlertURLContext(ctx, rawURL, policy)
	return err
}

// NewChannelClient is the guarded HTTP client for org channels: one per policy
// per process, shared by every notifier built with it, so connections pool and
// the transport's idle timer reaps them — a transport cloned per channel save
// or test kept its idle TLS connection and two persistConn goroutines alive
// for 90 s after the handler returned.
func NewChannelClient(policy TargetPolicy) *http.Client {
	return &http.Client{
		Timeout:       defaultTimeout,
		Transport:     egress.GuardedTransport(policy),
		CheckRedirect: egress.NoRedirects,
	}
}

// BuildChannelNotifier constructs the single-channel notifier for an
// org-configured channel (R2-3) on a client of its own, with the same URL
// validation and defaults as the deployment-global builder. Request paths
// should prefer BuildChannelNotifierWith and a shared NewChannelClient.
func BuildChannelNotifier(kind, rawURL, secret string, policy TargetPolicy) (AlertNotifier, error) {
	return BuildChannelNotifierWith(context.Background(), NewChannelClient(policy), kind, rawURL, secret, policy)
}

// BuildChannelNotifierWith is BuildChannelNotifier on a caller-owned client
// (NewChannelClient), with the validation lookup bounded by ctx.
func BuildChannelNotifierWith(ctx context.Context, client *http.Client, kind, rawURL, secret string, policy TargetPolicy) (AlertNotifier, error) {
	u, err := validateAlertURLContext(ctx, rawURL, policy)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = NewChannelClient(policy)
	}
	switch kind {
	case "webhook":
		return &WebhookNotifier{URL: u, Client: client, Secret: secret, MaxAttempts: defaultRetries, Backoff: defaultBackoff}, nil
	case "teams":
		return &TeamsNotifier{URL: u, Client: client, MaxAttempts: defaultRetries, Backoff: defaultBackoff}, nil
	case "slack":
		return &SlackNotifier{URL: u, Client: client, Secret: secret, MaxAttempts: defaultRetries, Backoff: defaultBackoff}, nil
	default:
		return nil, fmt.Errorf("unknown channel kind %q", kind)
	}
}

// Deliver runs one notifier with timeout + panic recovery — exported so the
// org-channel router reuses the dispatcher's exact delivery semantics.
func Deliver(ctx context.Context, n AlertNotifier, ev Event) {
	deliver(ctx, n, ev, defaultTimeout)
}

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

// EmailSender is the minimal email capability an EmailNotifier needs (*mail.Service
// satisfies it). Defined here so notify stays decoupled from mail; a test can
// inject a fake sender.
type EmailSender interface {
	SendAlert(ctx context.Context, to, subject, plainBody, htmlBody string) error
	Enabled() bool
}

// Config selects which channels are active. Empty URLs disable their channel.
type Config struct {
	// Policy gates whether internal/loopback targets are reachable. The zero
	// value blocks them; local/desktop wiring opts in.
	Policy        TargetPolicy
	WebhookURL    string
	WebhookSecret string // HMAC-SHA256 key for X-Baki-Signature header; empty = no signature
	TeamsURL      string
	// SlackURL is a Slack incoming-webhook URL. The Slack payload format is also
	// compatible with Discord, Mattermost, and Teams (Slack-compatible connector).
	SlackURL    string
	SlackSecret string // optional HMAC key for Slack payloads (most Slack webhooks ignore it)
	// EmailSender + EmailTo enable an email alert channel. A nil sender disables
	// the channel even when EmailTo is set.
	EmailSender EmailSender
	EmailTo     string
	// Jira: when all four are set, an issue is created per event on Jira Cloud.
	// JiraURL is the base (e.g. https://acme.atlassian.net); JiraEmail + JiraToken
	// form the Basic-auth credential; JiraProject is the project key.
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

// TargetPolicy decides whether a notification target may point at the machine
// (or the private network) the server itself runs on. An alias of egress.Policy:
// the guard lives in package egress so the AI provider layer applies the very
// same rules to user-supplied endpoints, and the alias keeps this package's
// API unchanged.
//
// It exists because the "http://localhost for dev" escape hatch was once
// unconditional: in a multi-tenant deployment ANY authenticated user can
// create an org and become its admin, add a webhook channel, and hit the
// synchronous test endpoint — a loopback port scanner plus a POST of
// attacker-influenced JSON at internal listeners.
type TargetPolicy = egress.Policy

const (
	// BlockInternalTargets refuses loopback, private, link-local and unspecified
	// addresses, and refuses plaintext http:// entirely.
	//
	// The ZERO VALUE on purpose: a caller that forgets to pass a policy gets
	// the safe one; opting into the permissive behaviour has to be typed out
	// (which is also why the loopback-needing tests say so explicitly).
	BlockInternalTargets = egress.BlockInternal
	// AllowLoopbackTargets additionally permits http:// to loopback. DESKTOP AND
	// LOCAL MODE ONLY — there the "org" is the single user running the app, so a
	// webhook pointing at their own machine is exactly what they meant.
	AllowLoopbackTargets = egress.AllowLoopback
)

// validateAlertURL enforces HTTPS for outbound governance alert URLs: the
// payload carries flow names and finding counts (internal details an attacker
// couldn't otherwise enumerate); plaintext HTTP would expose them on any
// in-path host. http://localhost / 127.0.0.1 / [::1] are permitted for local
// development; everything else must be https://.
//
// SSRF guard: HTTPS alone still allows an org admin to point the channel at
// cloud-internal endpoints (https://169.254.169.254/... metadata, private
// RFC1918 services) and use the synchronous test endpoint as a reachability
// oracle — so DNS-resolved targets on internal ranges are rejected unless the
// host is an explicit loopback NAME (the documented dev escape hatch).
//
// This is only the validation-time half: the dial-time half, which closes the
// DNS-rebinding window, is egress.GuardedTransport on every client built
// here, and egress.NoRedirects keeps a policy-passing endpoint from steering
// the signed payload somewhere that never passed validation. Behind an
// HTTP(S)_PROXY the guard fails closed: a proxy on an internal address is
// refused, and behind a public proxy the target must resolve locally to
// public addresses — deployments whose egress proxy sits on a private address
// must put the alert hosts in NO_PROXY so they are dialled directly.
func validateAlertURL(raw string, policy TargetPolicy) (string, error) {
	return validateAlertURLContext(context.Background(), raw, policy)
}

// validateAlertURLContext is validateAlertURL with the host lookup bounded
// by ctx (a request's deadline and disconnect).
func validateAlertURLContext(ctx context.Context, raw string, policy TargetPolicy) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid alert URL %q: %w", raw, err)
	}
	host := u.Hostname()
	if u.Scheme == "https" {
		if err := rejectPrivateTargetContext(ctx, host, policy); err != nil {
			return "", err
		}
		return raw, nil
	}
	if u.Scheme == "http" && policy == AllowLoopbackTargets && egress.IsLoopbackHost(host) {
		return raw, nil
	}
	return "", fmt.Errorf("alert URL %q must use https; governance payloads carry internal flow details and must not be sent in plaintext (http:// to localhost is accepted only in local/desktop mode)", raw)
}

// rejectPrivateTargetContext resolves host (skipping resolution for IPs and
// the loopback dev names) and refuses RFC1918/loopback/link-local/unique-local
// addresses (B1.9). A wrapper over egress.RejectPrivateHostContext that keeps
// the "alert URL ..." wording the channel endpoints surface to org admins.
func rejectPrivateTargetContext(ctx context.Context, host string, policy TargetPolicy) error {
	if host == "" {
		return fmt.Errorf("alert URL has no host")
	}
	if err := egress.RejectPrivateHostContext(ctx, nil, host, policy); err != nil {
		return fmt.Errorf("alert URL %w", err)
	}
	return nil
}

// New builds a Dispatcher from cfg. Channels with empty URLs are omitted, so
// the returned dispatcher may have zero notifiers (Enabled() == false) — a
// valid, no-op configuration. Non-HTTPS URLs are rejected with an error so the
// misconfiguration surfaces instead of silently leaking alert payloads over
// plaintext.
func New(cfg Config) (*Dispatcher, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	client := &http.Client{
		Timeout:       timeout,
		Transport:     egress.GuardedTransport(cfg.Policy),
		CheckRedirect: egress.NoRedirects,
	}

	var notifiers []AlertNotifier
	if cfg.WebhookURL != "" {
		u, err := validateAlertURL(cfg.WebhookURL, cfg.Policy)
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
		u, err := validateAlertURL(cfg.TeamsURL, cfg.Policy)
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
		u, err := validateAlertURL(cfg.SlackURL, cfg.Policy)
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
		base, err := validateAlertURL(cfg.JiraURL, cfg.Policy)
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

// maxDispatchConcurrency bounds how many deployment-global channels deliver
// at once: sequential delivery meant one dead webhook burned its whole retry
// budget (~32 s) into EVERY sweep while the healthy channels waited. The cap
// makes a dispatch cost its slowest channel, not the sum of all — the same
// discipline the scanner's org-channel path applies with its own semaphore.
const maxDispatchConcurrency = 4

// Dispatch delivers ev to every channel, up to maxDispatchConcurrency at a
// time, and returns once all have finished. Per-channel failures and panics
// are logged, not propagated (isolation lives in deliver), so one bad channel
// never blocks the others or the caller. Nil-safe; a no-op when unconfigured.
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) {
	if !d.Enabled() {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	sem := make(chan struct{}, min(maxDispatchConcurrency, len(d.notifiers)))
	var wg sync.WaitGroup
	for _, n := range d.notifiers {
		// Acquire BEFORE spawning, as the org-channel path does: the semaphore,
		// not a goroutine per channel, is what bounds concurrency.
		sem <- struct{}{}
		wg.Add(1)
		go func(n AlertNotifier) {
			defer wg.Done()
			defer func() { <-sem }()
			deliver(ctx, n, ev, d.timeout)
		}(n)
	}
	wg.Wait()
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
		errreport.CaptureError(ctx, "notify.deliver", err, errreport.Attrs{"channel": n.Name(), "flowId": ev.FlowID, "event": string(ev.Type)})
	}
}

// postJSON is the shared HTTP delivery helper: POST a JSON body, treat any
// non-2xx response as an error, retry transient failures (network error, 429,
// 5xx) with exponential backoff. A non-empty secret sets X-Baki-Signature
// (HMAC-SHA256 hex of the body) so receivers can verify authenticity.
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

// TeamsNotifier POSTs a legacy MessageCard (the format Teams incoming webhooks
// render natively) to a Microsoft Teams incoming-webhook URL.
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

// --- Slack channel ---

// SlackNotifier POSTs a Slack incoming-webhook attachment — a payload format
// also rendered by Discord, Mattermost, and Teams (Slack-compatible connector),
// so this one channel covers several chat platforms.
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
