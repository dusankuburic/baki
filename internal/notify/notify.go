// Package notify delivers governance alerts (drift, health regressions, new-rule notices) to external channels:
// the outbound sink for the continuous-governance loop. Each channel kind (webhook, Slack, Teams, Jira, email) is one
// registered KindSpec (kinds.go, one file per kind) that declares its fields, its egress and how it renders an Event.
// The scanner and the analysis path hand an Event to a Dispatcher (the deployment's channels) or to the org channels
// service.ChannelService builds; delivery is best effort (failures logged, never fatal).
package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	// EventNewRules: rules newer than the flows' baselines or health snapshots report findings, announced once per
	// scope and rule set (ADR 0016): in the inbox, and to the organisation's channels at the end of the sweep that
	// raised the notice (ADR 0021).
	EventNewRules EventType = "new_rules"
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

// Event is a single governance alert. Its JSON fields are the generic webhook's body (webhookBody adds the
// envelope's); richer channels map it to a card.
type Event struct {
	// ID identifies the event across channels, retries and replicas (EventID, NewEventID): receivers dedupe on it.
	// The webhook body carries it as "id".
	ID          string    `json:"-"`
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
	// RuleIDs and FlowCount describe an EventNewRules notice: the rules that report findings and in how many flows.
	RuleIDs   []string  `json:"ruleIds,omitempty"`
	FlowCount int       `json:"flowCount,omitempty"`
	At        time.Time `json:"at"`
}

// Stamped returns ev with an id and a time when it has none, so no channel receives a zero "at" (B3) or an event
// receivers cannot dedupe.
func (ev Event) Stamped(now time.Time) Event {
	if ev.At.IsZero() {
		ev.At = now.UTC()
	}
	if ev.ID == "" {
		ev.ID = NewEventID()
	}
	return ev
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

// ChannelConfig is one deployment channel: a kind and its field values (config.DeploymentChannel).
type ChannelConfig struct {
	Kind   string
	Values Values
}

// Config is the deployment's channels and how they deliver.
type Config struct {
	// Policy gates whether internal/loopback targets are reachable (egress.ForDeployment). The zero
	// value blocks them.
	Policy TargetPolicy
	// Channels are the deployment channels in delivery order. A kind that is not available (email without an
	// enabled sender) is left out; one that does not validate fails New.
	Channels []ChannelConfig
	// Mailer sends the email kind's messages; nil leaves the email channel out.
	Mailer      EmailSender
	Envelope    Envelope
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
// same rules to user-supplied endpoints.
//
// It exists because the "http://localhost for dev" escape hatch was once
// unconditional: in a multi-tenant deployment ANY authenticated user can
// create an org and become its admin, add a webhook channel, and hit the
// synchronous test endpoint — a loopback port scanner plus a POST of
// attacker-influenced JSON at internal listeners.
type TargetPolicy = egress.Policy

const (
	// BlockInternalTargets refuses loopback, private, link-local and unspecified
	// addresses, and refuses plaintext http:// entirely. The ZERO VALUE on purpose.
	BlockInternalTargets = egress.BlockInternal
	// AllowLoopbackTargets additionally permits http:// to loopback: a deployment without accounts, where the
	// single user's webhook on their own machine is exactly what they meant.
	AllowLoopbackTargets = egress.AllowLoopback
)

// New builds the deployment's Dispatcher. With no channel the dispatcher has zero notifiers (Enabled() == false), a
// valid no-op configuration. A channel that does not validate (a non-HTTPS URL, an internal target) fails New, so
// the misconfiguration surfaces at boot instead of silently leaking alert payloads.
func New(cfg Config) (*Dispatcher, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	clients := &Clients{timeout: timeout}
	var notifiers []AlertNotifier
	for _, ch := range cfg.Channels {
		spec, ok := Kind(ch.Kind)
		if !ok || spec.Scope&ScopeDeployment == 0 {
			return nil, fmt.Errorf("unknown deployment channel kind %q", ch.Kind)
		}
		n, err := Build(context.Background(), ch.Kind, ScopeDeployment, ch.Values, Deps{
			Client:      clients.For(cfg.Policy, spec.Egress),
			Policy:      cfg.Policy,
			Mailer:      cfg.Mailer,
			Envelope:    cfg.Envelope,
			MaxAttempts: cfg.MaxAttempts,
			Backoff:     cfg.Backoff,
		})
		if errors.Is(err, ErrKindUnavailable) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s channel: %w", ch.Kind, err)
		}
		notifiers = append(notifiers, n)
	}
	return &Dispatcher{notifiers: notifiers, timeout: timeout}, nil
}

// Enabled reports whether any channel is configured. Nil-safe.
func (d *Dispatcher) Enabled() bool {
	return d != nil && len(d.notifiers) > 0
}

// maxDispatchConcurrency bounds how many deployment-global channels deliver
// at once: sequential delivery meant one dead webhook burned its whole retry
// budget into EVERY sweep while the healthy channels waited. The cap
// makes a dispatch cost its slowest channel, not the sum of all.
const maxDispatchConcurrency = 4

// Dispatch delivers ev to every channel, up to maxDispatchConcurrency at a
// time, and returns once all have finished. Per-channel failures and panics
// are logged, not propagated (isolation lives in deliver), so one bad channel
// never blocks the others or the caller. Nil-safe; a no-op when unconfigured.
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) {
	if !d.Enabled() {
		return
	}
	ev = ev.Stamped(time.Now())
	sem := make(chan struct{}, min(maxDispatchConcurrency, len(d.notifiers)))
	var wg sync.WaitGroup
	for _, n := range d.notifiers {
		// Acquire BEFORE spawning: the semaphore, not a goroutine per channel, is what bounds concurrency.
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

// Deliver runs one notifier with the default timeout and panic recovery: the org channels' delivery, with the
// dispatcher's exact semantics.
func Deliver(ctx context.Context, n AlertNotifier, ev Event) {
	deliver(ctx, n, ev, defaultTimeout)
}

// deliver runs one notifier with a timeout and panic recovery. Its log line and error report never carry a
// channel's URL path or query (B4): every error leaving a notifier is redacted (RedactError).
func deliver(ctx context.Context, n AlertNotifier, ev Event, timeout time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("notify: channel panicked", "channel", n.Name(), "recover", r)
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := n.Notify(cctx, ev); err != nil {
		err = RedactError(err)
		logger.Warn("notify: delivery failed", "channel", n.Name(), "flowId", ev.FlowID, "event", string(ev.Type), "err", err)
		errreport.CaptureError(ctx, "notify.deliver", err, errreport.Attrs{"channel": n.Name(), "flowId": ev.FlowID, "event": string(ev.Type)})
	}
}

// Clients hands out the guarded HTTP client of an egress class (the deployment policy and a kind's EgressSpec), one
// per class per process: every notifier of a class shares one connection pool, and a client per channel save or test
// kept a cloned transport's idle connections alive after the handler returned (D-25).
type Clients struct {
	timeout time.Duration
	mu      sync.Mutex
	byClass map[clientClass]*http.Client
}

type clientClass struct {
	policy     egress.Policy
	trustProxy bool
	hosts      string // the kind's allow-list
	allowed    string // the deployment's allow-list (organisation channels)
	timeout    time.Duration
}

// For returns the client for policy and spec, building it once. A spec that trusts the proxy gets the allow-listed
// transport, every other spec the strict guard (egress.GuardedTransport); either refuses a host off the spec's
// lists (restrictHosts).
func (c *Clients) For(policy egress.Policy, spec EgressSpec) *http.Client {
	class := clientClass{policy: policy, trustProxy: spec.TrustProxy, hosts: strings.Join(spec.Hosts, ","),
		allowed: strings.Join(spec.Allowed, ","), timeout: spec.Timeout}
	if class.timeout <= 0 {
		class.timeout = c.timeout
	}
	if class.timeout <= 0 {
		class.timeout = defaultTimeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.byClass[class]; ok {
		return cl
	}
	transport := egress.GuardedTransport(policy)
	if spec.TrustProxy {
		transport = allowListedTransport(policy)
	}
	restrictHosts(transport, spec)
	cl := &http.Client{Timeout: class.timeout, Transport: transport, CheckRedirect: egress.NoRedirects}
	if c.byClass == nil {
		c.byClass = make(map[clientClass]*http.Client)
	}
	c.byClass[class] = cl
	return cl
}

// NewClients returns an empty client cache whose clients time out after timeout (0: the default 10 s).
func NewClients(timeout time.Duration) *Clients {
	return &Clients{timeout: timeout}
}
