package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"pad-analyzer/internal/egress"
)

// The channel-kind registry (ADR 0021). Each kind is one file (kinds_<id>.go) whose init registers its KindSpec: its
// fields, where it can be configured, its egress and how it renders an Event. Validation, redaction, the stored
// field mapping and the builders all read the spec, so a new kind is one file: an organisation kind's fields beyond
// url and secret are stored in org_channels' config (public) and secrets (sealed) columns.

// Scope says where a kind can be configured.
type Scope uint8

const (
	// ScopeOrg: an organisation admin configures it on the settings page (org_channels: url, secret, config and
	// secrets).
	ScopeOrg Scope = 1 << iota
	// ScopeDeployment: the operator configures it in the environment (config.DeploymentChannels).
	ScopeDeployment
)

// Values are a channel's field values by field name.
type Values map[string]string

// FieldSpec is one field of a kind.
type FieldSpec struct {
	// Name is the wire and storage name: "url" and "secret" are the org_channels columns of that name; an organisation
	// kind's other fields are keys of its config (public) or secrets (sealed) column.
	Name string
	// Secret: sealed at rest, never returned, kept when an edit omits it, removed only by an explicit clear.
	Secret bool
	// Required: a channel without it does not validate.
	Required bool
	// Scope limits the field to these of the kind's scopes; 0 is every scope of the kind (the email kind's "to" is
	// the deployment's, its "recipients" an organisation's).
	Scope Scope
	// Members: the value is the organisation's member user ids, comma-separated. The channel service accepts only
	// members with a verified address at save and hands Build the addresses of those still members at delivery
	// (Deps.MemberAddresses): an organisation admin can never make the server mail an address of their choosing.
	Members bool
	// Check validates a value under the deployment's egress policy; nil accepts any value.
	Check func(ctx context.Context, v string, p egress.Policy, e EgressSpec) error
	// CheckScope limits Check to these scopes; 0 is every scope. A check added to a field the deployment already
	// configured runs for organisations only: refusing a value the previous release accepted would fail the boot.
	CheckScope Scope
}

// EgressSpec is where a kind may send. The zero value is "any public host, through the strict guard".
type EgressSpec struct {
	// Hosts restricts targets to these host names ("example.com", or "*.example.com" for its subdomains); nil
	// allows any public host.
	Hosts []string
	// Allowed is the deployment's host allow-list for organisation channels (PAD_NOTIFY_ALLOWED_HOSTS, WithAllowed),
	// in Hosts' syntax: a target must be on it as well as on Hosts. Never set by a kind; nil allows what Hosts allows.
	Allowed []string
	// TrustProxy trusts the operator's HTTP(S)_PROXY for the hop (egress.GuardAllowListedTransport). Only with
	// Hosts: a tenant-chosen host behind a trusted proxy is an SSRF hole.
	TrustProxy bool
	// Timeout is the per-attempt client timeout; 0 is the default 10 s.
	Timeout time.Duration
}

// WithAllowed returns e restricted to the deployment's host allow-list too (nil: unrestricted).
func (e EgressSpec) WithAllowed(hosts []string) EgressSpec {
	e.Allowed = hosts
	return e
}

// Deps is what a kind's Build gets besides the channel's values.
type Deps struct {
	// Client is the guarded client of the kind's egress class (Clients.For).
	Client *http.Client
	Policy egress.Policy
	// AllowedHosts is the deployment's host allow-list of an organisation channel (PAD_NOTIFY_ALLOWED_HOSTS): Build
	// validates with it, and a client Build makes itself is restricted to it. nil for the deployment's own channels.
	AllowedHosts []string
	// Mailer sends the email kind's messages.
	Mailer EmailSender
	// MemberAddresses are the verified addresses of an organisation channel's Members field (the email kind's
	// recipients still members), resolved by the channel service.
	MemberAddresses []string
	// OrgID is the organisation an org channel belongs to (the webhook body's "orgId"); "" for deployment channels.
	OrgID       string
	Envelope    Envelope
	MaxAttempts int
	Backoff     time.Duration
}

// KindSpec declares a channel kind.
type KindSpec struct {
	// ID is the wire and storage value (org_channels.kind): frozen once shipped.
	ID     string
	Scope  Scope
	Fields []FieldSpec
	Egress EgressSpec
	// Signed kinds carry the Baki signature headers when the channel has a secret.
	Signed bool
	// Mail kinds send through the deployment's mail sender (Deps.Mailer) and are unavailable without an enabled one.
	Mail bool
	// Build makes the notifier of validated values.
	Build func(Deps, Values) (AlertNotifier, error)
	// Public is what the API shows of the values: the secret fields left out or redacted.
	Public func(Values) Values
}

// Field returns the kind's field name.
func (k KindSpec) Field(name string) (FieldSpec, bool) {
	for _, f := range k.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return FieldSpec{}, false
}

// FieldsIn returns the kind's fields in scope.
func (k KindSpec) FieldsIn(scope Scope) []FieldSpec {
	var out []FieldSpec
	for _, f := range k.Fields {
		if f.Scope == 0 || f.Scope&scope != 0 {
			out = append(out, f)
		}
	}
	return out
}

// FieldIn returns the kind's field name in scope.
func (k KindSpec) FieldIn(scope Scope, name string) (FieldSpec, bool) {
	f, ok := k.Field(name)
	return f, ok && (f.Scope == 0 || f.Scope&scope != 0)
}

// ErrKindUnavailable is a Build's answer for a kind the deployment cannot serve (the email kind without an enabled
// mail sender): the channel is left out without an error.
var ErrKindUnavailable = errors.New("channel kind unavailable in this deployment")

// FieldError is a validation failure of one field. Its text never carries a URL's path or query.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Err.Error() }

func (e *FieldError) Unwrap() error { return e.Err }

var (
	registryMu sync.RWMutex
	registry   = map[string]KindSpec{}
)

// Register adds a kind. It panics on a spec that is incomplete, a duplicate ID, or a proxy-trusting egress without
// hosts; kinds register from their file's init.
func Register(k KindSpec) {
	switch {
	case k.ID == "" || k.ID != strings.ToLower(strings.TrimSpace(k.ID)):
		panic(fmt.Sprintf("notify: kind id %q must be lower case without spaces", k.ID))
	case k.Scope == 0:
		panic(fmt.Sprintf("notify: kind %q has no scope", k.ID))
	case k.Build == nil || k.Public == nil:
		panic(fmt.Sprintf("notify: kind %q needs Build and Public", k.ID))
	case k.Egress.TrustProxy && len(k.Egress.Hosts) == 0:
		panic(fmt.Sprintf("notify: kind %q trusts the proxy without a host allow-list", k.ID))
	}
	seen := map[string]bool{}
	for _, f := range k.Fields {
		switch {
		case f.Name == "" || seen[f.Name]:
			panic(fmt.Sprintf("notify: kind %q: empty or duplicate field %q", k.ID, f.Name))
		case f.Scope&^k.Scope != 0:
			panic(fmt.Sprintf("notify: kind %q: field %q has a scope the kind has not", k.ID, f.Name))
		case f.Members && f.Scope != ScopeOrg:
			panic(fmt.Sprintf("notify: kind %q: members field %q must be the organisation's only", k.ID, f.Name))
		}
		seen[f.Name] = true
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[k.ID]; dup {
		panic(fmt.Sprintf("notify: kind %q registered twice", k.ID))
	}
	registry[k.ID] = k
}

// Kind returns the registered kind id.
func Kind(id string) (KindSpec, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	k, ok := registry[id]
	return k, ok
}

// Kinds lists the registered kinds available in scope (any scope for 0), by ID.
func Kinds(scope Scope) []KindSpec {
	registryMu.RLock()
	defer registryMu.RUnlock()
	var out []KindSpec
	for _, k := range registry {
		if scope == 0 || k.Scope&scope != 0 {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b KindSpec) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// KindIDs lists the IDs of the kinds available in scope.
func KindIDs(scope Scope) []string {
	var ids []string
	for _, k := range Kinds(scope) {
		ids = append(ids, k.ID)
	}
	return ids
}

// Validate checks values against kind in scope: a known kind, no field it does not declare, every required field
// set, and each field's Check under policy. allowed, when given, is the deployment's host allow-list
// (PAD_NOTIFY_ALLOWED_HOSTS) an organisation channel's URL must be on as well as on the kind's Hosts. A field error
// is a *FieldError.
func Validate(ctx context.Context, kind string, scope Scope, v Values, p egress.Policy, allowed ...string) error {
	spec, ok := Kind(kind)
	if !ok || spec.Scope&scope == 0 {
		return &FieldError{Field: "kind", Err: fmt.Errorf("unknown channel kind %q (one of %s)", kind, strings.Join(KindIDs(scope), ", "))}
	}
	for name := range v {
		if _, ok := spec.FieldIn(scope, name); !ok && v[name] != "" {
			return &FieldError{Field: name, Err: fmt.Errorf("a %s channel has no %s", kind, name)}
		}
	}
	eg := spec.Egress.WithAllowed(allowed)
	for _, f := range spec.FieldsIn(scope) {
		val := v[f.Name]
		if val == "" {
			if f.Required {
				return &FieldError{Field: f.Name, Err: errors.New("is required")}
			}
			continue
		}
		if f.Check != nil && (f.CheckScope == 0 || f.CheckScope&scope != 0) {
			if err := f.Check(ctx, val, p, eg); err != nil {
				return &FieldError{Field: f.Name, Err: err}
			}
		}
	}
	return nil
}

// Build validates values and builds kind's notifier. deps.Client nil gets a client of its own.
func Build(ctx context.Context, kind string, scope Scope, v Values, deps Deps) (AlertNotifier, error) {
	if err := Validate(ctx, kind, scope, v, deps.Policy, deps.AllowedHosts...); err != nil {
		return nil, err
	}
	spec, _ := Kind(kind)
	if deps.Client == nil {
		deps.Client = NewClients(0).For(deps.Policy, spec.Egress.WithAllowed(deps.AllowedHosts))
	}
	if deps.MaxAttempts <= 0 {
		deps.MaxAttempts = defaultRetries
	}
	if deps.Backoff <= 0 {
		deps.Backoff = defaultBackoff
	}
	return spec.Build(deps, v)
}

// PublicValues is what the API may show of v for kind: the kind's Public, or nothing for an unknown kind.
func PublicValues(kind string, v Values) Values {
	spec, ok := Kind(kind)
	if !ok {
		return Values{}
	}
	return spec.Public(v)
}

// checkAlertURL is the url field's Check: https, or http to a loopback name under AllowLoopback, to a host on the
// kind's and the deployment's allow-lists (when set) that resolves to no internal address. The lists are checked
// first: a host off them costs no lookup, and the answer names the host as typed, never an address.
//
// The payload carries flow names and finding counts (internal details an attacker could not otherwise enumerate), so
// plaintext would expose them on any in-path host. HTTPS alone still lets an org admin point a channel at
// cloud-internal endpoints (metadata, RFC 1918 services) and use the synchronous test endpoint as a reachability
// oracle, so names that resolve to internal ranges are refused. This is the validation-time half: the dial-time half
// (DNS rebinding) is the guarded transport of every client, and egress.NoRedirects keeps a passing endpoint from
// steering the signed payload elsewhere. Behind an HTTP(S)_PROXY the guard fails closed (see egress.GuardTransport).
// The error text names the URL redacted (RedactURL): for Slack and Teams the URL is the credential.
func checkAlertURL(ctx context.Context, raw string, policy egress.Policy, spec EgressSpec) error {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Hostname() != "" {
		switch err := spec.refuse(u.Hostname()); {
		case errors.Is(err, errHostNotAllowed):
			return fmt.Errorf("alert URL %s is not on this channel kind's host list (%s)", RedactURL(raw), strings.Join(spec.Hosts, ", "))
		case errors.Is(err, errHostNotOnAllowList):
			// The list itself is not shown: an operator may list other customers' hosts.
			return fmt.Errorf("alert URL host %q is not on this deployment's list of allowed notification hosts (PAD_NOTIFY_ALLOWED_HOSTS); ask its operator", u.Hostname())
		}
	}
	_, err := validateAlertURLContext(ctx, raw, policy)
	return err
}

// validateAlertURL is validateAlertURLContext without a deadline (tests).
func validateAlertURL(raw string, policy TargetPolicy) (string, error) {
	return validateAlertURLContext(context.Background(), raw, policy)
}

// validateAlertURLContext enforces https (or http to a loopback name under AllowLoopback) and refuses hosts that
// resolve to internal addresses, with the lookup bounded by ctx.
func validateAlertURLContext(ctx context.Context, raw string, policy TargetPolicy) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid alert URL %s", RedactURL(raw))
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
	return "", fmt.Errorf("alert URL %s must use https; governance payloads carry internal flow details and must not be sent in plaintext (http:// to localhost is accepted only on a deployment without accounts)", RedactURL(raw))
}

// rejectPrivateTargetContext resolves host (skipping resolution for IPs and the loopback dev names) and refuses
// internal addresses (B1.9). The answer an admin gets names neither the resolved address nor the resolver's error, so
// a channel save is no oracle for the server's internal DNS (B13).
func rejectPrivateTargetContext(ctx context.Context, host string, policy TargetPolicy) error {
	if host == "" {
		return fmt.Errorf("alert URL has no host")
	}
	err := egress.RejectPrivateHostContext(ctx, nil, host, policy)
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "does not resolve"):
		return fmt.Errorf("alert URL host %q does not resolve", host)
	}
	return fmt.Errorf("alert URL host %q resolves to a private, loopback or link-local address; internal targets are not allowed", host)
}

// hostAllowed reports whether host is on hosts: equal to an entry, or a subdomain of a "*.suffix" entry. A host is
// compared in the form the transport dials: as it is when ASCII, else IDNA-mapped as net/http maps it, so a
// spelling that maps onto a listed host is that host and one that does not map is refused.
func hostAllowed(host string, hosts []string) bool {
	host = strings.TrimSuffix(host, ".")
	if strings.IndexFunc(host, func(r rune) bool { return r >= utf8.RuneSelf }) >= 0 {
		var err error
		if host, err = idna.Lookup.ToASCII(host); err != nil {
			return false
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == h {
			return true
		}
	}
	return false
}

// errHostNotAllowed is the refusal of a target off a kind's allow-list (EgressSpec.Hosts).
var errHostNotAllowed = errors.New("notify: target host is not on the channel kind's allow-list")

// errHostNotOnAllowList is the refusal of a target off the deployment's allow-list (EgressSpec.Allowed).
var errHostNotOnAllowList = errors.New("notify: target host is not on the deployment's allow-list (PAD_NOTIFY_ALLOWED_HOSTS)")

// refuse returns why host may not be a target of e (errHostNotAllowed, errHostNotOnAllowList), or nil.
func (e EgressSpec) refuse(host string) error {
	switch {
	case len(e.Hosts) > 0 && !hostAllowed(host, e.Hosts):
		return errHostNotAllowed
	case len(e.Allowed) > 0 && !hostAllowed(host, e.Allowed):
		return errHostNotOnAllowList
	}
	return nil
}

// allowListedTransport is the transport of a proxy-trusting egress class: egress.GuardAllowListedTransport (the
// operator's proxy trusted for the hop, the target judged). restrictHosts keeps the trusted hop to allow-listed
// targets.
func allowListedTransport(policy egress.Policy) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	egress.GuardAllowListedTransport(t, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, policy)
	return t
}

// restrictHosts makes t refuse a request whose host is off spec's lists (the kind's Hosts, the deployment's
// Allowed) in its Proxy hook, which net/http runs for every request, direct or proxied, before it dials: a trusted
// proxy hop only ever carries allow-listed targets, and a notifier built without validation (a stored channel whose
// host left the deployment's list) cannot reach its host either. A spec without lists leaves t as it is.
func restrictHosts(t *http.Transport, spec EgressSpec) {
	if len(spec.Hosts) == 0 && len(spec.Allowed) == 0 {
		return
	}
	selectProxy := t.Proxy
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		if err := spec.refuse(req.URL.Hostname()); err != nil {
			return nil, err
		}
		if selectProxy == nil {
			return nil, nil
		}
		return selectProxy(req)
	}
}

// urlFields are the url and optional secret every URL kind of today declares.
func urlField(secret bool) FieldSpec {
	return FieldSpec{Name: "url", Secret: secret, Required: true, Check: checkAlertURL}
}

// listedURLValues is the Public of the URL kinds: the URL as ListedURL shows it, no secret.
func listedURLValues(v Values) Values {
	return Values{"url": ListedURL(v["url"])}
}
