package scrubber

import (
	"strings"

	"pad-core/models"
	"pad-core/parser"
)

// ScrubSourceText scrubs raw PAD source text (as read from disk for the
// selected-source-files chat context) with the same AST-level rules as
// ScrubDocument: the text is parsed, every value the document scrubber would
// mask (action-specific sensitive fields, credential-named keys, SET-TO
// targets, header literals and comment words of the Name pass, and any value
// the entropy fallback catches) is replaced with [REDACTED] wherever it
// occurs, and the generic regex pass runs afterwards. This closes the gap
// where PAD's `SET Password TO $”'secret”'` / `Text: $”'secret”'` syntax
// reached the model verbatim because it matches no key=value regex. A parse
// failure falls back to the regex-only ScrubText pass — degraded, but never
// worse than the previous behaviour.
func ScrubSourceText(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	scrubbed := text
	if doc, err := parser.ParseText(text, "scrub-source", int64(len(text))); err == nil && doc != nil {
		for _, secret := range collectSensitiveValues(doc) {
			scrubbed = strings.ReplaceAll(scrubbed, secret, "[REDACTED]")
		}
	}
	return ScrubText(scrubbed)
}

// collectSensitiveValues walks the parsed blocks and gathers the property
// VALUES the AST scrubber masks, for exact-occurrence replacement in the
// source text. Values that are pure `%Variable%` references carry no secret
// (the variable NAME is not a credential) and values shorter than 3 runes are
// skipped: replacing them would corrupt unrelated text for no secret-hiding
// gain — the same values are masked unconditionally in the document path
// where replacement is scoped to the property map.
func collectSensitiveValues(doc *models.FlowDocument) []string {
	var vals []string
	seen := map[string]struct{}{}
	add := func(v string) {
		if len([]rune(v)) < 3 || isPureVarRef(v) {
			return
		}
		if _, dup := seen[v]; dup {
			return
		}
		seen[v] = struct{}{}
		vals = append(vals, v)
	}
	var walk func(blocks []models.Block, switchHoldsCredential bool)
	walk = func(blocks []models.Block, switchHoldsCredential bool) {
		for i := range blocks {
			b := &blocks[i]
			// The Name pass: header literals and comment words (name.go).
			for _, sp := range nameSecretSpans(b, switchHoldsCredential) {
				add(b.Name[sp.start:sp.end])
			}
			// Action-specific fields (e.g. PopulateTextField.Text).
			if fields, ok := sensitiveActions[b.RawType]; ok {
				for _, f := range fields {
					if v, exists := b.Properties[f]; exists && len(v) > 0 {
						add(v)
					}
				}
			}
			// Credential-named keys on any action, and the entropy fallback
			// the document path applies to every property: an opaque token
			// under a key nobody listed (SET ServiceKey TO …, Note: …).
			for k, v := range b.Properties {
				if len(v) > 0 && (isSensitiveFieldName(k) || looksLikeHighEntropySecret(v)) {
					add(v)
				}
			}
			// SET <sensitive-name> TO <literal> injects _var/_value; mask the
			// literal when the target variable names a credential.
			if name, ok := b.Properties["_var"]; ok && isCredentialLikeName(name) {
				if v := b.Properties["_value"]; len(v) > 0 {
					add(v)
				}
			}
			walk(b.Children, b.Type == models.BlockTypeSwitch && switchHoldsCredentialVar(b))
		}
	}
	for _, sf := range doc.Subflows {
		walk(sf.Blocks, false)
	}
	return vals
}

// isPureVarRef reports whether v is a single %Variable% reference (possibly
// with surrounding whitespace) rather than a literal that could carry a
// secret.
func isPureVarRef(v string) bool {
	t := strings.TrimSpace(v)
	if !strings.HasPrefix(t, "%") || !strings.HasSuffix(t, "%") || len(t) < 2 {
		return false
	}
	return !strings.Contains(t[1:len(t)-1], "%")
}
