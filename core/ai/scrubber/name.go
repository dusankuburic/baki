package scrubber

import (
	"regexp"
	"strings"

	"pad-core/models"
)

// Block.Name is the raw source line for the block kinds PAD writes without `key: value` pairs: IF and
// ELSE IF headers (minus THEN), SWITCH and CASE, LOOP and WAIT expressions, and every comment. A literal in
// one of them (`IF %DbPassword% = $'''Pr0d!Pass'''`) is held nowhere else, and the regex pass cannot see it:
// the `%` closing the variable sits between the keyword and the delimiter. The Name pass below is shared by
// ScrubDocument (which masks in place) and ScrubSourceText (which gathers the values to replace in the text).

// span is a half-open byte range of a string.
type span struct{ start, end int }

// varRef is a PAD variable reference, with the property path or indexes a credential name may sit in
// (%DbPassword%, %Creds.Password%, %Creds['Password']%, %Rows[0]['Password']%, %Rows[%i%]['Password']%).
var varRef = regexp.MustCompile(`%\s*([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*|\[(?:[^\]\n%]|%[^%\]\n]*%)*\])*)`)

// nameSecretSpans returns the byte ranges of b.Name that the Name pass masks, ascending and disjoint. Every
// block gives up the content of each PAD literal when the block's variables name a credential — the parser
// lists the root of a reference, so the property path or index of one (%Creds.Password%) is read off the text
// too; a CASE borrows the enclosing SWITCH's — or when the literal itself passes the entropy check. A comment,
// or a line the parser could not classify, also gives up the bare words that pass the entropy check.
func nameSecretSpans(b *models.Block, switchHoldsCredential bool) []span {
	name := b.Name
	words := b.Type == models.BlockTypeComment || b.Type == models.BlockTypeUnknown
	var lits []span
	if strings.ContainsAny(name, `'"`) {
		lits = padLiterals(name)
	}
	if !words && len(lits) == 0 {
		return nil
	}
	holds := namesCredential(b.Variables) || (b.Type == models.BlockTypeCase && switchHoldsCredential) ||
		(len(lits) > 0 && refsCredential(name))
	refs := varRefSpans(name)
	var out []span
	prev := 0
	for _, lit := range lits {
		if words {
			out = append(out, opaqueWordSpans(name, prev, lit.start)...)
		}
		prev = lit.end
		v := name[lit.start:lit.end]
		// A quoted index inside a reference (%Creds['Password']%) is the key, not a value.
		if v == "" || v == redacted || !isLiteral(v) || insideAny(refs, lit) {
			continue
		}
		if holds || looksLikeHighEntropySecret(v) {
			out = append(out, lit)
		}
	}
	if words {
		out = append(out, opaqueWordSpans(name, prev, len(name))...)
	}
	return out
}

// switchHoldsCredentialVar reports whether a SWITCH block switches on a credential: by the variables the
// parser lists, or by the property path or index of the reference in its header (%Creds.Password%).
func switchHoldsCredentialVar(b *models.Block) bool {
	return namesCredential(b.Variables) || refsCredential(b.Name)
}

// namesCredential reports whether any of the variables a block references names a credential.
func namesCredential(vars []string) bool {
	for _, v := range vars {
		if isCredentialLikeName(v) {
			return true
		}
	}
	return false
}

// refsCredential reports whether a %Variable% reference in s names a credential.
func refsCredential(s string) bool {
	for _, m := range varRef.FindAllStringSubmatch(s, -1) {
		if isCredentialLikeName(m[1]) {
			return true
		}
	}
	return false
}

// maskSpans writes the mask over each span of s. The spans are ascending and disjoint.
func maskSpans(s string, spans []span) string {
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	for _, sp := range spans {
		b.WriteString(s[prev:sp.start])
		b.WriteString(redacted)
		prev = sp.end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// padLiterals returns the content spans of the PAD string literals in s, in order: the triple-quoted forms
// (a dollar sign and three single quotes, or the three quotes alone, which may span lines), then '…' and
// "…" (one line each). A single quote opens a literal only after a non-word byte, so the apostrophe of
// "flow's" or "isn't" does not; the content runs to the first closing delimiter. An opener with no closer
// is skipped, so a stray triple quote in a chat message does not hide the literals after it.
func padLiterals(s string) []span {
	var out []span
	for i := 0; i < len(s); {
		var open, close string
		switch {
		case strings.HasPrefix(s[i:], "$'''"):
			open, close = "$'''", "'''"
		case strings.HasPrefix(s[i:], "'''"):
			open, close = "'''", "'''"
		case s[i] == '\'' || s[i] == '"':
			if i > 0 && isWordByte(s[i-1]) {
				i++
				continue
			}
			open, close = s[i:i+1], s[i:i+1]
		default:
			i++
			continue
		}
		start := i + len(open)
		n := strings.Index(s[start:], close)
		if n < 0 {
			i = start
			continue
		}
		end := start + n
		if len(close) == 1 && strings.IndexByte(s[start:end], '\n') >= 0 {
			i++
			continue
		}
		out = append(out, span{start, end})
		i = end + len(close)
	}
	return out
}

// isWordByte reports whether b is an ASCII letter, digit or underscore.
func isWordByte(b byte) bool {
	return b == '_' || ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// opaqueWordSpans returns the spans of the whitespace-separated words of s[lo:hi] that pass the entropy
// check, without the brackets, quotes and punctuation around a word, which survive the mask. A word under
// the entropy floor is skipped before the check, so most comments cost one byte scan.
func opaqueWordSpans(s string, lo, hi int) []span {
	var out []span
	for i := lo; i < hi; {
		if isSpace(s[i]) {
			i++
			continue
		}
		j := i
		for j < hi && !isSpace(s[j]) {
			j++
		}
		if j-i >= 20 {
			start, end := i, j
			for start < end && strings.IndexByte("([{<\"'`", s[start]) >= 0 {
				start++
			}
			for end > start && strings.IndexByte(")]}>\"'`.,;:!?", s[end-1]) >= 0 {
				end--
			}
			if end-start >= 20 && looksLikeHighEntropySecret(s[start:end]) {
				out = append(out, span{start, end})
			}
		}
		i = j
	}
	return out
}
