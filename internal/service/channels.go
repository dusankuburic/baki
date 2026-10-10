package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/platform/apperr"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ChannelService owns an organisation's notification channels (ADR 0021): the kinds they may be (the notify
// registry's organisation kinds), their validation under the deployment's egress policy and host allow-list
// (PAD_NOTIFY_ALLOWED_HOSTS, at save and at every delivery), what the API shows of them
// (the URL redacted, never a secret), an edit's keep-on-omit semantics, the email kind's recipients (members only),
// the event subscriptions, the test delivery, and the runner every org-channel delivery runs on. The store seals the
// URL and the secrets at rest.
type ChannelService struct {
	store       storageif.OrgChannelStore
	policy      egress.Policy
	allowed     []string
	showAllowed bool
	mailer      notify.EmailSender
	members     ChannelMembers
	clients     *notify.Clients
	runner      *lifecycle.Bounded
	env         notify.Envelope
}

// ChannelMembers is the roster an email channel's recipients are checked against at save and resolved from at
// delivery.
type ChannelMembers interface {
	LoadOrg(ctx context.Context, id string) (*storageif.Organisation, error)
	LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*storageif.User, error)
}

// ChannelOptions configures a ChannelService.
type ChannelOptions struct {
	// Policy is the deployment's egress policy (egress.ForDeployment).
	Policy egress.Policy
	// AllowedHosts is the deployment's host allow-list (config NotifyAllowedHosts); nil allows any public host.
	AllowedHosts []string
	// AllowedHostsVisible shows AllowedHosts to organisation admins (PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE). Off, they
	// learn only that a list applies: an operator may list other customers' hosts.
	AllowedHostsVisible bool
	// Mailer sends the email kind's messages; without an enabled one no email channel can be saved or delivered.
	Mailer notify.EmailSender
	// Members is the roster email recipients are checked against and resolved from; nil allows no email channel.
	Members ChannelMembers
	// Runner runs the deliveries; nil is a "notify" runner of ChannelDeliveryLimit, drained at shutdown.
	Runner *lifecycle.Bounded
	// Envelope stamps the deliveries; the zero value is the real clock, random delivery ids and baki/<Version>.
	Envelope notify.Envelope
}

// ChannelDeliveryLimit caps the org-channel deliveries in flight, the scanner's and the channel tests' together:
// high enough that a normal sweep never waits, low enough not to exhaust file descriptors.
const ChannelDeliveryLimit = 32

// channelTestTimeout bounds a channel test's delivery, retries included.
const channelTestTimeout = 15 * time.Second

// NewChannelService returns the service over store. Without options the policy is the zero egress.BlockInternal, the
// safe one; at most one ChannelOptions is read (variadic so callers that need no options pass none).
func NewChannelService(store storageif.OrgChannelStore, options ...ChannelOptions) *ChannelService {
	var opts ChannelOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.Runner == nil {
		opts.Runner = lifecycle.NewBounded("notify", ChannelDeliveryLimit)
	}
	if opts.Envelope.UserAgent == "" {
		opts.Envelope.UserAgent = notify.UserAgent(Version)
	}
	return &ChannelService{store: store, policy: opts.Policy, allowed: opts.AllowedHosts, showAllowed: opts.AllowedHostsVisible,
		mailer: opts.Mailer, members: opts.Members, clients: notify.NewClients(0), runner: opts.Runner, env: opts.Envelope}
}

// ChannelView is what the API shows of a channel: its public values (the URL as notify.ListedURL shows it, host and
// "/…", to admins too: for Slack and Teams the URL is the credential), and whether it has a signing secret, never
// the secret.
type ChannelView struct {
	ID        string
	Name      string
	Kind      string
	URL       string
	Enabled   bool
	CreatedAt time.Time
	HasSecret bool
	// Values are the kind's other public fields (jira's email and project, an email channel's recipients).
	Values notify.Values
	// Secrets says, per secret field of the kind beyond secret (jira's token), whether it is set; never the value.
	Secrets map[string]bool
	// Events are the event types the channel receives (the default set when it never chose).
	Events []notify.EventType
	// Unreadable: the stored URL or secrets open with no configured key (the encryption key changed). Listed so an
	// admin can delete the channel or enter its URL again; never delivered to.
	Unreadable bool
}

func viewOf(ch *storageif.OrgChannel) ChannelView {
	values := valuesOf(ch)
	public := notify.PublicValues(ch.Kind, values)
	url := public["url"]
	spec, known := notify.Kind(ch.Kind)
	if !known {
		// A kind a newer release wrote: listed, testable and removable, its URL redacted all the same.
		url = notify.ListedURL(ch.URL)
	}
	v := ChannelView{ID: ch.ID, Name: ch.Name, Kind: ch.Kind, URL: url, Enabled: ch.Enabled, CreatedAt: ch.CreatedAt,
		// hasSecret is the kind's declared secret: a Teams row's legacy secret column signs nothing.
		HasSecret: values["secret"] != "", Events: notify.SubscribedEvents(ch.Events), Unreadable: ch.Unreadable}
	if ch.Unreadable {
		v.URL = ""
	}
	for name, val := range public {
		if name != "url" && val != "" {
			if v.Values == nil {
				v.Values = notify.Values{}
			}
			v.Values[name] = val
		}
	}
	for _, f := range spec.FieldsIn(notify.ScopeOrg) {
		if f.Secret && f.Name != "url" && f.Name != "secret" {
			if v.Secrets == nil {
				v.Secrets = map[string]bool{}
			}
			v.Secrets[f.Name] = values[f.Name] != ""
		}
	}
	return v
}

// valuesOf maps a stored channel onto its kind's fields: the url and secret columns, and the config (public) and
// secrets (sealed) columns for the others. A value the kind does not declare (a Teams row's legacy secret) is left
// out.
func valuesOf(ch *storageif.OrgChannel) notify.Values {
	spec, ok := notify.Kind(ch.Kind)
	if !ok {
		return notify.Values{"url": ch.URL, "secret": ch.Secret}
	}
	v := notify.Values{}
	for _, f := range spec.FieldsIn(notify.ScopeOrg) {
		if c := storedValue(ch, f); c != "" {
			v[f.Name] = c
		}
	}
	return v
}

// storedValue is f's value in ch: its own column for url and secret, else the secrets or the config column.
func storedValue(ch *storageif.OrgChannel, f notify.FieldSpec) string {
	switch {
	case f.Name == "url":
		return ch.URL
	case f.Name == "secret":
		return ch.Secret
	case f.Secret:
		return ch.Secrets[f.Name]
	}
	return ch.Config[f.Name]
}

// storeValues puts a kind's validated values into ch's columns, as storedValue reads them.
func storeValues(ch *storageif.OrgChannel, spec notify.KindSpec, values notify.Values) {
	ch.URL, ch.Secret = values["url"], values["secret"]
	for _, f := range spec.FieldsIn(notify.ScopeOrg) {
		val := values[f.Name]
		switch {
		case f.Name == "url" || f.Name == "secret" || val == "":
		case f.Secret:
			if ch.Secrets == nil {
				ch.Secrets = map[string]string{}
			}
			ch.Secrets[f.Name] = val
		default:
			if ch.Config == nil {
				ch.Config = map[string]string{}
			}
			ch.Config[f.Name] = val
		}
	}
}

// List returns the org's channels, enabled or not.
func (s *ChannelService) List(ctx context.Context, orgID string) ([]ChannelView, error) {
	channels, err := s.store.ListOrgChannels(ctx, orgID, false)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelView, len(channels))
	for i, ch := range channels {
		out[i] = viewOf(ch)
	}
	return out, nil
}

// ChannelChoices is what an organisation admin's channel form needs from the deployment.
type ChannelChoices struct {
	// Kinds are the organisation kinds this deployment delivers (email only with mail settings).
	Kinds []string
	// Events are the event types a channel can subscribe to; DefaultEvents what one that never chose receives.
	Events, DefaultEvents []notify.EventType
	// HostsRestricted: PAD_NOTIFY_ALLOWED_HOSTS is set, and a channel URL off it is refused.
	HostsRestricted bool
	// AllowedHosts is that list when the operator made it visible (PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE), else nil: the
	// list may name other customers' hosts (a tenant's Teams or Jira host), which no organisation should learn.
	AllowedHosts []string
}

// Choices returns the deployment's ChannelChoices.
func (s *ChannelService) Choices() ChannelChoices {
	c := ChannelChoices{Events: notify.OrgEvents(), DefaultEvents: notify.DefaultOrgEvents(), HostsRestricted: len(s.allowed) > 0}
	for _, k := range notify.Kinds(notify.ScopeOrg) {
		if s.offers(k) {
			c.Kinds = append(c.Kinds, k.ID)
		}
	}
	if s.showAllowed && len(s.allowed) > 0 {
		c.AllowedHosts = slices.Clone(s.allowed)
	}
	return c
}

// find returns the org's channel with id, or nil when the org has none by that id.
func (s *ChannelService) find(ctx context.Context, orgID, id string) (*storageif.OrgChannel, error) {
	channels, err := s.store.ListOrgChannels(ctx, orgID, false)
	if err != nil {
		return nil, err
	}
	for _, c := range channels {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, nil
}

// ChannelSave is a create (no ID, or an ID the org has no channel by) or an edit of an organisation's channel. An
// edit keeps what it omits:
//   - Name nil keeps the name; Enabled nil keeps the state (a create is enabled);
//   - Secret "" keeps the signing secret; Clear ["secret"] removes it;
//   - URL "" keeps the URL, and so does a URL equal to the listed one (a tab of the previous release re-sends what
//     it was shown), alone or with URLUnchanged. URLUnchanged with another URL is refused, and so is any other URL
//     carrying the redaction marker: a stale listing (the channel changed since) or a pasted one, never stored.
//   - Values: a field absent keeps its stored value, and so does a secret field sent as "" (jira's token, like
//     Secret); a public field sent as "" is cleared, which a required one refuses;
//   - Events nil keeps the subscription (a create receives the default set).
//
// Kind "" on an edit keeps the kind.
type ChannelSave struct {
	ID           string
	Name         *string
	Kind         string
	URL          string
	URLUnchanged bool
	Secret       string
	Clear        []string
	Enabled      *bool
	// Values are the kind's fields beyond url and secret by name: jira's email, token and project, an email
	// channel's recipients (member user ids, comma-separated).
	Values map[string]string
	// Events are the event types the channel receives (notify.OrgEvents).
	Events []string
}

// channelInvalid is a save's validation failure: 400 INVALID_CHANNEL naming the field.
func channelInvalid(field, msg string) error {
	return apperr.Invalid("INVALID_CHANNEL", msg).WithDetails(map[string]any{"field": field})
}

// Save validates and stores a channel of the org and returns it as the API shows it.
func (s *ChannelService) Save(ctx context.Context, orgID string, in ChannelSave) (ChannelView, error) {
	var existing *storageif.OrgChannel
	if in.ID != "" {
		var err error
		if existing, err = s.find(ctx, orgID, in.ID); err != nil {
			return ChannelView{}, err
		}
	}
	kind := strings.TrimSpace(in.Kind)
	if kind == "" && existing != nil {
		kind = existing.Kind
	}
	spec, ok := notify.Kind(kind)
	if !ok || spec.Scope&notify.ScopeOrg == 0 {
		return ChannelView{}, channelInvalid("kind", fmt.Sprintf("kind must be one of %s", strings.Join(notify.KindIDs(notify.ScopeOrg), ", ")))
	}
	if !s.offers(spec) && (existing == nil || existing.Kind != kind) {
		return ChannelView{}, channelInvalid("kind", fmt.Sprintf("this deployment offers no %s channel (it has no mail settings)", kind))
	}
	if existing == nil && in.URLUnchanged {
		return ChannelView{}, channelInvalid("url", "urlUnchanged needs a saved channel")
	}
	url := submitted(in, "url")
	if existing != nil && url != "" && url == viewOf(existing).URL {
		url = ""
	}
	switch {
	case url == "":
	case in.URLUnchanged:
		return ChannelView{}, channelInvalid("url", "urlUnchanged with a new url")
	case notify.HasRedactionMarker(url):
		return ChannelView{}, channelInvalid("url", "this is the redacted placeholder; enter the full URL")
	}
	for _, name := range in.Clear {
		f, declared := spec.FieldIn(notify.ScopeOrg, name)
		switch {
		case !declared || !f.Secret || f.Required:
			return ChannelView{}, channelInvalid(name, fmt.Sprintf("a %s channel cannot clear %q", kind, name))
		case submitted(in, name) != "":
			return ChannelView{}, channelInvalid(name, fmt.Sprintf("%s is both set and cleared", name))
		}
	}
	// A value for a field the kind does not declare (a secret on Teams, a token on a webhook) is refused, not dropped.
	undeclared := slices.Sorted(maps.Keys(in.Values))
	if url != "" {
		undeclared = append(undeclared, "url")
	}
	if in.Secret != "" {
		undeclared = append(undeclared, "secret")
	}
	for _, name := range undeclared {
		if _, ok := spec.FieldIn(notify.ScopeOrg, name); !ok && submitted(in, name) != "" {
			return ChannelView{}, channelInvalid(name, fmt.Sprintf("a %s channel has no %s", kind, name))
		}
	}

	var kept notify.Values
	if existing != nil {
		kept = valuesOf(existing)
	}
	values := notify.Values{}
	for _, f := range spec.FieldsIn(notify.ScopeOrg) {
		v := submitted(in, f.Name)
		_, sent := in.Values[f.Name]
		switch {
		case slices.Contains(in.Clear, f.Name):
			continue
		case f.Name == "url":
			v = url
		case sent && !f.Secret:
			// A public field sent empty is cleared; a required one is then refused by name.
			if v = strings.TrimSpace(v); v != "" {
				values[f.Name] = v
			}
			continue
		}
		if v == "" {
			v = kept[f.Name]
		}
		if v != "" {
			values[f.Name] = v
		}
	}
	if existing != nil && existing.Unreadable {
		// Nothing sealed could be kept: a required sealed value not entered again is named with the reason.
		for _, f := range spec.FieldsIn(notify.ScopeOrg) {
			if f.Required && values[f.Name] == "" && (f.Secret || f.Name == "url") {
				return ChannelView{}, channelInvalid(f.Name, f.Name+": the stored value cannot be decrypted with this deployment's encryption key (the key changed); enter it again")
			}
		}
	}
	if err := notify.Validate(ctx, kind, notify.ScopeOrg, values, s.policy, s.allowed...); err != nil {
		var fe *notify.FieldError
		if errors.As(err, &fe) {
			return ChannelView{}, channelInvalid(fe.Field, fe.Error())
		}
		return ChannelView{}, err
	}
	for _, f := range spec.FieldsIn(notify.ScopeOrg) {
		// Only what this save sends is checked against the roster: a recipient who left since is skipped at delivery
		// and must not block a pause.
		if _, sent := in.Values[f.Name]; f.Members && sent {
			if err := s.checkMembers(ctx, orgID, f.Name, values[f.Name]); err != nil {
				return ChannelView{}, err
			}
		}
	}
	var events []string
	if existing != nil {
		events = existing.Events
	}
	if in.Events != nil {
		parsed, err := notify.ParseOrgEvents(in.Events)
		if err != nil {
			return ChannelView{}, channelInvalid("events", err.Error())
		}
		events = make([]string, len(parsed))
		for i, t := range parsed {
			events[i] = string(t)
		}
	}

	ch := &storageif.OrgChannel{ID: in.ID, OrgID: orgID, Kind: kind, Events: events, Enabled: true, CreatedAt: time.Now().UTC()}
	storeValues(ch, spec, values)
	if existing != nil {
		ch.Name, ch.Enabled, ch.CreatedAt = existing.Name, existing.Enabled, existing.CreatedAt
	}
	if ch.ID == "" {
		ch.ID = uuid.NewString()
	}
	if in.Name != nil {
		ch.Name = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		ch.Enabled = *in.Enabled
	}
	if err := s.store.SaveOrgChannel(ctx, ch); err != nil {
		return ChannelView{}, err
	}
	return viewOf(ch), nil
}

// submitted is the value a save carries for field name: the URL trimmed, the secret byte for byte (it is an HMAC key),
// another field as sent.
func submitted(in ChannelSave, name string) string {
	switch name {
	case "url":
		return strings.TrimSpace(in.URL)
	case "secret":
		return in.Secret
	}
	return in.Values[name]
}

// offers reports whether the deployment delivers kind k: a mail kind needs an enabled mail sender, and a kind with a
// members field the roster to resolve it.
func (s *ChannelService) offers(k notify.KindSpec) bool {
	if k.Mail && (s.mailer == nil || !s.mailer.Enabled()) {
		return false
	}
	for _, f := range k.FieldsIn(notify.ScopeOrg) {
		if f.Members && s.members == nil {
			return false
		}
	}
	return true
}

// checkMembers refuses a recipient that is not a member of the org with a verified email address: an organisation's
// email goes to members only, at the address their account proved, so an admin cannot make the server mail anyone
// else. Not a member and not verified read alike, so the answer tells nothing about accounts outside the org.
func (s *ChannelService) checkMembers(ctx context.Context, orgID, field, value string) error {
	ids := strings.Split(value, ",")
	addrs, err := s.memberAddresses(ctx, orgID, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := addrs[id]; !ok {
			return channelInvalid(field, fmt.Sprintf("recipient %q is not a member of this organisation with a verified email address", id))
		}
	}
	return nil
}

// memberAddresses maps those of ids that are members of orgID with a verified email address to that address.
func (s *ChannelService) memberAddresses(ctx context.Context, orgID string, ids []string) (map[string]string, error) {
	if s.members == nil {
		return nil, errors.New("no member directory to resolve email recipients")
	}
	org, err := s.members.LoadOrg(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("load the organisation's members: %w", err)
	}
	member := map[string]bool{}
	for _, m := range org.Members {
		member[m.UserID] = true
	}
	var wanted []string
	for _, id := range ids {
		if member[id] {
			wanted = append(wanted, id)
		}
	}
	out := map[string]string{}
	if len(wanted) == 0 {
		return out, nil
	}
	users, err := s.members.LoadUsersByIDs(ctx, wanted)
	if err != nil {
		return nil, fmt.Errorf("load the recipients: %w", err)
	}
	for _, id := range wanted {
		if u := users[id]; u != nil && u.EmailVerified && u.Email != "" {
			out[id] = u.Email
		}
	}
	return out, nil
}

// Delete removes the org's channel id; an unknown id is not an error.
func (s *ChannelService) Delete(ctx context.Context, orgID, id string) error {
	return s.store.DeleteOrgChannel(ctx, orgID, id)
}

// errChannelUnreadable is the delivery's refusal of a channel whose sealed values no configured key opens.
var errChannelUnreadable = errors.New("the channel's stored URL or secrets cannot be decrypted with the configured encryption keys")

// build makes ch's notifier for delivery. It validates the stored values again, the host allow-list included, and its
// client refuses a host off the list: a channel saved before its host left the list is not delivered to. An email
// channel's recipients are resolved to the verified addresses of those still members, so a member who left or was
// erased is not mailed. The notifier delivers only the events the channel subscribes to.
func (s *ChannelService) build(ctx context.Context, ch *storageif.OrgChannel) (notify.AlertNotifier, error) {
	if ch.Unreadable {
		return nil, errChannelUnreadable
	}
	spec, ok := notify.Kind(ch.Kind)
	if !ok {
		return nil, fmt.Errorf("unknown channel kind %q", ch.Kind)
	}
	values := valuesOf(ch)
	deps := notify.Deps{
		Client: s.clients.For(s.policy, spec.Egress.WithAllowed(s.allowed)), Policy: s.policy, AllowedHosts: s.allowed,
		OrgID: ch.OrgID, Envelope: s.env, Mailer: s.mailer,
	}
	for _, f := range spec.FieldsIn(notify.ScopeOrg) {
		if f.Members && values[f.Name] != "" {
			ids := strings.Split(values[f.Name], ",")
			addrs, err := s.memberAddresses(ctx, ch.OrgID, ids)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				if a, ok := addrs[id]; ok {
					deps.MemberAddresses = append(deps.MemberAddresses, a)
				}
			}
		}
	}
	n, err := notify.Build(ctx, ch.Kind, notify.ScopeOrg, values, deps)
	if err != nil {
		return nil, err
	}
	return notify.Subscribed(n, notify.SubscribedEvents(ch.Events)), nil
}

// OrgNotifiers builds the org's enabled channels for delivery (the scanner builds them once per sweep). A channel
// that no longer validates, of a kind this release does not know, or whose sealed values cannot be opened, is
// skipped and logged by its id, without its URL; the others are delivered to.
func (s *ChannelService) OrgNotifiers(ctx context.Context, orgID string) ([]notify.AlertNotifier, error) {
	channels, err := s.store.ListOrgChannels(ctx, orgID, true)
	if err != nil {
		return nil, err
	}
	built := make([]notify.AlertNotifier, 0, len(channels))
	for _, ch := range channels {
		n, err := s.build(ctx, ch)
		if err != nil {
			logger.Warn("notify: org channel not delivered to", "orgId", orgID, "channelId", ch.ID, "kind", ch.Kind, "err", notify.RedactError(err))
			if ch.Unreadable {
				errreport.CaptureError(ctx, "notify.org_channel_unreadable", err, errreport.Attrs{"orgId": orgID, "channelId": ch.ID, "kind": ch.Kind})
			}
			continue
		}
		built = append(built, n)
	}
	return built, nil
}

// Enqueue delivers ev to n on the runner, detached from ctx (cancelling mid-POST would abandon a half-sent alert) and
// bounded by the delivery timeout. At the runner's cap it waits for a slot while ctx lives: a sweep slows down
// instead of dropping an alert it has recorded. done, when not nil, runs once the delivery has ended, or at once
// when it was not started; it reports whether it started.
func (s *ChannelService) Enqueue(ctx context.Context, n notify.AlertNotifier, ev notify.Event, done func()) bool {
	ev = ev.Stamped(time.Now())
	delivery := context.WithoutCancel(ctx)
	started := s.runner.GoWait(ctx, func() {
		if done != nil {
			defer done()
		}
		notify.Deliver(delivery, n, ev)
	})
	if !started && done != nil {
		done()
	}
	return started
}

// Test delivers a synthetic event to the org's channel id and waits for the answer: the admin's check that the
// destination works. The delivery runs on the runner, detached from the request and bounded by 15 s; a full runner
// answers 503 rather than queueing a request. A failure is 502 CHANNEL_DELIVERY_FAILED with coarse details (the
// endpoint's status, or what failed about the connection), never a resolved address or the URL (B13).
func (s *ChannelService) Test(ctx context.Context, orgID, id string) error {
	ch, err := s.find(ctx, orgID, id)
	if err != nil {
		return err
	}
	if ch == nil {
		return apperr.NotFound("", "channel not found")
	}
	n, err := s.build(ctx, ch)
	if err != nil {
		var fe *notify.FieldError
		switch {
		case ch.Unreadable:
			logger.FromContext(ctx).Warn("org channel test refused: its stored values cannot be decrypted", "channelId", ch.ID, "kind", ch.Kind)
			return apperr.Invalid("CHANNEL_UNREADABLE", "this channel's stored URL cannot be decrypted (the encryption key changed); enter the URL again or remove the channel").
				WithDetails(map[string]any{"field": "url"})
		case errors.Is(err, notify.ErrKindUnavailable):
			return channelInvalid("kind", fmt.Sprintf("this deployment cannot deliver a %s channel (no mail settings)", ch.Kind))
		case errors.As(err, &fe):
			return channelInvalid(fe.Field, fe.Error())
		}
		return apperr.Invalid("INVALID_CHANNEL", err.Error())
	}
	ev := notify.Event{
		ID:      notify.NewEventID(),
		Type:    notify.EventTest,
		Title:   "baki channel test",
		Message: fmt.Sprintf("This is a test delivery to %q (%s). If you can read this, the channel works.", ch.Name, ch.Kind),
		At:      time.Now().UTC(),
	}
	done := make(chan error, 1)
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), channelTestTimeout)
	started := s.runner.Go(func() {
		defer cancel()
		// A panicking notifier answers as a failed delivery instead of leaving the request waiting.
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("delivery panicked: %v", r)
			}
		}()
		done <- n.Notify(dctx, ev)
	})
	if !started {
		cancel()
		return apperr.Unavailable("CHANNEL_DELIVERIES_BUSY", "channel deliveries are busy; try the test again shortly")
	}
	if err := <-done; err != nil {
		err = notify.RedactError(err)
		logger.FromContext(ctx).Warn("org channel test failed", "channelId", ch.ID, "kind", ch.Kind, "err", err)
		return channelDeliveryFailed(err)
	}
	return nil
}

// channelDeliveryFailed is a failed test delivery as the API answers it: the endpoint's HTTP status, or a fixed
// description of the connection failure (egress.DescribeFailure), in details.reason.
func channelDeliveryFailed(err error) error {
	details := map[string]any{}
	var se *notify.StatusError
	switch {
	case errors.As(err, &se):
		details["status"] = se.Status
		details["reason"] = fmt.Sprintf("the endpoint answered HTTP %d", se.Status)
	default:
		reason, ok := egress.DescribeFailure(err)
		if !ok {
			reason = "the delivery failed"
		}
		details["reason"] = reason
	}
	return apperr.BadGateway("CHANNEL_DELIVERY_FAILED", "test delivery failed: "+details["reason"].(string)).WithDetails(details).Wrap(err)
}
