package scrubber

import (
	"maps"
	"math"
	"pad-core/models"
	"regexp"
	"strings"
)

var secretRegexes = []*regexp.Regexp{
	// Generic key/value secrets. Allows whitespace around the delimiter and a
	// YAML-style `key: value` form (not just a bare `key=value`/`key:value`).
	regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)\s*[:=]\s*["']?([A-Za-z0-9\-._~+/=]{8,})["']?`),
	// Bearer tokens
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-._~+/]+=*`),
	// Specific well-known prefixes (e.g. Stripe, GitHub)
	regexp.MustCompile(`(?i)\b(sk_[a-zA-Z0-9_]{20,}|pk_[a-zA-Z0-9_]{20,}|ghp_[a-zA-Z0-9]{36}|glpat_[a-zA-Z0-9\-]{20,})\b`),
	// Connection-string secrets: Password=/PWD= (covers User=;Password= forms).
	// The value stops at ';' (connection-string delimiter) or end of line —
	// letting it run to end-of-text made any prose mentioning "password=…"
	// swallow the entire rest of the message into the mask.
	regexp.MustCompile(`(?i)(?:Password|PWD)\s*=\s*([^;\r\n]+)`),
}

// sensitiveActions maps a PAD action RawType to specific property fields whose
// values must always be masked, regardless of their content. This is a belt-and-
// suspenders layer on top of the field-name and regex/entropy passes below.
var sensitiveActions = map[string][]string{
	"WebAutomation.PopulateTextField":     {"Text"},
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

// sensitiveFieldNames is the set of property keys (normalized: lowercased with
// separators stripped) whose value is treated as a credential and masked on ANY
// action. This catches credentials in actions not enumerated in sensitiveActions
// — the dominant real-world leak path, since PAD has 200+ action types.
var sensitiveFieldNames = map[string]struct{}{
	"password": {}, "passwd": {}, "pwd": {}, "passphrase": {},
	"secret": {}, "secretkey": {}, "clientsecret": {},
	"apikey": {}, "accesskey": {}, "privatekey": {},
	"token": {}, "accesstoken": {}, "refreshtoken": {}, "bearertoken": {},
	"connectionstring": {}, "credential": {}, "credentials": {},
}

// normalizeFieldName lowercases a property key and strips common separators so
// "Api_Key", "api-key" and "ApiKey" all collapse to "apikey".
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
//
// It clones only what scrubBlock mutates — each block's Properties map and the
// Children tree — plus the slices that structurally contain them. This avoids
// the previous full json.Marshal→json.Unmarshal round-trip, which serialized the
// entire flow (potentially 10k+ blocks) twice on the AI hot path. Read-only
// fields (Variables, Tokens, metadata) are shallow-copied since the scrubber
// never mutates them. The error return is retained for API compatibility.
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

	// The transient lookup maps copied above still reference the original blocks;
	// drop and rebuild them against the cloned tree (mirrors the prior behaviour
	// where the maps were absent after unmarshal and rebuilt here).
	copyDoc.BlocksByID = nil
	copyDoc.BlockSubflow = nil
	copyDoc.SubflowsByID = nil
	copyDoc.RebuildIndexes()

	return &copyDoc, nil
}

// cloneBlocks deep-copies a block tree, duplicating each block's Properties map
// and Children slice recursively so masking never mutates the caller's document.
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
	// 1. AST-Aware masking based on specific action and properties
	// b.RawType is the actual PAD action name like "WebAutomation.PopulateTextField"
	if fieldsToMask, ok := sensitiveActions[b.RawType]; ok {
		for _, field := range fieldsToMask {
			if val, exists := b.Properties[field]; exists {
				if len(val) > 0 {
					b.Properties[field] = "[REDACTED]"
				}
			}
		}
	}

	// 2. Field-name masking: any property whose KEY names a credential is masked
	//    outright, regardless of action. Catches secrets in the long tail of
	//    actions not enumerated in sensitiveActions.
	for k := range b.Properties {
		if b.Properties[k] == "[REDACTED]" {
			continue
		}
		if isSensitiveFieldName(k) && len(b.Properties[k]) > 0 {
			b.Properties[k] = "[REDACTED]"
		}
	}

	// 3. Generic regex masking on all properties, plus a high-entropy fallback
	//    for opaque tokens/keys that don't match a known pattern (restores the
	//    entropy-based catch-all that the prior context.go masking provided).
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

	// Recurse
	for i := range b.Children {
		scrubBlock(&b.Children[i])
	}
}

// looksLikeHighEntropySecret reports whether a standalone value resembles an
// opaque secret (API key, token, hash) rather than human-readable text. It
// targets long, whitespace-free, high-entropy strings so prose and short values
// are left intact for the AI to analyse.
func looksLikeHighEntropySecret(v string) bool {
	if len(v) < 20 || strings.ContainsAny(v, " \t\n\r") {
		return false
	}
	// URLs/URIs are long, space-free, and often high-entropy but are not secrets
	// (e.g. a WebAutomation "Url" property); keep them for the AI to analyse.
	// Any embedded credential in the URL is still caught by the regex pass.
	if strings.Contains(v, "://") {
		return false
	}
	// Filesystem paths are likewise long, space-free, and regularly clear the
	// entropy bar (Windows/POSIX paths entropy ~4.2+). Masking them degrades the
	// AI's analysis of file/folder actions; a credential embedded in such a
	// value is still caught by the regex pass above.
	if looksLikePath(v) {
		return false
	}
	// Length-tiered thresholds (S3): a longer opaque string is almost certainly
	// not prose, so it is treated as a secret at a lower entropy bar — a 64-char
	// hex token entropies ~3.7, below the 4.0 the short tier requires but clearly
	// not human-readable. Shorter values keep the stricter bar to avoid masking
	// identifiers. See scrubber_test.go for the measured boundary cases.
	threshold := 4.0
	if len(v) > 50 {
		threshold = 3.5
	}
	return shannonEntropy(v) > threshold
}

// looksLikePath reports whether v resembles a filesystem path rather than an
// opaque token: a POSIX root (/), a home shortcut (~/), or a Windows drive root
// (e.g. C:\). Mid-string separators alone don't qualify, since standard base64
// tokens legitimately contain '/'.
func looksLikePath(v string) bool {
	if strings.HasPrefix(v, `/`) || strings.HasPrefix(v, `~/`) {
		return true
	}
	// Windows drive root: "<letter>:\" or "<letter>:/". v[1] is a continuation
	// byte for a non-ASCII lead byte, which never equals ':' — safe for UTF-8.
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

// ScrubText applies regex patterns to replace found secrets in raw text
func ScrubText(text string) string {
	result := text
	for _, pat := range secretRegexes {
		// MatchString first: it is cheaper than the replace machinery and the
		// overwhelmingly common case (chat output, prose context) has no match.
		if !pat.MatchString(result) {
			continue
		}
		result = pat.ReplaceAllStringFunc(result, func(match string) string {
			return maskMatch(pat, match)
		})
	}
	return result
}

func maskMatch(pat *regexp.Regexp, match string) string {
	// If the regex has a submatch (capture group), we only mask the capture group.
	// Otherwise, we mask the whole string.
	submatches := pat.FindStringSubmatchIndex(match)
	if len(submatches) > 2 { // Has a capture group
		// submatches[2] and [3] are the start/end of the first capture group
		start := submatches[2]
		end := submatches[3]
		return match[:start] + "[REDACTED]" + match[end:]
	}
	return "[REDACTED]"
}
