package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/logger"
	"pad-core/models"
)

// ruleProfileTTL bounds profile reuse: invalidation is exact per replica only —
// 30s bounds cross-replica staleness without re-querying storage every run.
const ruleProfileTTL = 30 * time.Second

// ruleProfileCacheMax bounds the cache by org count; entries expire by TTL
// anyway, so this only caps orgs analyzing within one TTL (oldest re-resolves).
const ruleProfileCacheMax = 256

// DeploymentCustomRules are the PAD_CUSTOM_RULES rules compiled once at boot — a
// named type (fx disambiguation) and the node breaking the resolver cycle.
type DeploymentCustomRules []analyzer.Rule

// RuleProfile is the fully resolved analysis configuration for one org (or no
// org, locally): the settings the engine reads and the exact rule set to run.
type RuleProfile struct {
	Settings *models.AppSettings
	Rules    []analyzer.Rule
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

	mu     sync.RWMutex
	cached map[string]*cachedProfile
	// gen fences a resolve against Invalidate: a profile read before a change committed is not cached.
	gen eventbus.Gen[string]
	// flights deduplicates cold resolves per org (see Resolve).
	flights singleflight.Group
}

type cachedProfile struct {
	profile  *RuleProfile
	resolved time.Time
}

func NewRuleProfileResolver(backend storageif.StorageBackend, settings SettingsProvider, deploymentRules DeploymentCustomRules) *RuleProfileResolver {
	return &RuleProfileResolver{
		backend:         backend,
		settings:        settings,
		deploymentRules: deploymentRules,
		cached:          make(map[string]*cachedProfile),
	}
}

// Invalidate drops an org's cached profile (every entry when orgID is empty —
// a deployment-settings change affects all orgs, which inherit from it) and
// refuses the resolves in flight.
func (r *RuleProfileResolver) Invalidate(orgID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if orgID == "" {
		r.gen.BumpAll()
		clear(r.cached)
		return
	}
	r.gen.Bump(orgID)
	delete(r.cached, orgID)
}

// SubscribeRuleProfiles drops cached rule profiles when what they are resolved from changes: the deployment
// settings (every org inherits them), an org's settings or custom rules (SettingsChanged{OrgSettings}), and an
// org's deletion. Subscribe it after the settings cache, whose fresh value the next resolve reads.
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
	})
	eventbus.Subscribe(bus, eventbus.Sync, "rule-profiles", func(_ context.Context, e events.OrgMemberChanged) {
		if e.Kind == events.OrgDeleted && e.OrgID != "" {
			r.Invalidate(e.OrgID)
		}
	})
}

// Resolve returns the profile for orgID (empty = pre-R4 deployment behaviour).
// Storage failures degrade to the deployment layer, logged — an org losing its
// rules for a run beats a flow that cannot be analyzed at all.
func (r *RuleProfileResolver) Resolve(ctx context.Context, orgID string) *RuleProfile {
	if r == nil {
		return &RuleProfile{Settings: nil, Rules: analyzer.AllRules()}
	}
	if p := r.cachedFor(orgID); p != nil {
		return p
	}

	// One cold build per org: concurrent analyses for the same org (a
	// "fix all" batch, the scanner sweep, interactive analyses) each ran
	// the full resolve — two storage reads plus a regex compile per custom
	// rule — racing to write identical cache entries. The resolve runs
	// detached from the caller's cancellation so a caller that abandons
	// its request mid-resolve does not hand every waiter its degraded
	// (storage-unavailable) profile; the inner re-check serves callers that
	// waited out a flight which already refreshed the entry. The builder
	// never returns an error, so the ignored flight error is inert.
	// The flight stores what it resolved only if no invalidation ran since it
	// began (and its transaction announced no change of its own): a resolve
	// that read before a change committed must not be cached for the TTL.
	v, _, _ := r.flights.Do(orgID, func() (any, error) {
		if p := r.cachedFor(orgID); p != nil {
			return p, nil
		}
		captured := r.gen.Load(orgID)
		profile := r.resolveUncached(context.WithoutCancel(ctx), orgID)
		r.mu.Lock()
		if r.gen.FillOK(ctx, orgID, captured) {
			now := time.Now()
			r.pruneLocked(now)
			r.cached[orgID] = &cachedProfile{profile: profile, resolved: now}
		}
		r.mu.Unlock()
		return profile, nil
	})
	return v.(*RuleProfile)
}

// pruneLocked makes room for one insert: it drops expired profiles — reads
// skip them, but nothing removed them, so every org that ever ran an analysis
// (deleted orgs included) kept its rule set, compiled custom-rule regexes and
// settings clone for the life of the process — then, while the cache is at
// ruleProfileCacheMax, the oldest. Caller holds r.mu. One pass per insert, and
// an org inserts at most once per ruleProfileTTL.
func (r *RuleProfileResolver) pruneLocked(now time.Time) {
	for id, c := range r.cached {
		if now.Sub(c.resolved) >= ruleProfileTTL {
			delete(r.cached, id)
		}
	}
	for len(r.cached) >= ruleProfileCacheMax {
		oldest, oldestAt, first := "", time.Time{}, true
		for id, c := range r.cached {
			if first || c.resolved.Before(oldestAt) {
				oldest, oldestAt, first = id, c.resolved, false
			}
		}
		delete(r.cached, oldest)
	}
}

func (r *RuleProfileResolver) cachedFor(orgID string) *RuleProfile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.cached[orgID]; ok && time.Since(c.resolved) < ruleProfileTTL {
		return c.profile
	}
	return nil
}

func (r *RuleProfileResolver) resolveUncached(ctx context.Context, orgID string) *RuleProfile {
	// Base layer: deployment settings + built-ins + the operator's rule file.
	settings := r.settings.Get()
	rules := analyzer.AllRules()
	rules = append(rules, r.deploymentRules...)

	if orgID == "" || r.backend == nil {
		return &RuleProfile{Settings: settings, Rules: rules}
	}

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
		return &RuleProfile{Settings: settings, Rules: rules}
	}
	for _, stored := range orgRules {
		var cfg analyzer.CustomRuleConfig
		if err := json.Unmarshal(stored.Config, &cfg); err != nil {
			// Writes are validated before they land, so this means the row was
			// edited outside the API or the schema moved. Skip the one rule and
			// say which — a silent skip is how R0-5 found operators believing a
			// typo'd rule was enforcing.
			logger.Warn("rule profile: stored custom rule is unreadable, skipping",
				"orgId", orgID, "ruleId", stored.RuleID, "error", err)
			continue
		}
		compiled, err := analyzer.NewCustomRule(cfg)
		if err != nil {
			logger.Warn("rule profile: stored custom rule failed to compile, skipping",
				"orgId", orgID, "ruleId", stored.RuleID, "error", err)
			continue
		}
		rules = append(rules, compiled)
	}
	return &RuleProfile{Settings: settings, Rules: rules}
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
