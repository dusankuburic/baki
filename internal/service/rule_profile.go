package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/logger"
	"pad-core/models"
)

// ruleProfileTTL bounds how long a resolved profile is reused without
// re-reading storage.
//
// Writes invalidate explicitly (see Invalidate), which is exact on a single
// replica. It is NOT exact across replicas: replica B never sees replica A's
// invalidation, so without a TTL an org's rule change could take until the next
// restart to reach every replica. 30s keeps a stale profile bounded without
// making the analysis path re-query storage on every run.
const ruleProfileTTL = 30 * time.Second

// DeploymentCustomRules are the rules loaded from PAD_CUSTOM_RULES, compiled
// once at boot.
//
// A named type, not a bare []analyzer.Rule, so fx can tell it apart from any
// other rule slice in the graph. It is also what breaks the dependency cycle
// this feature would otherwise have: the resolver needs the deployment rules
// and the AnalysisService needs the resolver, so the rules have to be their own
// node rather than an accessor on the service.
type DeploymentCustomRules []analyzer.Rule

// RuleProfile is the fully resolved analysis configuration for one org (or for
// no org, in desktop/local mode): the settings the engine reads rule
// enable/severity/options from, and the exact rule set to run.
type RuleProfile struct {
	Settings *models.AppSettings
	Rules    []analyzer.Rule
}

// RuleProfileResolver answers "what rules, at what severity, for this flow's
// org".
//
// Before R4 the answer was a single process-global value: rule enable/severity
// came from the deployment settings singleton and custom rules from one
// server-side JSON file. In a multi-tenant deployment that meant one tenant's
// rule toggle changed analysis for every other tenant.
//
// Resolution is layered, most specific winning, merged PER RULE ID so an org
// that overrides two rules still inherits the rest:
//
//	org profile (org_settings + org_custom_rules)
//	  → deployment settings + PAD_CUSTOM_RULES file
//	    → built-in rule defaults
type RuleProfileResolver struct {
	backend  storageif.StorageBackend
	settings SettingsProvider
	// deploymentRules are the PAD_CUSTOM_RULES file's rules, compiled once at
	// boot. They remain deployment-wide on purpose: an operator-managed file is
	// a legitimate way to ship rules to every tenant, and removing it would
	// break existing installs.
	deploymentRules DeploymentCustomRules

	mu     sync.RWMutex
	cached map[string]*cachedProfile
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

// Invalidate drops an org's cached profile so the next analysis re-resolves it.
// An empty orgID clears every entry (a deployment-settings change affects all
// orgs, since they inherit from it).
func (r *RuleProfileResolver) Invalidate(orgID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if orgID == "" {
		clear(r.cached)
		return
	}
	delete(r.cached, orgID)
}

// Resolve returns the profile for orgID. An empty orgID (desktop/local mode, or
// a flow with no org) yields exactly the pre-R4 behaviour: deployment settings
// plus the file-loaded custom rules.
//
// Storage failures degrade to the deployment layer rather than failing the
// analysis: an org losing its custom rules for one run is bad, but a flow that
// cannot be analyzed at all because the settings table hiccuped is worse. The
// degradation is logged, never silent.
func (r *RuleProfileResolver) Resolve(ctx context.Context, orgID string) *RuleProfile {
	if r == nil {
		return &RuleProfile{Settings: nil, Rules: analyzer.AllRules()}
	}
	if p := r.cachedFor(orgID); p != nil {
		return p
	}

	profile := r.resolveUncached(ctx, orgID)

	r.mu.Lock()
	r.cached[orgID] = &cachedProfile{profile: profile, resolved: time.Now()}
	r.mu.Unlock()
	return profile
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
