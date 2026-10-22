package scrubber

import (
	"slices"
	"strings"
	"testing"
)

// TestSecretClasses_Structure pins the contract the stream scrubber relies on, for every class in
// secretClasses: it has a sample; each of its token-prefix anchors has one; its pattern's match in each sample
// BEGINS at one of its anchors (the stream holds from the anchor on, so a match starting earlier is split and
// never masked); viability accepts every prefix of that match from the anchor on (a prefix it rejects kills
// the anchor and the rest streams out in clear); and its anchors are in the derived anchor set.
func TestSecretClasses_Structure(t *testing.T) {
	byClass := map[string][]int{}
	for i, s := range secretSamples {
		byClass[s.class] = append(byClass[s.class], i)
	}
	names := map[string]bool{}
	for _, c := range secretClasses {
		if names[c.name] {
			t.Errorf("class name %q is used twice", c.name)
		}
		names[c.name] = true

		samples := byClass[c.name]
		if len(samples) == 0 {
			t.Errorf("class %q has no secretSamples entry: it is never run through the chunking leak tests", c.name)
			continue
		}
		if c.sampleEachAnchor {
			for _, a := range c.anchors {
				if !slices.ContainsFunc(samples, func(i int) bool { return containsFold(secretSamples[i].in, a) }) {
					t.Errorf("class %q: anchor %q (a token prefix of its own) has no sample", c.name, a)
				}
			}
		}
		for _, a := range c.anchors {
			if start, _ := findAnchor([]byte(a), 0); !slices.Contains(anchorLiterals, a) || start != 0 {
				t.Errorf("class %q: anchor %q is missing from the derived anchor set", c.name, a)
			}
		}

		for _, i := range samples {
			s := secretSamples[i]
			matches := c.pattern.FindAllStringIndex(s.in, -1)
			if len(matches) == 0 {
				t.Errorf("class %q: its own pattern does not match its sample %q", c.name, s.name)
				continue
			}
			for _, m := range matches {
				match := s.in[m[0]:m[1]]
				anchor := anchorAt(match, c.anchors)
				if anchor == "" {
					t.Errorf("class %q, sample %q: the match %q does not begin at one of the class's anchors %v",
						c.name, s.name, match, c.anchors)
					continue
				}
				for k := len(anchor); k <= len(match); k++ {
					if !c.viable.MatchString(match[:k]) {
						t.Errorf("class %q, sample %q: viability rejects the match prefix %q — the anchor would die "+
							"there and the rest of the secret stream out", c.name, s.name, match[:k])
						break
					}
				}
				// ScrubText skips the class when its lead is absent, so every match must begin with one.
				if c.lead != nil {
					if loc := c.lead.FindStringIndex(match); loc == nil || loc[0] != 0 {
						t.Errorf("class %q, sample %q: the match %q does not begin with the class's lead %q — ScrubText "+
							"would skip the pattern that masks it", c.name, s.name, match, c.lead)
					}
				}
			}
		}
	}
	for class := range byClass {
		if !names[class] {
			t.Errorf("secretSamples names class %q, which secretClasses does not define", class)
		}
	}
	if len(secretRegexes) != len(secretClasses) || len(viablePrefixRegexes) != len(secretClasses) {
		t.Errorf("derived lists out of step with secretClasses: %d patterns, %d viability regexes, %d classes",
			len(secretRegexes), len(viablePrefixRegexes), len(secretClasses))
	}
}

// TestSecretClasses_TerminalClassesKeepTheTailViable: the fast path extends a hold by bytes of a terminal's
// class without re-running viability, so every such byte must keep a tail the terminal matched viable.
func TestSecretClasses_TerminalClassesKeepTheTailViable(t *testing.T) {
	for _, c := range secretClasses {
		for _, ts := range c.terminals {
			for _, s := range secretSamples {
				if s.class != c.name {
					continue
				}
				for _, m := range c.pattern.FindAllStringIndex(s.in, -1) {
					match := s.in[m[0]:m[1]]
					anchor := anchorAt(match, c.anchors)
					for k := len(anchor); k <= len(match); k++ {
						tail := match[:k]
						if !ts.re.MatchString(tail) {
							continue
						}
						for b := 0; b < 256; b++ {
							if ts.class[b] && !c.viable.MatchString(tail+string(rune(b))) && b < 128 {
								t.Errorf("class %q: terminal class admits %q after %q, which viability rejects",
									c.name, string(rune(b)), tail)
								return
							}
						}
					}
				}
			}
		}
	}
}

func containsFold(s, sub string) bool { return strings.Contains(strings.ToLower(s), sub) }

// anchorAt returns the anchor match begins with (case-insensitively), the longest when several do.
func anchorAt(match string, anchors []string) string {
	best := ""
	for _, a := range anchors {
		if len(a) > len(best) && len(match) >= len(a) && strings.EqualFold(match[:len(a)], a) {
			best = a
		}
	}
	return best
}
