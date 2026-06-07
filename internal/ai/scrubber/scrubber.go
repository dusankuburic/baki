package scrubber

import (
	"encoding/json"
	"math"
	"pad-analyzer/internal/models"
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
	regexp.MustCompile(`(?i)(?:Password|PWD)\s*=\s*([^;]+)`),
}

// sensitiveActions maps a PAD action RawType to specific property fields whose
// values must always be masked, regardless of their content. This is a belt-and-
// suspenders layer on top of the field-name and regex/entropy passes below.
var sensitiveActions = map[string][]string{
	"WebAutomation.PopulateTextField": {"Text"},
	"Database.Connect":                {"ConnectionString"},
	"Database.OpenSQLConnection":      {"ConnectionString"},
	"Database.OpenODBCConnection":     {"ConnectionString"},
	"Excel.LaunchExcel":               {"Password"},
	"Excel.LaunchAndOpen":             {"Password"},
	"Cryptography.DecryptTextWithAES": {"Passphrase", "Key"},
	"Cryptography.EncryptTextWithAES": {"Passphrase", "Key"},
	"Cryptography.DecryptFromFileWithAES": {"Passphrase", "Key"},
	"Cryptography.EncryptToFileWithAES":   {"Passphrase", "Key"},
	"Cryptography.HashText":               {"Key"},
	"Cryptography.HashFromFile":           {"Key"},
	"Email.ConnectToExchange":  {"Password"},
	"Email.ConnectToIMAP":      {"Password"},
	"Email.RetrieveEmailMessages": {"Password"},
	"Outlook.LaunchOutlook":    {"Password"},
	"FTP.OpenConnection":       {"Password"},
	"FTP.OpenSecureConnection": {"Password"},
	"ActiveDirectory.Connect":  {"Password"},
	"Terminal.Open":            {"Password"},
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
func ScrubDocument(doc *models.FlowDocument) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, nil
	}

	// Deep copy via JSON
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var copyDoc models.FlowDocument
	if err := json.Unmarshal(data, &copyDoc); err != nil {
		return nil, err
	}

	for i := range copyDoc.Subflows {
		sf := &copyDoc.Subflows[i]
		for j := range sf.Blocks {
			scrubBlock(&sf.Blocks[j])
		}
	}

	// Rebuild indexes since we just unmarshaled
	copyDoc.RebuildIndexes()

	return &copyDoc, nil
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
	return shannonEntropy(v) > 4.0
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
