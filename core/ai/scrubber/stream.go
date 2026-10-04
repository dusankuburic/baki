package scrubber

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// streamMaxHold bounds how much text the stream scrubber holds back while a
// potential secret is still growing; beyond it the text is emitted anyway.
const streamMaxHold = 4096

// streamMatchedHold is the tighter budget once the held text ALREADY contains
// a complete secret — holding longer only grows how much trailing text the
// redaction swallows.
const streamMatchedHold = 256

// anchorRegex finds every position a secretRegexes pattern could begin; a tail with no anchor cannot start a future match.
var anchorRegex = regexp.MustCompile(`(?i)password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|Bearer|sk_|pk_|ghp_|glpat_|AKIA|ASIA|AIza|xox|eyJ|-----BEGIN`)

// anchorLiterals expands anchorRegex's alternation into plain strings for chunk-boundary anchoring. The
// prefix patterns (AKIA/ASIA, AIza, xox, eyJ, -----BEGIN) MUST be here: anchorsByFirst is built from THIS
// list alone and a fully-arrived literal not in it is emitted verbatim — their absence leaked five of
// nine secret classes while streaming.
var anchorLiterals = []string{
	"password", "passwd", "pwd", "secret", "token",
	"apikey", "api_key", "api-key",
	"accesskey", "access_key", "access-key",
	"privatekey", "private_key", "private-key",
	"bearer", "sk_", "pk_", "ghp_", "glpat_",
	"akia", "asia", "aiza", "xox", "eyj",
	"-----begin",
}

// maxAnchorLen and anchorsByFirst are derived from anchorLiterals so the partial-anchor check only compares literals sharing the suffix's first byte.
var (
	maxAnchorLen   int
	anchorsByFirst [256][]string
)

func init() {
	for _, a := range anchorLiterals {
		if len(a) > maxAnchorLen {
			maxAnchorLen = len(a)
		}
		c := a[0]
		anchorsByFirst[c] = append(anchorsByFirst[c], a)
		anchorsByFirst[c-'a'+'A'] = append(anchorsByFirst[c-'a'+'A'], a)
	}
}

// viablePrefixRegexes decide whether a held tail is still a prefix of a possible secretRegexes match:
// each mirrors one pattern with everything after the lead-in optional, anchored to buffer end — the
// moment a character the real pattern couldn't consume arrives, the anchor is dead and released.
var viablePrefixRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)\s*(?:[:=]\s*["']?[A-Za-z0-9\-._~+/=]*["']?)?$`),
	regexp.MustCompile(`(?i)^Bearer(?:\s+[A-Za-z0-9\-._~+/]*=*)?$`),
	regexp.MustCompile(`(?i)^(?:sk_[a-zA-Z0-9_]*|pk_[a-zA-Z0-9_]*|ghp_[a-zA-Z0-9]*|glpat_[a-zA-Z0-9\-]*)$`),
	regexp.MustCompile(`(?i)^(?:Password|PWD)\s*(?:=[^;\r\n]*)?$`),
	// AWS access key IDs (AKIA/ASIA + up to 16 uppercase alphanumerics).
	regexp.MustCompile(`^(?:AKIA|ASIA)[0-9A-Z]*$`),
	// Google API key (AIza + up to 35 base64url chars).
	regexp.MustCompile(`^AIza[0-9A-Za-z_-]*$`),
	// Slack token (xox[abprs]- + up to N chars). Every component after "xox"
	// must be optional: the anchor fires the moment "xox" arrives, and dead
	// anchors are never revisited — a required type letter once released tokens in pieces.
	regexp.MustCompile(`^xox(?:[abprs](?:-[A-Za-z0-9-]*)?)?$`),
	// JWT. Viability must hold for EVERY prefix of a match, including ones
	// ending mid-separator; spelling out the segment structure permanently
	// killed anchors and streamed tokens out in fragments. Deliberately wider
	// than the secret pattern — over-holding is bounded by the cap.
	regexp.MustCompile(`^eyJ[A-Za-z0-9_.-]*$`),
	// PEM private key block: held from BEGIN until END, spans lines ([\s\S]).
	// Deliberately permissive after "-----BEGIN" — progressive-prefix
	// spelling once rejected its own anchor; the hold cap bounds prose delay
	// and capOverflow scrubs the buffer rather than releasing a split match.
	regexp.MustCompile(`^-----BEGIN[\s\S]*$`),
}

// byteClass is a 256-entry membership table — the per-byte fast path for a held value's trailing run.
type byteClass [256]bool

func makeClass(pred func(byte) bool) *byteClass {
	var c byteClass
	for i := range 256 {
		c[i] = pred(byte(i))
	}
	return &c
}

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

var (
	genericValueClass = makeClass(func(b byte) bool { return isAlnum(b) || strings.IndexByte(`-._~+/=`, b) >= 0 })
	bearerValueClass  = makeClass(func(b byte) bool { return isAlnum(b) || strings.IndexByte(`-._~+/`, b) >= 0 })
	skpkClass         = makeClass(func(b byte) bool { return isAlnum(b) || b == '_' })
	ghpClass          = makeClass(func(b byte) bool { return isAlnum(b) })
	glpatClass        = makeClass(func(b byte) bool { return isAlnum(b) || b == '-' })
	pwdValueClass     = makeClass(func(b byte) bool { return b != ';' && b != '\r' && b != '\n' })
)

// terminalStates map a viable-held tail onto the byte class of its trailing
// character run: extending the hold becomes an O(chunk) byte scan instead of
// re-running the viability regexes. A byte outside the class falls back to
// the full rescan, which re-derives the state.
var terminalStates = []struct {
	re    *regexp.Regexp
	class *byteClass
}{
	// Generic key/value with the value still open (no closing quote yet).
	{regexp.MustCompile(`(?i)^(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)\s*[:=]\s*["']?[A-Za-z0-9\-._~+/=]*$`), genericValueClass},
	// Bearer value; '=' padding is rare and handled by the rescan fallback.
	{regexp.MustCompile(`(?i)^Bearer\s+[A-Za-z0-9\-._~+/]*$`), bearerValueClass},
	{regexp.MustCompile(`(?i)^(?:sk_|pk_)[a-zA-Z0-9_]*$`), skpkClass},
	{regexp.MustCompile(`(?i)^ghp_[a-zA-Z0-9]*$`), ghpClass},
	{regexp.MustCompile(`(?i)^glpat_[a-zA-Z0-9\-]*$`), glpatClass},
	{regexp.MustCompile(`(?i)^(?:Password|PWD)\s*=[^;\r\n]*$`), pwdValueClass},
}

type holdKind uint8

const (
	holdNone    holdKind = iota // pending is empty
	holdPartial                 // pending is a proper prefix of an anchor literal
	holdViable                  // pending starts at an anchor whose tail is viable
)

// StreamScrubber applies ScrubText to chunked text, masking secrets that span boundaries: it emits only
// the prefix that can no longer interact with future input. Concatenated output equals
// ScrubText(concatenated input) while the hold cap is not hit; a dead anchor never revives. Not concurrent-safe.
type StreamScrubber struct {
	pending []byte
	kind    holdKind
	// terminal is the byte class of the held tail's trailing run (see terminalStates); nil forces a rescan.
	// draining is armed when the cap emitted a COMPLETE secret still arriving: every later byte of that
	// secret's value-run class is swallowed — without it the rest streamed out clear (JWTs, PEM bodies).
	draining *byteClass
	terminal *byteClass
}

func NewStreamScrubber() *StreamScrubber {
	return &StreamScrubber{}
}

// Write appends chunk and returns the newly releasable text, scrubbed; "" while the whole buffer could still become part of a secret.
func (s *StreamScrubber) Write(chunk string) string {
	if chunk == "" {
		return ""
	}
	// Draining the tail of a secret the cap already emitted masked —
	// emitting these bytes would leak the rest of the credential.
	if s.draining != nil {
		i := 0
		for i < len(chunk) && s.draining[chunk[i]] {
			i++
		}
		chunk = chunk[i:]
		if chunk == "" {
			return ""
		}
		s.draining = nil // a byte outside the class ends the value
	}

	// Fast path: the hold is inside a terminal character run and every new
	// byte continues it — extend the hold with no regex work.
	if s.kind == holdViable && s.terminal != nil && allInClass(chunk, s.terminal) {
		s.pending = append(s.pending, chunk...)
		return s.capOverflow()
	}
	s.pending = append(s.pending, chunk...)
	return s.release()
}

// Flush scrubs and returns everything still held; call at stream end so trailing text is delivered.
func (s *StreamScrubber) Flush() string {
	out := ScrubText(string(s.pending))
	s.pending = s.pending[:0]
	s.kind, s.terminal, s.draining = holdNone, nil, nil
	return out
}

// holdBudget returns how many bytes this hold may occupy before it is released
// anyway; the tight budget applies only once the held text already contains a
// finished match (checked lazily, so short holds never pay the scan).
func holdBudget(held []byte) int {
	if len(held) > streamMatchedHold && HasCompleteSecret(held) {
		return streamMatchedHold
	}
	return streamMaxHold
}

func allInClass(chunk string, cls *byteClass) bool {
	for i := 0; i < len(chunk); i++ {
		if !cls[chunk[i]] {
			return false
		}
	}
	return true
}

// release recomputes the hold state and emits the safe prefix: text before
// the earliest anchor whose tail is still a viable prefix of a secret
// pattern, else before a trailing partial anchor; completed matches inside
// the emitted prefix are masked by ScrubText.
func (s *StreamScrubber) release() string {
	n := len(s.pending)
	boundary := n
	s.kind, s.terminal = holdNone, nil

	off := 0
	for off < n {
		loc := anchorRegex.FindIndex(s.pending[off:])
		if loc == nil {
			break
		}
		a := off + loc[0]
		tail := s.pending[a:]
		viable := false
		for _, vp := range viablePrefixRegexes {
			if vp.Match(tail) {
				viable = true
				break
			}
		}
		if viable {
			boundary = a
			s.kind = holdViable
			for _, ts := range terminalStates {
				if ts.re.Match(tail) {
					s.terminal = ts.class
					break
				}
			}
			break
		}
		// Dead anchors stay dead (viability is end-anchored); never revisit.
		off = a + 1
	}

	// A buffer ending mid-keyword has no anchor for the loop above — hold the
	// longest suffix that is a proper prefix of an anchor literal.
	if boundary == n {
		if p := partialAnchorStart(s.pending); p < n {
			boundary = p
			s.kind = holdPartial
		}
	}

	// Hold cap: an over-budget potential match stops stalling the stream.
	// Emit the WHOLE buffer through one ScrubText pass — if it really is a
	// secret, the mask covers the complete match instead of a split leaking
	// its lead-in.
	if n-boundary > holdBudget(s.pending[boundary:]) {
		// Same drain-arming rule as capOverflow: a chunk large enough to carry
		// the key AND part of the value lands here, not in capOverflow.
		if s.terminal != nil && !s.terminal[' '] && HasCompleteSecret(s.pending[boundary:]) {
			s.draining = s.terminal
		}
		boundary = n
		s.kind, s.terminal = holdNone, nil
	}

	// Never emit a trailing PARTIAL rune: each released chunk is JSON-encoded
	// into its own SSE event, and encoding/json substitutes U+FFFD for invalid
	// UTF-8 — a provider splitting a multi-byte character across deltas once
	// garbled the text on the client. Flush() releases them at end of stream.
	boundary = runeSafeBoundary(s.pending, boundary)

	if boundary <= 0 {
		return ""
	}
	out := ScrubText(string(s.pending[:boundary]))
	m := copy(s.pending, s.pending[boundary:])
	s.pending = s.pending[:m]
	return out
}

// runeSafeBoundary reduces n to the largest offset <= n that does not cut a
// multi-byte rune in half; malformed (not merely incomplete) sequences are
// left alone, so garbage input cannot stall the stream.
func runeSafeBoundary(b []byte, n int) int {
	if n <= 0 || n > len(b) {
		return n
	}
	for i := 1; i <= 4 && i <= n; i++ {
		c := b[n-i]
		if !utf8.RuneStart(c) {
			continue // continuation byte; keep walking back to the lead byte
		}
		if want := leadRuneLen(c); want > i {
			return n - i // lead byte promises more bytes than we hold
		}
		return n
	}
	return n
}

// leadRuneLen returns the total byte length a UTF-8 rune beginning with c
// should have; an invalid lead byte reports 1 so it is emitted as-is.
func leadRuneLen(c byte) int {
	switch {
	case c < 0x80:
		return 1
	case c&0xE0 == 0xC0:
		return 2
	case c&0xF0 == 0xE0:
		return 3
	case c&0xF8 == 0xF0:
		return 4
	}
	return 1
}

// capOverflow releases a fast-path hold that outgrew the cap: the whole
// buffer goes through one ScrubText pass and the hold resets (same policy as
// release()'s cap branch).
func (s *StreamScrubber) capOverflow() string {
	if len(s.pending) <= holdBudget(s.pending) {
		return ""
	}
	// The cap bounds the hold, never at the cost of emitting half a character.
	boundary := runeSafeBoundary(s.pending, len(s.pending))
	if boundary <= 0 {
		return ""
	}
	out := ScrubText(string(s.pending[:boundary]))
	// If the cap fired on a COMPLETE secret whose value run was still open, swallow the rest via its byte
	// class — armed ONLY for space-free classes: a connection-string value runs to end of line and accepts
	// spaces, so draining it would swallow the paragraph; the real leakers (JWTs, PEM base64) are space-free.
	if s.terminal != nil && !s.terminal[' '] && HasCompleteSecret(s.pending[:boundary]) {
		s.draining = s.terminal
	}
	m := copy(s.pending, s.pending[boundary:])
	s.pending = s.pending[:m]
	s.kind, s.terminal = holdNone, nil
	return out
}

// partialAnchorStart returns the start of the longest pending-suffix that is
// a proper prefix of an anchor literal (case-insensitive), or len(pending) when none.
func partialAnchorStart(pending []byte) int {
	n := len(pending)
	kmax := maxAnchorLen - 1
	if kmax > n {
		kmax = n
	}
	for k := kmax; k >= 1; k-- {
		first := pending[n-k]
		for _, a := range anchorsByFirst[first] {
			if len(a) > k && equalFoldASCII(pending[n-k:], a[:k]) {
				return n - k
			}
		}
	}
	return n
}

// equalFoldASCII reports whether b equals the (already lowercase) s under ASCII case folding.
func equalFoldASCII(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != s[i] {
			return false
		}
	}
	return true
}
