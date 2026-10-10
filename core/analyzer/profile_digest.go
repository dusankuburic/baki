package analyzer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"sync"

	"pad-core/models"
)

// RuleDigester is implemented by rules whose behaviour comes from CONFIGURATION
// rather than compiled code (CustomRule): same ID, different configuration.
type RuleDigester interface {
	RuleDigest() string
}

// ProfileDigest digests the analysis configuration a report depends on besides the document. Each rule
// contributes its ID, its RuleDigest (configured rules), whether it runs under settings (RuleEnabled), and the
// stage, revision, confidence and fix type registered under its ID, which the engine stamps on its findings; so
// does the parse-error fallback, which reports parse errors even when that rule is not in the set. The rule
// settings are folded type-aware: an option "8" and an option 8 differ (the rules read only numbers). The order
// of rules does not matter. Full SHA-256 in hex; in memory only (cache keys, the scanner's sweep skip), so free
// to change.
func ProfileDigest(settings *models.AppSettings, rules []Rule) string {
	type span struct{ lo, hi int }
	bufs := profileBuffers.Get().(*[2][]byte)
	defer profileBuffers.Put(bufs)
	buf := bufs[0][:0]
	spans := make([]span, 0, len(rules))
	for _, r := range rules {
		lo := len(buf)
		buf = appendProfileRule(buf, r, settings)
		spans = append(spans, span{lo, len(buf)})
	}
	slices.SortFunc(spans, func(x, y span) int { return bytes.Compare(buf[x.lo:x.hi], buf[y.lo:y.hi]) })

	out := bufs[1][:0]
	out = strconv.AppendInt(append(out, "rules:"...), int64(len(spans)), 10)
	for _, s := range spans {
		out = appendLenPrefixed(out, buf[s.lo:s.hi])
	}
	out = appendLenPrefixed(out, appendProfileRule(buf[:0], parseErrorProfileRule, settings))
	out = appendRuleSettings(out, settings)
	sum := sha256.Sum256(out)
	bufs[0], bufs[1] = buf, out
	return hex.EncodeToString(sum[:])
}

// profileBuffers recycles ProfileDigest's two encoding buffers: it runs on every analysis request, hits included.
var profileBuffers = sync.Pool{New: func() any { return new([2][]byte) }}

// parseErrorProfileRule stands for the engine's document-level parse-error findings in ProfileDigest.
var parseErrorProfileRule Rule = &ParseErrorRule{}

// appendProfileRule encodes what r contributes to a profile.
func appendProfileRule(b []byte, r Rule, settings *models.AppSettings) []byte {
	b = appendLenPrefixed(b, r.ID())
	if d, ok := r.(RuleDigester); ok {
		b = appendLenPrefixed(b, d.RuleDigest())
	} else {
		b = append(b, '-')
	}
	b = strconv.AppendBool(b, RuleEnabled(r, settings))
	if m := lookupMetaRef(r.ID()); m != nil {
		b = append(b, 'm')
		b = appendLenPrefixed(b, m.Stage)
		b = strconv.AppendInt(b, int64(m.Revision), 10)
		b = appendLenPrefixed(b, m.Confidence)
		b = appendLenPrefixed(b, m.AutoFix)
	}
	return b
}

// appendRuleSettings encodes settings.Analysis.Rules canonically (sorted rule ids and option keys) with every
// option value tagged by its type.
func appendRuleSettings(b []byte, settings *models.AppSettings) []byte {
	if settings == nil {
		return append(b, "no-settings"...)
	}
	rules := settings.Analysis.Rules
	if rules == nil {
		return append(b, "no-rules"...)
	}
	ids := make([]string, 0, len(rules))
	for id := range rules {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	b = strconv.AppendInt(append(b, "settings:"...), int64(len(ids)), 10)
	var keys []string
	for _, id := range ids {
		rc := rules[id]
		b = appendLenPrefixed(b, id)
		b = strconv.AppendBool(b, rc.Enabled)
		b = appendLenPrefixed(b, rc.Severity)
		keys = keys[:0]
		for k := range rc.Options {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b = strconv.AppendInt(b, int64(len(keys)), 10)
		b = append(b, ';')
		for _, k := range keys {
			b = appendLenPrefixed(b, k)
			b = appendOptionValue(b, rc.Options[k])
		}
	}
	return b
}

// appendOptionValue encodes one option value with its type.
func appendOptionValue(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, 'n')
	case bool:
		return strconv.AppendBool(append(b, 'b'), x)
	case string:
		return appendLenPrefixed(append(b, 's'), x)
	case int:
		return append(strconv.AppendInt(append(b, 'i'), int64(x), 10), ';')
	case float64:
		return append(strconv.AppendUint(append(b, 'f'), math.Float64bits(x), 16), ';')
	}
	// Anything else (other numeric types, json.Number, nested values): its Go type and its JSON, or its Go syntax
	// when it has no JSON.
	enc, err := json.Marshal(v)
	if err != nil {
		enc = fmt.Appendf(nil, "%#v", v)
	}
	b = appendLenPrefixed(append(b, 'x'), fmt.Sprintf("%T", v))
	return appendLenPrefixed(b, enc)
}
