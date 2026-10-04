package scrubber

import (
	"maps"
	"math"
	"pad-core/models"
	"regexp"
	"strings"
	"unicode"
)

var secretRegexes = []*regexp.Regexp{
	// Generic key/value secrets: whitespace around the delimiter and YAML-style `key: value` too.
	regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)\s*[:=]\s*["']?([A-Za-z0-9\-._~+/=]{8,})["']?`),
	// Bearer tokens
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-._~+/]+=*`),
	// Specific well-known prefixes (Stripe, GitHub)
	regexp.MustCompile(`(?i)\b(sk_[a-zA-Z0-9_]{20,}|pk_[a-zA-Z0-9_]{20,}|ghp_[a-zA-Z0-9]{36}|glpat_[a-zA-Z0-9\-]{20,})\b`),
	// Connection-string secrets: Password=/PWD= (covers User=;Password= forms).
	// The value stops at ';' or end of line — running to end-of-text made prose
	// mentioning "password=…" swallow the entire rest of the message.
	regexp.MustCompile(`(?i)(?:Password|PWD)\s*=\s*([^;\r\n]+)`),
	// AWS access key IDs (AKIA = normal, ASIA = temporary/STS): canonical
	// 16 chars, word-bounded to avoid mid-word hits.
	regexp.MustCompile(`\b((?:AKIA|ASIA)[0-9A-Z]{16})\b`),
	// Google API key (AIza + 35 chars): Google Cloud API and Android/Maps keys.
	regexp.MustCompile(`\b(AIza[0-9A-Za-z_-]{35})\b`),
	// Slack tokens: xox[abprs]- + 10+ chars (bot, user, app, refresh, legacy).
	regexp.MustCompile(`\b(xox[abprs]-[A-Za-z0-9-]{10,})\b`),
	// JWT: eyJ (base64url of `{"`) on both segments, so false positives need a
	// coincidental eyJ match in two segments.
	regexp.MustCompile(`\b(eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\b`),
	// PEM private key blocks between BEGIN/END markers, spanning lines.
	regexp.MustCompile(`-----BEGIN (?:[A-Z ]+)PRIVATE KEY-----[\s\S]*?-----END (?:[A-Z ]+)PRIVATE KEY-----`),
}

// sensitiveActions maps PAD action RawTypes to property fields always masked — belt-and-suspenders over the passes below.
var sensitiveActions = map[string][]string{
	"WebAutomation.PopulateTextField": {"Text"},
	// FillWith is the typed value (the credential); Text is the locator and stays visible.
	"WebAutomation.FillTextByText":        {"FillWith"},
	"WebAutomation.FillTextByName":        {"FillWith"},
	"WebAutomation.FillTextByPlaceholder": {"FillWith"},
	"Database.Connect":                    {"ConnectionString"},
	"Database.OpenSQLConnection":          {"ConnectionString"},
	"Database.OpenODBCConnection":         {"ConnectionString"},
	"Excel.LaunchExcel":                   {"Password"},
	"Excel.LaunchAndOpen":                 {"Password"},
	"Cryptography.DecryptTextWithAES":     {"Passphrase", "Key"},
	"Cryptography.EncryptTextWithAES":     {"Passphrase", "Key"},
	"Cryptography.DecryptFromFileWithAES": {"Passphrase", "Key"},
	"Cryptography.EncryptToFileWithAES":   {"Passphrase", "Key"},
	"Cryptography.HashText":               {"Key"},
	"Cryptography.HashFromFile":           {"Key"},
	"Email.ConnectToExchange":             {"Password"},
	"Email.ConnectToIMAP":                 {"Password"},
	"Email.RetrieveEmailMessages":         {"Password"},
	"Outlook.LaunchOutlook":               {"Password"},
	"FTP.OpenConnection":                  {"Password"},
	"FTP.OpenSecureConnection":            {"Password"},
	"ActiveDirectory.Connect":             {"Password"},
	"Terminal.Open":                       {"Password"},
}

// sensitiveFieldNames: normalized keys masked on ANY action — catches
// credentials in the long tail of PAD's 200+ action types.
var sensitiveFieldNames = map[string]struct{}{
	"password": {}, "passwd": {}, "pwd": {}, "passphrase": {},
	"secret": {}, "secretkey": {}, "clientsecret": {},
	"apikey": {}, "accesskey": {}, "privatekey": {},
	"token": {}, "accesstoken": {}, "refreshtoken": {}, "bearertoken": {},
	"connectionstring": {}, "credential": {}, "credentials": {},
}

// normalizeFieldName lowercases a key and strips separators so "Api_Key", "api-key" and "ApiKey" collapse to "apikey".
func normalizeFieldName(k string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isSensitiveFieldName(k string) bool {
	_, ok := sensitiveFieldNames[normalizeFieldName(k)]
	return ok
}

// ScrubDocument returns a deep copy of the FlowDocument with secrets masked.
// It clones only what scrubBlock mutates (Properties maps and Children trees)
// — the json round-trip serialized the entire flow twice on the AI hot path.
func ScrubDocument(doc *models.FlowDocument) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, nil
	}

	copyDoc := *doc // shallow copy of top-level scalar/metadata fields
	copyDoc.Subflows = make([]models.Subflow, len(doc.Subflows))
	for i := range doc.Subflows {
		sf := doc.Subflows[i] // copies Subflow scalar fields + slice headers
		sf.Blocks = cloneBlocks(doc.Subflows[i].Blocks)
		copyDoc.Subflows[i] = sf
	}

	for i := range copyDoc.Subflows {
		sf := &copyDoc.Subflows[i]
		for j := range sf.Blocks {
			scrubBlock(&sf.Blocks[j])
		}
	}

	// The transient lookup maps copied above still reference the original
	// blocks; rebuild them against the cloned tree.
	copyDoc.BlocksByID = nil
	copyDoc.BlockSubflow = nil
	copyDoc.SubflowsByID = nil
	copyDoc.RebuildIndexes()

	return &copyDoc, nil
}

// cloneBlocks deep-copies a block tree (Properties maps, Children) so masking never mutates the caller's document.
func cloneBlocks(src []models.Block) []models.Block {
	if src == nil {
		return nil
	}
	out := make([]models.Block, len(src))
	for i := range src {
		b := src[i] // copies scalar fields + slice headers (Variables, Tokens)
		if src[i].Properties != nil {
			props := make(map[string]string, len(src[i].Properties))
			maps.Copy(props, src[i].Properties)
			b.Properties = props
		}
		b.Children = cloneBlocks(src[i].Children)
		b.ChildPtrs = nil // transient (json:"-"); not used by the scrubbed copy
		out[i] = b
	}
	return out
}

func scrubBlock(b *models.Block) {
	// 1. AST-aware masking on specific action properties (RawType is the PAD action name).
	if fieldsToMask, ok := sensitiveActions[b.RawType]; ok {
		for _, field := range fieldsToMask {
			if val, exists := b.Properties[field]; exists {
				if len(val) > 0 {
					b.Properties[field] = "[REDACTED]"
				}
			}
		}
	}

	// 2. Field-name masking: any property whose KEY names a credential (the long tail sensitiveActions misses).
	for k := range b.Properties {
		if b.Properties[k] == "[REDACTED]" {
			continue
		}
		if isSensitiveFieldName(k) && len(b.Properties[k]) > 0 {
			b.Properties[k] = "[REDACTED]"
		}
	}

	// 2b. SET <name> TO <literal> injects synthetic _var/_value keys the key
	//     pass cannot see: mask the literal when the target names a credential.
	if name, ok := b.Properties["_var"]; ok && isSensitiveFieldName(name) {
		if v := b.Properties["_value"]; len(v) > 0 {
			b.Properties["_value"] = "[REDACTED]"
		}
	}

	// 3. Generic regex masking on all properties, plus a high-entropy fallback
	//    for opaque tokens matching no known pattern.
	for k, v := range b.Properties {
		if b.Properties[k] == "[REDACTED]" {
			continue
		}
		scrubbed := ScrubText(v)
		if scrubbed == v && looksLikeHighEntropySecret(v) {
			scrubbed = "[REDACTED]"
		}
		b.Properties[k] = scrubbed
	}

	for i := range b.Children {
		scrubBlock(&b.Children[i])
	}
}

// looksLikeHighEntropySecret reports whether a standalone value resembles an
// opaque secret (API key, token, hash) rather than human-readable text, which
// stays intact for the AI to analyse.
func looksLikeHighEntropySecret(v string) bool {
	if len(v) < 20 || strings.ContainsAny(v, " \t\n\r") {
		return false
	}
	// URLs are long, space-free, often high-entropy but not secrets (e.g. a
	// WebAutomation "Url" property); embedded credentials are still caught by
	// the regex pass.
	if strings.Contains(v, "://") {
		return false
	}
	// Paths likewise regularly clear the entropy bar (~4.2+); masking them
	// degrades file/folder analysis, and embedded credentials are still
	// caught by the regex pass.
	if looksLikePath(v) {
		return false
	}
	// Length-tiered thresholds: longer opaque strings are almost certainly
	// not prose and get a lower bar (64-char hex ~3.7); shorter values keep
	// the stricter bar (boundary cases in scrubber_test.go).
	threshold := 4.0
	if len(v) > 50 {
		threshold = 3.5
	}
	return shannonEntropy(v) > threshold
}

// looksLikePath reports whether v resembles a filesystem path: POSIX root (/),
// home shortcut (~/), or Windows drive root — mid-string separators alone
// don't qualify, since base64 tokens contain '/'.
func looksLikePath(v string) bool {
	if strings.HasPrefix(v, `/`) || strings.HasPrefix(v, `~/`) {
		return true
	}
	// Windows drive root: "<letter>:\" or "<letter>:/". v[1] of a non-ASCII
	// lead byte is a continuation byte, never ':' — UTF-8 safe.
	if len(v) >= 3 && v[1] == ':' && (v[2] == '\\' || v[2] == '/') {
		return true
	}
	return false
}

func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := make(map[rune]int)
	for _, r := range s {
		freq[r]++
	}
	var entropy float64
	total := float64(len(s))
	for _, count := range freq {
		p := float64(count) / total
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// mayContainSecret is the prefilter for ScrubText: every secretRegexes match
// contains an anchorLiteral case-insensitively, so text with none cannot
// match — one folding pass plus substring scans replaces nine regex scans
// (~35× faster on clean prose). U+017F (ſ) and U+212A fold to 's'/'k' under
// the regexes' (?i) but not plain lower-casing, so folded explicitly; the
// prefilter stays a strict superset of the regexes' lead-ins.
func mayContainSecret(text string) bool {
	lower := strings.Map(func(r rune) rune {
		switch r {
		case '\u017F':
			return 's'
		case '\u212A':
			return 'k'
		}
		return unicode.ToLower(r)
	}, text)
	for _, a := range anchorLiterals {
		if strings.Contains(lower, a) {
			return true
		}
	}
	return false
}

// ScrubText applies regex patterns to replace found secrets in raw text
func ScrubText(text string) string {
	if !mayContainSecret(text) {
		return text
	}
	result := text
	for _, pat := range secretRegexes {
		// MatchString first: cheaper than the replace machinery, and the common case (prose) has no match.
		if !pat.MatchString(result) {
			continue
		}
		result = pat.ReplaceAllStringFunc(result, func(match string) string {
			return maskMatch(pat, match)
		})
	}
	return result
}

// HasCompleteSecret reports whether b already contains a finished secret match — lets the stream scrubber decide redaction coverage early without weakening detection.
func HasCompleteSecret(b []byte) bool {
	for _, pat := range secretRegexes {
		if pat.Match(b) {
			return true
		}
	}
	return false
}

func maskMatch(pat *regexp.Regexp, match string) string {
	// With a capture group, mask only the group; otherwise the whole string.
	submatches := pat.FindStringSubmatchIndex(match)
	if len(submatches) > 2 { // Has a capture group
		// submatches[2]/[3] are the first capture group's start/end.
		start := submatches[2]
		end := submatches[3]
		return match[:start] + "[REDACTED]" + match[end:]
	}
	return "[REDACTED]"
}
