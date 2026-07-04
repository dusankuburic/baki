package scrubber

import (
	"regexp"
	"strings"
)

// streamMaxHold bounds how much text the stream scrubber holds back while a
// potential secret is still growing. Beyond this the text is emitted anyway:
// a multi-kilobyte unterminated match is pathological, and an unbounded hold
// would stall the visible stream and grow memory with the response.
const streamMaxHold = 4096

// anchorRegex finds every position where one of the secretRegexes patterns
// could begin. It is the union of their fixed lead-ins (key names, "Bearer",
// well-known token prefixes); a tail with no anchor cannot be the start of a
// future match and is safe to emit.
var anchorRegex = regexp.MustCompile(`(?i)password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|Bearer|sk_|pk_|ghp_|glpat_`)

// anchorLiterals expands anchorRegex's alternation into plain strings so a
// keyword split across chunk boundaries ("passw" + "ord=…") is detected as a
// partial anchor and held until the next chunk resolves it. Common single-
// letter suffixes ("s" → "secret") hold at most a few bytes for one chunk.
var anchorLiterals = []string{
	"password", "passwd", "pwd", "secret", "token",
	"apikey", "api_key", "api-key",
	"accesskey", "access_key", "access-key",
	"privatekey", "private_key", "private-key",
	"bearer", "sk_", "pk_", "ghp_", "glpat_",
}

// maxAnchorLen and anchorsByFirst are derived from anchorLiterals at init so
// the per-Write partial-anchor check only compares against literals that can
// actually start with the suffix's first byte.
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

// viablePrefixRegexes decide whether the buffer tail starting at an anchor is
// still a prefix of a possible secretRegexes match. Each mirrors one secret
// pattern with every component after the lead-in made optional and anchored to
// the end of the buffer (`$`): the moment a character arrives that the real
// pattern could not consume, the tail stops matching, the anchor is dead, and
// the text is released. This is what lets prose like "the password field" flow
// with at most one chunk of delay while "password=abc" stays held until the
// value's end is known.
var viablePrefixRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)\s*(?:[:=]\s*["']?[A-Za-z0-9\-._~+/=]*["']?)?$`),
	regexp.MustCompile(`(?i)^Bearer(?:\s+[A-Za-z0-9\-._~+/]*=*)?$`),
	regexp.MustCompile(`(?i)^(?:sk_[a-zA-Z0-9_]*|pk_[a-zA-Z0-9_]*|ghp_[a-zA-Z0-9]*|glpat_[a-zA-Z0-9\-]*)$`),
	regexp.MustCompile(`(?i)^(?:Password|PWD)\s*(?:=[^;\r\n]*)?$`),
}

// byteClass is a 256-entry membership table — the per-byte fast path for a
// held value's trailing character run.
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
// character run. While the tail is in such a run, extending the hold needs
// only an O(chunk) byte-class scan of the new text instead of re-running the
// viability regexes over the whole held buffer — this is what keeps the
// greedy `PWD=[^;]*` hold linear as it grows toward the cap. A byte outside
// the class (or a tail matching no terminal state, e.g. "password" with no
// delimiter yet) falls back to the full rescan, which re-derives the state.
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

// StreamScrubber applies ScrubText to text arriving in chunks, masking secrets
// that span chunk boundaries. It buffers input and emits only the prefix that
// can no longer interact with future input — everything up to the earliest
// point where a secret pattern might still be growing. Call Write per chunk
// and Flush once at end of stream; the concatenated output equals
// ScrubText(concatenated input) whenever the hold cap is not hit.
//
// The hold state is incremental: a dead anchor never revives (viability is
// anchored to the buffer end, so once broken it stays broken), which means a
// rescan is needed only when the hold's own state breaks — steady-state Writes
// cost O(chunk), not O(held).
//
// Not safe for concurrent use; chat streams invoke it from a single callback.
type StreamScrubber struct {
	pending []byte
	kind    holdKind
	// terminal is the byte class of the held tail's trailing run when the
	// viable hold is inside one (see terminalStates); nil forces a rescan.
	terminal *byteClass
}

func NewStreamScrubber() *StreamScrubber {
	return &StreamScrubber{}
}

// Write appends chunk and returns the newly releasable text, scrubbed. It
// returns "" while the whole buffer could still become part of a secret.
func (s *StreamScrubber) Write(chunk string) string {
	if chunk == "" {
		return ""
	}
	// Fast path: the hold is inside a terminal character run and every new
	// byte continues it — extend the hold without any regex work.
	if s.kind == holdViable && s.terminal != nil && allInClass(chunk, s.terminal) {
		s.pending = append(s.pending, chunk...)
		return s.capOverflow()
	}
	s.pending = append(s.pending, chunk...)
	return s.release()
}

// Flush scrubs and returns everything still held. Call at stream end (done or
// error) so trailing text is delivered.
func (s *StreamScrubber) Flush() string {
	out := ScrubText(string(s.pending))
	s.pending = s.pending[:0]
	s.kind, s.terminal = holdNone, nil
	return out
}

func allInClass(chunk string, cls *byteClass) bool {
	for i := 0; i < len(chunk); i++ {
		if !cls[chunk[i]] {
			return false
		}
	}
	return true
}

// release recomputes the hold state and emits the prefix that is safe: text
// before the earliest anchor whose tail is still a viable prefix of a secret
// pattern, else before a trailing partial anchor. Completed matches inside
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

	// A buffer ending mid-keyword has no anchor for the loop above to find —
	// hold the longest suffix that is a proper prefix of an anchor literal.
	if boundary == n {
		if p := partialAnchorStart(s.pending); p < n {
			boundary = p
			s.kind = holdPartial
		}
	}

	// Hold cap: a potential match that outgrew streamMaxHold stops stalling
	// the stream. Emit the WHOLE buffer through one ScrubText pass — if it
	// really is a secret, the mask applies to the complete match instead of a
	// split leaking its lead-in. Value bytes that arrive afterwards stream
	// unheld (the documented cap tradeoff).
	if n-boundary > streamMaxHold {
		boundary = n
		s.kind, s.terminal = holdNone, nil
	}

	if boundary <= 0 {
		return ""
	}
	out := ScrubText(string(s.pending[:boundary]))
	m := copy(s.pending, s.pending[boundary:])
	s.pending = s.pending[:m]
	return out
}

// capOverflow releases a fast-path hold that outgrew the cap: the whole
// buffer goes through one ScrubText pass (masking the complete oversized
// match) and the hold resets — same policy as release()'s cap branch.
func (s *StreamScrubber) capOverflow() string {
	if len(s.pending) <= streamMaxHold {
		return ""
	}
	out := ScrubText(string(s.pending))
	s.pending = s.pending[:0]
	s.kind, s.terminal = holdNone, nil
	return out
}

// partialAnchorStart returns the start of the longest pending-suffix that is a
// proper prefix of an anchor literal (case-insensitive), or len(pending) when
// there is none. A full anchor at the tail is the viability loop's job.
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

// equalFoldASCII reports whether b equals the (already lowercase) s under
// ASCII case folding. Non-ASCII bytes in b never match.
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
