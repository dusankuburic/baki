package analyzer

import (
	"encoding/json"

	"pad-core/models"
)

// ruleOption reads the integer threshold spec declares from a rule's Options,
// falling back to spec.Default when the option is absent, non-numeric, or
// below spec.Min. The same spec is listed in the rule's RuleMeta.Options, so
// the catalog and DefaultRuleConfigs see the default the rule applies. The
// value's Go type depends on the settings source (JSON-decoded settings carry
// float64; models.DefaultSettings() and Go callers carry int) — the threshold
// rules once accepted float64 ONLY, silently ignoring int thresholds, the
// defaults included.
func ruleOption(settings *models.AppSettings, ruleID string, spec OptionSpec) int {
	def := spec.Default
	if settings == nil {
		return def
	}
	rc, ok := settings.Analysis.Rules[ruleID]
	if !ok {
		return def
	}
	v, ok := rc.Options[spec.Key]
	if !ok {
		return def
	}
	var n int
	switch x := v.(type) {
	case float64:
		n = int(x)
	case float32:
		n = int(x)
	case int:
		n = x
	case int64:
		n = int(x)
	case int32:
		n = int(x)
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return def
		}
		n = int(i)
	default:
		return def
	}
	if n < spec.Min {
		return def
	}
	return n
}
