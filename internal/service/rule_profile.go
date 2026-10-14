package service

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/reqtx"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"
)

// ruleProfileTTL bounds profile reuse: invalidation is exact per replica only —
// 30s bounds cross-replica staleness without re-querying storage every run.
const ruleProfileTTL = 30 * time.Second

// ruleProfileCacheMax bounds the cache by reader count (an org's own entry and one per caller who resolved it);
// entries expire by TTL anyway, so this only caps the readers resolving within one TTL (oldest re-resolves).
const ruleProfileCacheMax = 1024

// ruleProfiles caches resolved org profiles per reader (ADR 0017). An org's custom rules are visible to its
// members only (row-level security), so a profile is what its reader's scope showed and is served to that reader
// alone: a resolve in a request's scope is its caller's entry (OrgUser); one without a scope (a background job, a
// route exempt from it) reads every row on the pool, the org's own (Org). A non-member's resolve, which sees no
// custom rules, can no longer hide them from the members, nor a pool read show them on a scoped route.
// deploymentProfile is the one profile without an org: the deployment settings and the operator's rule file.
var (
	ruleProfiles = cache.Declare("rules.profiles",
		cache.Keys(cache.KindTenant),
		cache.Shared(cache.MechanismInvalidation, "a rule or membership change on any replica drops it on every replica once committed; the 30 s TTL bounds a lost message"))
	deploymentProfile = cache.Declare("rules.deployment-profile",
		cache.Global("the deployment layer alone (no org): deployment settings and the operator's rule file, the same for every reader"),
		cache.Shared(cache.MechanismInvalidation, "a deployment settings change on any replica drops it on every replica once committed; the 30 s TTL bounds a lost message"))
)

// DeploymentCustomRules are the PAD_CUSTOM_RULES rules compiled once at boot — a
// named type (fx disambiguation) and the node breaking the resolver cycle.
type DeploymentCustomRules []analyzer.Rule

// RuleProfile is the fully resolved analysis configuration for one org (or no
// org, locally): the settings the engine reads and the exact rule set to run.
type RuleProfile struct {
	Settings *models.AppSettings
	Rules    []analyzer.Rule
	// Narrowed: the reader is not a member of the org and reads under row-level security, which hides the org's
	// custom rules from them (a collaborator the flow was shared with). An analysis under it is the reader's
	// alone: it must not stand in for the org's report (AnalysisService records none).
	Narrowed bool
}

// RuleProfileResolver answers "what rules, at what severity, for this flow's
// org" per tenant (pre-R4 one tenant's toggle changed every tenant). Resolution
// is layered, most specific winning, merged per rule id: org profile →
// deployment settings + PAD_CUSTOM_RULES → built-in defaults.
type RuleProfileResolver struct {
	backend  storageif.StorageBackend
	settings SettingsProvider
	// deploymentRules are the PAD_CUSTOM_RULES file's rules, boot-compiled;
	// deployment-wide on purpose — an operator file is a legitimate way to ship
	// rules to every tenant.
	deploymentRules DeploymentCustomRules

	store *profileStore
	// gen fences a resolve against Invalidate: a profile read before a change committed is not cached.
	gen eventbus.Gen[string]
	// flights deduplicates cold resolves per cache key, never across readers the key separates (see Resolve).
	flights singleflight.Group
}

// profileStore holds the cached profiles by key (either spec's); each entry remembers its org ("" for the
// deployment profile), which Invalidate drops by.
type profileStore struct {
	mu sync.RWMutex
	m  map[cache.Key]*cachedProfile
}

type cachedProfile struct {
	orgID    string
	profile  *RuleProfile
	resolved time.Time
}

// Len and Cost size the org profiles for the cache metrics (one unit per profile).
func (s *profileStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for k := range s.m {
		if k.Spec() == ruleProfiles {
			n++
		}
	}
	return n
}

func (s *profileStore) Cost() int64 { return int64(s.Len()) }

func NewRuleProfileResolver(backend storageif.StorageBackend, settings SettingsProvider, deploymentRules DeploymentCustomRules) *RuleProfileResolver {
	store := &profileStore{m: make(map[cache.Key]*cachedProfile)}
	cache.Track(ruleProfiles, store)
	return &RuleProfileResolver{
		backend:         backend,
		settings:        settings,
		deploymentRules: deploymentRules,
		store:           store,
	}
}

// Invalidate drops an org's cached profiles, every reader's (every entry when
// orgID is empty — a deployment-settings change affects all orgs, which inherit
// from it), and refuses the resolves in flight.
func (r *RuleProfileResolver) Invalidate(orgID string) {
	if r == nil {
		return
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if orgID == "" {
		r.gen.BumpAll()
	} else {
		r.gen.Bump(orgID)
	}
	for k, c := range r.store.m {
		if orgID == "" || c.orgID == orgID {
			delete(r.store.m, k)
			k.Spec().RecordEviction(cache.EvictDeleted)
		}
	}
}

// SubscribeRuleProfiles drops cached rule profiles when what they are resolved from changes: the deployment
// settings (every org inherits them), an org's settings or custom rules (SettingsChanged{OrgSettings}), an org's
// deletion, and its membership, which decides what a member's entry shows. Subscribe it after the settings cache,
// whose fresh value the next resolve reads.
func SubscribeRuleProfiles(bus *eventbus.Bus, r *RuleProfileResolver) {
	eventbus.Subscribe(bus, eventbus.Sync, "rule-profiles", func(_ context.Context, e events.SettingsChanged) {
		switch e.Scope {
		case events.DeploymentSettings:
			r.Invalidate("")
		case events.OrgSettings:
			if e.ID != "" {
				r.Invalidate(e.ID)
			}
		}
	}, eventbus.Replicated(func(e events.SettingsChanged) []eventbus.Invalidation {
		switch {
		case e.Scope == events.DeploymentSettings:
			// Every org inherits the deployment layer: both caches, whole.
			return []eventbus.Invalidation{{Key: "rules.deployment-profile"}, {Key: "rules.profiles"}}
		case e.Scope == events.OrgSettings && e.ID != "":
			return []eventbus.Invalidation{{Key: "rules.profiles", Scope: eventbus.NewScope("org", e.ID)}}
		}
		return nil
	}, "rules.deployment-profile", "rules.profiles"))
	eventbus.Subscribe(bus, eventbus.Sync, "rule-profiles", func(_ context.Context, e events.OrgMemberChanged) {
		switch {
		case e.OrgID == "":
		case e.Kind == events.OrgDeleted:
			r.Invalidate(e.OrgID)
		case e.Kind == events.OrgMemberAdded || e.Kind == events.OrgMemberRemoved:
			r.invalidateReader(e.OrgID, e.UserID)
		}
	}, eventbus.Replicated(func(e events.OrgMemberChanged) []eventbus.Invalidation {
		switch {
		case e.OrgID == "":
		case e.Kind == events.OrgDeleted:
			return []eventbus.Invalidation{{Key: "rules.profiles", Scope: eventbus.NewScope("org", e.OrgID)}}
		case e.Kind == events.OrgMemberAdded || e.Kind == events.OrgMemberRemoved:
			// With the reader when there is one: a member's own entry decides what their membership shows.
			scope := eventbus.NewScope("org", e.OrgID)
			if e.UserID != "" {
				scope["user"] = e.UserID
			}
			return []eventbus.Invalidation{{Key: "rules.profiles", Scope: scope}}
		}
		return nil
	}, "rules.profiles"))
	// What a peer dropped. The deployment profile only ever goes whole, and Invalidate("") drops both caches (every
	// org inherits the deployment layer), so a message for either key does that. A resync (empty scope) drops all.
	bus.OnRemote("rules.deployment-profile", func(eventbus.Scope) { r.Invalidate("") })
	bus.OnRemote("rules.profiles", func(s eventbus.Scope) {
		if org := s["org"]; org != "" {
			r.invalidateReader(org, s["user"])
			return
		}
		r.Invalidate("")
	})
}

// invalidateReader drops userID's own entry of orgID's profile, whose view their membership decides, and refuses
// the org's resolves in flight; every entry of the org without a user id.
func (r *RuleProfileResolver) invalidateReader(orgID, userID string) {
	if userID == "" {
		r.Invalidate(orgID)
		return
	}
	k, err := ruleProfiles.Key(cache.OrgUser(orgID, userID))
	if err != nil {
		r.Invalidate(orgID)
		return
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	r.gen.Bump(orgID)
	if _, ok := r.store.m[k]; ok {
		delete(r.store.m, k)
		ruleProfiles.RecordEviction(cache.EvictDeleted)
	}
}

// Resolve returns the profile for orgID (empty = pre-R4 deployment behaviour).
// Storage failures degrade to the deployment layer, logged — an org losing its
// rules for a run beats a flow that cannot be analyzed at all.
func (r *RuleProfileResolver) Resolve(ctx context.Context, orgID string) *RuleProfile {
	if r == nil {
		return &RuleProfile{Settings: nil, Rules: analyzer.AllRules()}
	}
	k, entryOrg, cacheable := r.profileKey(ctx, orgID)
	if !cacheable {
		return r.resolveUncached(context.WithoutCancel(ctx), orgID)
	}
	if p := r.store.get(k); p != nil {
		k.Spec().RecordHit()
		return p
	}
	k.Spec().RecordMiss()

	// One cold build per key: concurrent analyses of the same org by the same
	// reader (a "fix all" batch, the scanner sweep, interactive analyses) each
	// ran the full resolve — two storage reads plus a regex compile per custom
	// rule — racing to write identical cache entries. The flight is keyed like
	// the cache, so it never hands one reader's view to another. The resolve
	// runs detached from the caller's cancellation so a caller that abandons
	// its request mid-resolve does not hand every waiter its degraded
	// (storage-unavailable) profile; the inner re-check serves callers that
	// waited out a flight which already refreshed the entry. The builder
	// never returns an error, so the ignored flight error is inert.
	// The flight stores what it resolved only if no invalidation ran since it
	// began (and its transaction announced no change of its own): a resolve
	// that read before a change committed must not be cached for the TTL.
	v, _, _ := r.flights.Do(k.String(), func() (any, error) {
		if p := r.store.get(k); p != nil {
			return p, nil
		}
		captured := r.gen.Load(entryOrg)
		profile := r.resolveUncached(context.WithoutCancel(ctx), orgID)
		// A fail-open scope that could not start read the pool instead: not the view k names.
		after, _, _ := r.profileKey(ctx, orgID)
		r.store.mu.Lock()
		if after == k && r.gen.FillOK(ctx, entryOrg, captured) {
			now := time.Now()
			r.store.pruneLocked(now)
			r.store.m[k] = &cachedProfile{orgID: entryOrg, profile: profile, resolved: now}
			k.Spec().RecordStore()
		}
		r.store.mu.Unlock()
		return profile, nil
	})
	return v.(*RuleProfile)
}

// profileKey is the cache key of orgID's profile as ctx's reader sees it and the org Invalidate drops it by. The
// deployment layer alone (no org, or no backend to read one from) is one entry for every reader; an org's profile
// is its reader's (profileReader). A scope without a calling user id is nobody's entry: false, resolved uncached.
func (r *RuleProfileResolver) profileKey(ctx context.Context, orgID string) (k cache.Key, entryOrg string, ok bool) {
	if orgID == "" || r.backend == nil {
		k, err := deploymentProfile.Key()
		return k, "", err == nil
	}
	reader := profileReader(ctx, orgID)
	if reader.IsZero() {
		return cache.Key{}, "", false
	}
	k, err := ruleProfiles.Key(reader)
	return k, orgID, err == nil
}

// profileReader is whose view of orgID's custom rules a resolve under ctx reads. In a request's scope it is the
// caller's, which row-level security shows them to only when a member. Without one (a background job, a route
// exempt from the scope) the read runs on the pool and sees every row, whoever asks: the org's own view. A scope
// without a calling user id is nobody's (zero).
func profileReader(ctx context.Context, orgID string) cache.Part {
	if !reqtx.ReadsFiltered(ctx) {
		return cache.Org(orgID)
	}
	a := auth.ActorFromContext(ctx)
	if a == nil || a.Kind == auth.ActorSystem {
		return cache.Part{}
	}
	return cache.OrgUser(orgID, a.UserID)
}

// get returns the fresh profile under k, nil when none is.
func (s *profileStore) get(k cache.Key) *RuleProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.m[k]; ok && time.Since(c.resolved) < ruleProfileTTL {
		return c.profile
	}
	return nil
}

// pruneLocked makes room for one insert: it drops expired profiles — reads
// skip them, but nothing removed them, so every org that ever ran an analysis
// (deleted orgs included) kept its rule set, compiled custom-rule regexes and
// settings clone for the life of the process — then, while the cache is at
// ruleProfileCacheMax, the oldest. Caller holds s.mu. One pass per insert, and
// a key inserts at most once per ruleProfileTTL.
func (s *profileStore) pruneLocked(now time.Time) {
	for k, c := range s.m {
		if now.Sub(c.resolved) >= ruleProfileTTL {
			delete(s.m, k)
			k.Spec().RecordEviction(cache.EvictExpired)
		}
	}
	for len(s.m) >= ruleProfileCacheMax {
		var oldest cache.Key
		oldestAt, first := time.Time{}, true
		for k, c := range s.m {
			if first || c.resolved.Before(oldestAt) {
				oldest, oldestAt, first = k, c.resolved, false
			}
		}
		delete(s.m, oldest)
		oldest.Spec().RecordEviction(cache.EvictCapacity)
	}
}

// cachedFor is the profile cached for a background job's resolve of orgID, nil when none is fresh (tests).
func (r *RuleProfileResolver) cachedFor(orgID string) *RuleProfile {
	k, _, ok := r.profileKey(context.Background(), orgID)
	if !ok {
		return nil
	}
	return r.store.get(k)
}

func (r *RuleProfileResolver) resolveUncached(ctx context.Context, orgID string) *RuleProfile {
	// Base layer: deployment settings + built-ins + the operator's rule file.
	settings := r.settings.Get()
	rules := analyzer.AllRules()
	rules = append(rules, r.deploymentRules...)

	if orgID == "" || r.backend == nil {
		return &RuleProfile{Settings: settings, Rules: rules}
	}
	narrowed := r.outsider(ctx, orgID)

	// Org settings layer: merge the org's per-rule overrides over the
	// deployment's, entry by entry. A wholesale replace would silently drop
	// every deployment-level rule config the org never touched.
	if orgSettings, err := r.backend.LoadOrgSettings(ctx, orgID); err != nil {
		logger.Warn("rule profile: org settings unavailable, using deployment defaults",
			"orgId", orgID, "error", err)
	} else if orgSettings != nil && len(orgSettings.Analysis.Rules) > 0 {
		merged := settings.Clone()
		if merged.Analysis.Rules == nil {
			merged.Analysis.Rules = make(map[string]models.RuleConfig, len(orgSettings.Analysis.Rules))
		}
		for id, rc := range orgSettings.Analysis.Rules {
			merged.Analysis.Rules[id] = toModelRuleConfig(rc)
		}
		settings = merged
	}

	// Org custom-rule layer.
	orgRules, err := r.backend.ListOrgCustomRules(ctx, orgID, true)
	if err != nil {
		logger.Warn("rule profile: org custom rules unavailable, using deployment rules only",
			"orgId", orgID, "error", err)
		return &RuleProfile{Settings: settings, Rules: rules, Narrowed: narrowed}
	}
	rules = append(rules, compileOrgRules(orgID, orgRules)...)
	return &RuleProfile{Settings: settings, Rules: rules, Narrowed: narrowed}
}

// outsider reports whether ctx's reader reads orgID's rows filtered as a non-member (RuleProfile.Narrowed). An
// unknown membership counts as outside, so such a view is never taken for the org's; a read on the pool, or in a
// scope without a calling user, is not a member's or an outsider's and keeps the org's view.
func (r *RuleProfileResolver) outsider(ctx context.Context, orgID string) bool {
	if !reqtx.ReadsFiltered(ctx) {
		return false
	}
	a := auth.ActorFromContext(ctx)
	if a == nil || a.Kind == auth.ActorSystem || a.UserID == "" {
		return false
	}
	_, member, err := r.backend.OrgMemberRole(ctx, orgID, a.UserID)
	return err != nil || !member
}

// toModelRuleConfig converts the storage-layer RuleConfig to the analyzer's.
//
// The two structs are field-identical, and SystemService converts whole
// settings objects with a JSON round-trip. This does not: resolution runs on
// the analysis path, and marshalling an entire AppSettings per rule (or per
// run) to move three fields is wasteful. Options is COPIED rather than aliased
// — the resolved profile is cached and shared across concurrent analyses, so
// handing out the stored map would let one run mutate another's configuration.
func toModelRuleConfig(rc storageif.RuleConfig) models.RuleConfig {
	out := models.RuleConfig{Enabled: rc.Enabled, Severity: rc.Severity}
	if len(rc.Options) > 0 {
		out.Options = make(map[string]interface{}, len(rc.Options))
		for k, v := range rc.Options {
			out.Options[k] = v
		}
	}
	return out
}
