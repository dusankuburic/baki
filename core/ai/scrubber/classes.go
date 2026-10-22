package scrubber

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"strings"
	"unicode/utf8"
)

// secretClass is one kind of secret: the pattern ScrubText masks, and everything the stream scrubber needs to
// hold that secret back while it arrives in pieces. secretRegexes, anchorLiterals, viablePrefixRegexes and
// terminalStates are all derived from secretClasses — they used to be five lists kept in step by hand, and
// a pattern whose lead-in was missing from the anchors streamed out in clear.
type secretClass struct {
	name string
	// pattern is the ScrubText regex. A match must BEGIN at one of anchors (case-insensitively): the stream
	// scrubber holds text from an anchor on, so a pattern that started before its anchor would be split
	// across two ScrubText calls and never match. With a capture group only the group is masked.
	pattern *regexp.Regexp
	// accept, when set, vets a match's captured value; a match it rejects is left as it is.
	accept func(value string) bool
	// lead, when set, is a regex every match of pattern begins with: ScrubText skips the class while the
	// text has no lead, and consecutive classes sharing one pay for it once. Optional.
	lead *regexp.Regexp
	// anchors are the lower-case literals a match can begin with: the prefilter and the stream's anchoring.
	anchors []string
	// viable matches every prefix of a pattern match that begins at an anchor, anchored to the end of the
	// held text: the moment a held tail stops matching, the anchor is dead and its text is released.
	viable *regexp.Regexp
	// terminals map a held tail onto the byte class of its trailing run (the stream's fast path); optional.
	terminals []terminalState
	// sampleEachAnchor marks a class whose anchors are distinct token prefixes: each needs its own
	// secretSamples entry (TestSecretClasses_Structure).
	sampleEachAnchor bool
}

// terminalState maps a viable-held tail onto the byte class of its trailing character run: extending the
// hold becomes an O(chunk) byte scan instead of re-running the viability regexes. A byte outside the class
// falls back to the full rescan, which re-derives the state.
type terminalState struct {
	re    *regexp.Regexp
	class *byteClass
}

// credentialKeywords are the key names whose value is a credential in key/value text (config, JSON, YAML,
// connection strings, headers), as a regex alternation and as the literal spellings that anchor it.
// functions-key is the x-functions-key header of Azure Functions (the ?code= form is a URL parameter).
const credentialKeywords = `password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|account[_-]?key|subscription[_-]?key|functions[_-]?key`

var credentialKeywordAnchors = keywordAnchors()

// keywordLead is the lead-in the four keyword classes share: a credential keyword, its closing quote, then
// the delimiter. A streamed chunk that merely mentions a keyword ("the password field") has none, and would
// otherwise pay four (?i) alternation scans for nothing.
var keywordLead = regexp.MustCompile(`(?i)(?:` + credentialKeywords + `)["']?\s*[:=]`)

func keywordAnchors() []string {
	out := []string{"password", "passwd", "pwd", "secret", "token"}
	for _, first := range []string{"api", "access", "private", "account", "subscription", "functions"} {
		// The two-word names as they are written: joined, snake_case and kebab-case.
		out = append(out, first+"key", first+"_key", first+"-key")
	}
	return out
}

var (
	// tokenValueClass is a token body that carries both '_' and '-' (sk-…, glpat-…).
	tokenValueClass = makeClass(func(b byte) bool { return isAlnum(b) || b == '_' || b == '-' })
	// symbolValueClass is a key-value-symbols value: anything up to whitespace, a quote, ';' or ','.
	symbolValueClass = makeClass(func(b byte) bool { return !isSpace(b) && strings.IndexByte(`"'<>;,`, b) < 0 })
	// urlParamValueClass is a query-parameter value: up to the next parameter, fragment or segment.
	urlParamValueClass = makeClass(func(b byte) bool { return !isSpace(b) && strings.IndexByte(`&#;"'<>`, b) < 0 })
	// basicValueClass is unpadded standard base64; '=' padding falls back to the rescan, as for Bearer.
	basicValueClass = makeClass(func(b byte) bool { return isAlnum(b) || b == '+' || b == '/' })
	// jwtValueClass is base64url plus the '.' separators.
	jwtValueClass = makeClass(func(b byte) bool { return isAlnum(b) || strings.IndexByte(`_.-`, b) >= 0 })
)

// isSpace mirrors the regexes' \s for a byte.
func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

// urlCredentialParams are query parameters whose value is a credential: SAS signatures, Azure Functions
// keys, API and subscription keys, OAuth tokens and secrets, AWS SigV4 signatures and session tokens.
var urlCredentialParams = []string{
	"sig", "signature", "code", "key", "apikey", "api_key", "subscription-key",
	"access_token", "client_secret", "password", "pwd", "token",
	"x-amz-signature", "x-amz-credential", "x-amz-security-token",
}

// urlParamAnchors are the literals a credential query parameter begins with: "?name=" and "&name=".
func urlParamAnchors() []string {
	out := make([]string, 0, 2*len(urlCredentialParams))
	for _, p := range urlCredentialParams {
		out = append(out, "?"+p+"=", "&"+p+"=")
	}
	return out
}

// urlParamNames is the regex alternation of urlCredentialParams.
var urlParamNames = urlParamAlternation()

func urlParamAlternation() string {
	quoted := make([]string, len(urlCredentialParams))
	for i, p := range urlCredentialParams {
		quoted[i] = regexp.QuoteMeta(p)
	}
	return strings.Join(quoted, "|")
}

// prefixChain returns a regex matching any prefix of word, with then allowed only once word is complete:
// viability must accept every prefix of a match, including one cut inside a fixed word.
func prefixChain(word, then string) string {
	var b strings.Builder
	for _, r := range word {
		b.WriteString("(?:")
		b.WriteString(regexp.QuoteMeta(string(r)))
	}
	b.WriteString(then)
	for range word {
		b.WriteString(")?")
	}
	return b.String()
}

// looksLikeSymbolCredential vets a key-value-symbols match: the values the generic pattern cannot see
// (Winter#2024, P@ss!) are credentials; ones it can see are left to it. Prose after a keyword is not one: a
// value opening with a symbol is a PAD variable reference (%Password%), a template (${env:TOKEN}), Markdown
// (`…`, **…**) or a parenthesis, a value carrying "://" is a URL, and a non-ASCII letter is a letter.
func looksLikeSymbolCredential(v string) bool {
	if v == "" || !isAlnum(v[0]) || strings.Contains(v, "://") {
		return false
	}
	for i := 0; i < len(v); i++ {
		if b := v[i]; b < utf8.RuneSelf && !genericValueClass[b] {
			return true
		}
	}
	return false
}

// isLiteral vets a quoted value: PAD's %Name% and a ${NAME} template name a secret without carrying one.
func isLiteral(v string) bool {
	if len(v) < 3 {
		return true
	}
	padRef := strings.HasPrefix(v, "%") && strings.HasSuffix(v, "%") && !strings.Contains(v[1:len(v)-1], "%")
	template := strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") && !strings.Contains(v[2:len(v)-1], "}")
	return !padRef && !template
}

// isBasicCredentials vets a basic-auth match: the value is base64 for "user:password", so a word of prose
// after "Basic" ("Basic authentication") is not one. The stream cap vets the held prefix of a value still
// arriving, so a prefix cut mid-quantum drops its dangling character instead of failing to decode.
func isBasicCredentials(v string) bool {
	v = strings.TrimRight(v, "=")
	if len(v)%4 == 1 {
		v = v[:len(v)-1]
	}
	plain, err := base64.RawStdEncoding.DecodeString(v)
	return err == nil && bytes.IndexByte(plain, ':') >= 0
}

// secretClasses is the order ScrubText applies the patterns in. The URL classes come first so a percent-
// encoded value is masked whole, and the quoted-value classes before the generic one so a value with spaces
// or symbols is not cut at its first non-base64 byte.
var secretClasses = []secretClass{
	{
		name: "url-userinfo",
		// scheme://user:password@host — only the password is masked; scheme, user and host stay readable.
		// The password stops at '?' and '#' (RFC 3986 gen-delims): "host:port?hint=user@domain" is not userinfo.
		pattern: regexp.MustCompile(`://[^\s/:@]+:([^\s/?#@]+)@`),
		anchors: []string{"://"},
		viable:  regexp.MustCompile(`^://[^\s/:@]*(?::[^\s/?#@]*@?)?$`),
	},
	{
		name:    "url-credential-param",
		pattern: regexp.MustCompile(`(?i)[?&](?:` + urlParamNames + `)=([^&#;\s"'<>]+)`),
		anchors: urlParamAnchors(),
		// Viable to the end of the query, not of the value: a hold that died at the next '&' released the
		// "?name=" lead-in and re-anchored on the keyword inside the name (token in access_token), whose
		// key/value classes then masked the rest of the query. Over-holding is bounded by the cap.
		viable: regexp.MustCompile(`(?i)^[?&](?:` + urlParamNames + `)=[^\s"'<>]*$`),
		// The trailing run is the value of the last credential parameter, which may follow other parameters.
		terminals: []terminalState{{regexp.MustCompile(`(?i)^[?&](?:` + urlParamNames + `)=(?:[^\s"'<>]*[?&](?:` + urlParamNames + `)=)?[^&#;\s"'<>]*$`), urlParamValueClass}},
	},
	{
		name: "quoted-double",
		// "password": "…" and password: "…" — the value runs to its closing quote, spaces and symbols
		// included; escapes count.
		pattern: regexp.MustCompile(`(?i)(?:` + credentialKeywords + `)"?\s*[:=]\s*"((?:\\.|[^"\\\r\n]){3,})"`),
		accept:  isLiteral,
		lead:    keywordLead,
		anchors: credentialKeywordAnchors,
		viable:  regexp.MustCompile(`(?i)^(?:` + credentialKeywords + `)"?\s*(?:[:=]\s*(?:"(?:\\.|[^"\\\r\n])*(?:\\|")?)?)?$`),
	},
	{
		name:    "quoted-single",
		pattern: regexp.MustCompile(`(?i)(?:` + credentialKeywords + `)'?\s*[:=]\s*'((?:\\.|[^'\\\r\n]){3,})'`),
		accept:  isLiteral,
		lead:    keywordLead,
		anchors: credentialKeywordAnchors,
		viable:  regexp.MustCompile(`(?i)^(?:` + credentialKeywords + `)'?\s*(?:[:=]\s*(?:'(?:\\.|[^'\\\r\n])*(?:\\|')?)?)?$`),
	},
	{
		name: "key-value-symbols",
		// password: Winter#2024 — a value with a symbol the generic class stops at; runs to whitespace,
		// a quote, ';' or ','. accept leaves symbol-free values (prose words, base64) to the generic class.
		pattern:   regexp.MustCompile(`(?i)(?:` + credentialKeywords + `)["']?\s*[:=]\s*["']?([^\s"'<>;,]{6,})`),
		accept:    looksLikeSymbolCredential,
		lead:      keywordLead,
		anchors:   credentialKeywordAnchors,
		viable:    regexp.MustCompile(`(?i)^(?:` + credentialKeywords + `)["']?\s*(?:[:=]\s*["']?[^\s"'<>;,]*)?$`),
		terminals: []terminalState{{regexp.MustCompile(`(?i)^(?:` + credentialKeywords + `)["']?\s*[:=]\s*["']?[^\s"'<>;,]*$`), symbolValueClass}},
	},
	{
		name: "key-value",
		// Generic key/value secrets: whitespace around the delimiter, YAML-style `key: value`, and a quote
		// closing a JSON key before the colon.
		pattern: regexp.MustCompile(`(?i)(?:` + credentialKeywords + `)["']?\s*[:=]\s*["']?([A-Za-z0-9\-._~+/=]{8,})["']?`),
		lead:    keywordLead,
		anchors: credentialKeywordAnchors,
		viable:  regexp.MustCompile(`(?i)^(?:` + credentialKeywords + `)["']?\s*(?:[:=]\s*["']?[A-Za-z0-9\-._~+/=]*["']?)?$`),
		terminals: []terminalState{
			// The value still open (no closing quote yet).
			{regexp.MustCompile(`(?i)^(?:` + credentialKeywords + `)["']?\s*[:=]\s*["']?[A-Za-z0-9\-._~+/=]*$`), genericValueClass},
		},
	},
	{
		name:      "basic-auth",
		pattern:   regexp.MustCompile(`(?i)authorization\s*:\s*basic\s+([A-Za-z0-9+/]{4,}={0,2})`),
		accept:    isBasicCredentials,
		anchors:   []string{"authorization"},
		viable:    regexp.MustCompile(`(?i)^authorization\s*(?::\s*` + prefixChain("basic", `(?:\s+[A-Za-z0-9+/]*=*)?`) + `)?$`),
		terminals: []terminalState{{regexp.MustCompile(`(?i)^authorization\s*:\s*basic\s+[A-Za-z0-9+/]*$`), basicValueClass}},
	},
	{
		name:    "bearer",
		pattern: regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-._~+/]+=*`),
		anchors: []string{"bearer"},
		viable:  regexp.MustCompile(`(?i)^Bearer(?:\s+[A-Za-z0-9\-._~+/]*=*)?$`),
		// '=' padding is rare and handled by the rescan fallback.
		terminals: []terminalState{{regexp.MustCompile(`(?i)^Bearer\s+[A-Za-z0-9\-._~+/]*$`), bearerValueClass}},
	},
	{
		name: "stripe",
		// Secret, publishable and restricted keys, and webhook signing secrets.
		pattern:          regexp.MustCompile(`(?i)\b((?:sk|pk|rk)_[a-zA-Z0-9_]{20,}|whsec_[a-zA-Z0-9]{20,})\b`),
		anchors:          []string{"sk_", "pk_", "rk_", "whsec_"},
		viable:           regexp.MustCompile(`(?i)^(?:(?:sk|pk|rk)_[a-zA-Z0-9_]*|whsec_[a-zA-Z0-9]*)$`),
		terminals:        []terminalState{{regexp.MustCompile(`(?i)^(?:sk|pk|rk)_[a-zA-Z0-9_]*$`), skpkClass}, {regexp.MustCompile(`(?i)^whsec_[a-zA-Z0-9]*$`), ghpClass}},
		sampleEachAnchor: true,
	},
	{
		name: "github",
		// Classic tokens (ghp_ personal, gho_ OAuth, ghu_ user-to-server, ghs_ server-to-server, ghr_ refresh)
		// and fine-grained personal access tokens.
		pattern:          regexp.MustCompile(`(?i)\b(gh[pousr]_[a-zA-Z0-9]{36}|github_pat_[a-zA-Z0-9_]{22,})\b`),
		anchors:          []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"},
		viable:           regexp.MustCompile(`(?i)^(?:gh[pousr]_[a-zA-Z0-9]*|github_pat_[a-zA-Z0-9_]*)$`),
		terminals:        []terminalState{{regexp.MustCompile(`(?i)^gh[pousr]_[a-zA-Z0-9]*$`), ghpClass}, {regexp.MustCompile(`(?i)^github_pat_[a-zA-Z0-9_]*$`), skpkClass}},
		sampleEachAnchor: true,
	},
	{
		name: "gitlab",
		// Real personal access tokens are glpat-…; the underscore form is kept for the tokens it already caught.
		pattern:          regexp.MustCompile(`(?i)\b(glpat[-_][a-zA-Z0-9_\-]{20,})\b`),
		anchors:          []string{"glpat-", "glpat_"},
		viable:           regexp.MustCompile(`(?i)^glpat[-_][a-zA-Z0-9_\-]*$`),
		terminals:        []terminalState{{regexp.MustCompile(`(?i)^glpat[-_][a-zA-Z0-9_\-]*$`), tokenValueClass}},
		sampleEachAnchor: true,
	},
	{
		name: "sk-dash",
		// OpenAI (sk-…, sk-proj-…) and Anthropic (sk-ant-…) API keys.
		pattern:   regexp.MustCompile(`\b(sk-[A-Za-z0-9_\-]{20,})\b`),
		anchors:   []string{"sk-"},
		viable:    regexp.MustCompile(`^sk-[A-Za-z0-9_\-]*$`),
		terminals: []terminalState{{regexp.MustCompile(`^sk-[A-Za-z0-9_\-]*$`), tokenValueClass}},
	},
	{
		name: "connection-string",
		// Password=/PWD= (covers User=;Password= forms). The value stops at ';' or end of line — running to
		// end-of-text made prose mentioning "password=…" swallow the entire rest of the message.
		pattern:   regexp.MustCompile(`(?i)(?:Password|PWD)\s*=\s*([^;\r\n]+)`),
		anchors:   []string{"password", "pwd"},
		viable:    regexp.MustCompile(`(?i)^(?:Password|PWD)\s*(?:=[^;\r\n]*)?$`),
		terminals: []terminalState{{regexp.MustCompile(`(?i)^(?:Password|PWD)\s*=[^;\r\n]*$`), pwdValueClass}},
	},
	{
		name: "aws-access-key",
		// AKIA = normal, ASIA = temporary/STS: canonical 16 chars, word-bounded to avoid mid-word hits.
		pattern: regexp.MustCompile(`\b((?:AKIA|ASIA)[0-9A-Z]{16})\b`),
		anchors: []string{"akia", "asia"},
		viable:  regexp.MustCompile(`^(?:AKIA|ASIA)[0-9A-Z]*$`),
	},
	{
		name: "google-api-key",
		// AIza + 35 chars: Google Cloud API and Android/Maps keys.
		pattern: regexp.MustCompile(`\b(AIza[0-9A-Za-z_-]{35})\b`),
		anchors: []string{"aiza"},
		viable:  regexp.MustCompile(`^AIza[0-9A-Za-z_-]*$`),
	},
	{
		name: "slack",
		// xox[abprs]- + 10+ chars (bot, user, app, refresh, legacy). Every component after "xox" is optional
		// in viability: the anchor fires the moment "xox" arrives, and dead anchors are never revisited — a
		// required type letter once released tokens in pieces.
		pattern: regexp.MustCompile(`\b(xox[abprs]-[A-Za-z0-9-]{10,})\b`),
		anchors: []string{"xox"},
		viable:  regexp.MustCompile(`^xox(?:[abprs](?:-[A-Za-z0-9-]*)?)?$`),
	},
	{
		name: "jwt",
		// eyJ (base64url of `{"`) on both segments, so false positives need a coincidental eyJ match in two
		// segments. Viability must hold for EVERY prefix of a match, including ones ending mid-separator;
		// spelling out the segment structure permanently killed anchors and streamed tokens out in fragments.
		// Deliberately wider than the pattern — over-holding is bounded by the cap.
		pattern:   regexp.MustCompile(`\b(eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\b`),
		anchors:   []string{"eyj"},
		viable:    regexp.MustCompile(`^eyJ[A-Za-z0-9_.-]*$`),
		terminals: []terminalState{{regexp.MustCompile(`^eyJ[A-Za-z0-9_.-]*$`), jwtValueClass}},
	},
	{
		name: "pem-private-key",
		// Held from BEGIN until END, spanning lines. Deliberately permissive after "-----BEGIN" —
		// progressive-prefix spelling once rejected its own anchor; the hold cap bounds prose delay and
		// capOverflow scrubs the buffer rather than releasing a split match.
		pattern: regexp.MustCompile(`-----BEGIN (?:[A-Z ]+)PRIVATE KEY-----[\s\S]*?-----END (?:[A-Z ]+)PRIVATE KEY-----`),
		anchors: []string{"-----begin"},
		viable:  regexp.MustCompile(`^-----BEGIN[\s\S]*$`),
	},
}

// Derived from secretClasses (see secretClass).
var (
	// secretRegexes are the ScrubText patterns, in application order.
	secretRegexes = classPatterns()
	// anchorLiterals are every class's anchors, de-duplicated, lower-cased: every position a secretRegexes
	// match could begin (the prefilter and the stream's findAnchor); a tail with none cannot start a match.
	anchorLiterals = classAnchors()
	// anchorClassMasks[i] is the set of classes anchorLiterals[i] anchors.
	anchorClassMasks = classAnchorMasks()
	// viablePrefixRegexes decide whether a held tail is still a prefix of a possible match.
	viablePrefixRegexes = classViability()
	// terminalStates are every class's fast-path states, in class order (the first match wins).
	terminalStates = classTerminals()
)

func classPatterns() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(secretClasses))
	for i, c := range secretClasses {
		out[i] = c.pattern
	}
	return out
}

func classAnchors() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range secretClasses {
		for _, a := range c.anchors {
			if a != strings.ToLower(a) {
				panic("scrubber: anchor literal " + a + " of class " + c.name + " must be lower-case")
			}
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// classMask is a set of secretClasses indexes: the classes a text can match (see presentClasses).
type classMask uint64

func classAnchorMasks() []classMask {
	if len(secretClasses) > 64 {
		panic("scrubber: classMask holds at most 64 classes")
	}
	index := make(map[string]int, len(anchorLiterals))
	for i, a := range anchorLiterals {
		index[a] = i
	}
	out := make([]classMask, len(anchorLiterals))
	for i, c := range secretClasses {
		for _, a := range c.anchors {
			out[index[a]] |= 1 << i
		}
	}
	return out
}

func classViability() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(secretClasses))
	for i, c := range secretClasses {
		out[i] = c.viable
	}
	return out
}

func classTerminals() []terminalState {
	var out []terminalState
	for _, c := range secretClasses {
		out = append(out, c.terminals...)
	}
	return out
}
