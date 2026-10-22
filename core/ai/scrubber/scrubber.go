package scrubber

import (
	"maps"
	"math"
	"pad-core/models"
	"regexp"
	"strings"
	"unicode"
)

// The secret patterns are the secretClasses table in classes.go (secretRegexes is derived there).

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
	// WebAutomation "Url" property). Credentials embedded in one — the userinfo
	// password and credential query parameters (sig=, code=, X-Amz-Signature=,
	// …) — are masked by the url-userinfo and url-credential-param classes of
	// the regex pass that runs first.
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

// presentClasses is the prefilter for ScrubText: every match of a class's
// pattern contains one of its anchors case-insensitively, so text with none of
// them cannot match the class, and text with no anchor at all is clean — one
// folding pass plus substring scans in place of a regex scan per class (~35×
// faster on clean prose), and a streamed chunk that merely mentions a keyword
// runs the keyword classes alone instead of all nineteen. U+017F (ſ) and
// U+212A fold to 's'/'k' under the regexes' (?i) but not plain lower-casing,
// so folded explicitly; the prefilter stays a strict superset of the regexes'
// lead-ins.
func presentClasses(text string) classMask {
	lower := strings.Map(func(r rune) rune {
		switch r {
		case '\u017F':
			return 's'
		case '\u212A':
			return 'k'
		}
		return unicode.ToLower(r)
	}, text)
	var present classMask
	for i, a := range anchorLiterals {
		// A literal whose classes are all in already (passwd after password) is not worth the scan.
		if present&anchorClassMasks[i] != anchorClassMasks[i] && strings.Contains(lower, a) {
			present |= anchorClassMasks[i]
		}
	}
	return present
}

// redacted is the mask ScrubText writes in place of a secret.
const redacted = "[REDACTED]"

// ScrubText applies regex patterns to replace found secrets in raw text
func ScrubText(text string) string {
	present := presentClasses(text)
	if present == 0 {
		return text
	}
	result := text
	var lead *regexp.Regexp
	leadFound := false
	for i := range secretClasses {
		// Masking only ever replaces text with "[REDACTED]", which holds no anchor, so a class absent from
		// the input is absent from every intermediate result too; the same keeps a shared lead's verdict
		// good for the classes that follow.
		if present&(1<<i) == 0 {
			continue
		}
		c := &secretClasses[i]
		if c.lead != nil {
			if c.lead != lead {
				lead, leadFound = c.lead, c.lead.MatchString(result)
			}
			if !leadFound {
				continue
			}
		}
		// MatchString first: cheaper than the replace machinery, and the common case (prose) has no match.
		if !c.pattern.MatchString(result) {
			continue
		}
		result = c.pattern.ReplaceAllStringFunc(result, func(match string) string {
			value := capturedValue(c.pattern, match)
			// A value an earlier class already masked is left alone: a wider pattern would otherwise take
			// "[REDACTED]" plus what follows it as its value ("?key=[REDACTED]&page=2" losing "&page=2").
			if strings.HasPrefix(value, redacted) {
				return match
			}
			if c.accept != nil && !c.accept(value) {
				return match
			}
			return maskMatch(c.pattern, match)
		})
	}
	return result
}

// HasCompleteSecret reports whether b already contains a finished secret match — lets the stream scrubber decide redaction coverage early without weakening detection.
func HasCompleteSecret(b []byte) bool {
	for i := range secretClasses {
		c := &secretClasses[i]
		if c.accept == nil {
			if c.pattern.Match(b) {
				return true
			}
			continue
		}
		// A class with accept matches only when a captured value passes it: a rejected match is not a
		// secret, and over-reporting one would arm the stream's drain on ordinary text.
		for _, m := range c.pattern.FindAllSubmatchIndex(b, -1) {
			if len(m) > 3 && m[2] >= 0 && c.accept(string(b[m[2]:m[3]])) {
				return true
			}
		}
	}
	return false
}

// capturedValue returns the first capture group of pattern in match, or match itself when there is none.
func capturedValue(pattern *regexp.Regexp, match string) string {
	m := pattern.FindStringSubmatchIndex(match)
	if len(m) > 3 && m[2] >= 0 {
		return match[m[2]:m[3]]
	}
	return match
}

func maskMatch(pat *regexp.Regexp, match string) string {
	// With a capture group, mask only the group; otherwise the whole string.
	submatches := pat.FindStringSubmatchIndex(match)
	if len(submatches) > 3 && submatches[2] >= 0 { // Has a capture group that took part in the match
		// submatches[2]/[3] are the first capture group's start/end.
		start := submatches[2]
		end := submatches[3]
		return match[:start] + "[REDACTED]" + match[end:]
	}
	return "[REDACTED]"
}
