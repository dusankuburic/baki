package scrubber

import (
	"math/rand"
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"
)

// backtrackBudget mirrors regexp's unexported maxBacktrackProg: a program over it never runs on the bit-state
// backtracker, only on the NFA, which costs several times more per input byte.
const backtrackBudget = 500

// programSize compiles re the way package regexp does and returns its instruction count.
func programSize(t *testing.T, re *regexp.Regexp) int {
	t.Helper()
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		t.Fatalf("parse %q: %v", re, err)
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		t.Fatalf("compile %q: %v", re, err)
	}
	return len(prog.Inst)
}

// TestStreamScrubber_HotPathRegexesFitTheBacktracker pins the engine every regex on release()'s per-delta
// path runs on: a program over the budget is executed by the NFA for every byte of every chunk. The anchor
// alternation derived from the class table (75 literals under (?i)) compiled to 639 instructions and made
// every stream benchmark 5x slower than the hand-kept 25-literal list; the anchor scan is a byte scan now
// (findAnchor), and the viability and terminal regexes must stay within the budget — as must every class's
// pattern and lead, which release() runs through ScrubText on each chunk it emits.
func TestStreamScrubber_HotPathRegexesFitTheBacktracker(t *testing.T) {
	check := func(name string, re *regexp.Regexp) {
		if n := programSize(t, re); n > backtrackBudget {
			t.Errorf("%s compiles to %d instructions, over the %d the backtracker accepts: release() runs it on the NFA", name, n, backtrackBudget)
		}
	}
	for _, c := range secretClasses {
		check(c.name+" pattern", c.pattern)
		if c.lead != nil {
			check(c.name+" lead", c.lead)
		}
		check(c.name+" viable", c.viable)
		for _, ts := range c.terminals {
			check(c.name+" terminal", ts.re)
		}
	}
}

// TestFindAnchor_MatchesTheAlternation: findAnchor replaced a (?i) alternation of anchorLiterals; for ASCII
// input the two must agree on every leftmost match (U+017F and U+212A, which (?i) folds to s and k, are the
// documented exception, as they were for the partial-anchor check all along).
func TestFindAnchor_MatchesTheAlternation(t *testing.T) {
	quoted := make([]string, len(anchorLiterals))
	for i, a := range anchorLiterals {
		quoted[i] = regexp.QuoteMeta(a)
	}
	alternation := regexp.MustCompile(`(?i)` + strings.Join(quoted, "|"))

	compare := func(in []byte) {
		t.Helper()
		for off := 0; off <= len(in); off++ {
			start, end := findAnchor(in, off)
			loc := alternation.FindIndex(in[off:])
			wantStart, wantEnd := -1, -1
			if loc != nil {
				wantStart, wantEnd = off+loc[0], off+loc[1]
			}
			if start != wantStart || end != wantEnd {
				t.Fatalf("findAnchor(%q, %d) = (%d, %d), the alternation finds (%d, %d)", in, off, start, end, wantStart, wantEnd)
			}
		}
	}

	for _, s := range secretSamples {
		compare([]byte(s.in))
	}
	// Anchors in every case, back to back, and split by one byte so partial literals abound.
	var dense strings.Builder
	for _, a := range anchorLiterals {
		dense.WriteString(strings.ToUpper(a) + a + " " + a[:len(a)-1] + "|")
	}
	compare([]byte(dense.String()))

	rng := rand.New(rand.NewSource(1))
	alphabet := "pasword tkenscrAPIKEY_-?&=:/@;xoeyjAIZASKghpl-BEGIN\n"
	for range 300 {
		b := make([]byte, 1+rng.Intn(48))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		compare(b)
	}
}
