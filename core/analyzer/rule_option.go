package analyzer

import (
	"encoding/json"

	"pad-core/models"
)

// ruleOptionInt reads an integer threshold from a rule's Options, falling back
// to def when the option is absent, non-numeric, or below min.
//
// The value's Go type depends on where the settings came from: JSON-decoded
// settings (the API, .bakirc.json, the desktop settings file) carry float64;
// models.DefaultSettings() and Go callers carry int. The four threshold rules
// each used to accept float64 ONLY, so an int threshold — the defaults
// included — was silently ignored and the rule fell back to its hardcoded
// constant. That constant happened to equal the default, which is why it
// never showed; the first Go caller to pass a different int would have been
// ignored without a trace.
func ruleOptionInt(settings *models.AppSettings, ruleID, key string, def, min int) int {
	if settings == nil {
		return def
	}
	rc, ok := settings.Analysis.Rules[ruleID]
	if !ok {
		return def
	}
	v, ok := rc.Options[key]
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
	if n < min {
		return def
	}
	return n
}
