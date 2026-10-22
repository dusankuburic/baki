package scrubber

import (
	"strings"
	"testing"
)

// PERF-7: ScrubText's prefilter skips the regex scans for text that holds
// none of the patterns' lead-ins, and stays a strict superset of them —
// including the two runes the regexes' (?i) folds that a plain lower-casing
// does not (U+017F long s → 's', U+212A Kelvin sign → 'k'). Per class, too:
// the class that masks a sample must be in the sample's mask, or ScrubText
// skips the one pattern that would have matched.
func TestPresentClasses_Prefilter(t *testing.T) {
	clean := strings.Repeat("The flow reads a spreadsheet, filters the rows and writes a report. ", 40)
	if presentClasses(clean) != 0 {
		t.Error("clean prose must not pass the prefilter")
	}
	if got := ScrubText(clean); got != clean {
		t.Error("clean prose must come back unchanged")
	}

	classIndex := map[string]int{}
	for i, c := range secretClasses {
		classIndex[c.name] = i
	}
	for _, c := range secretSamples {
		if present := presentClasses(c.in); present&(1<<classIndex[c.class]) == 0 {
			t.Errorf("%s: the prefilter leaves class %q out of the mask %b for a text it masks: %q", c.name, c.class, present, c.in)
		}
		if strings.Contains(ScrubText(c.in), c.secret) {
			t.Errorf("%s: secret survived ScrubText", c.name)
		}
	}
	for i, a := range anchorLiterals {
		if present := presentClasses("x " + strings.ToUpper(a) + " y"); present&anchorClassMasks[i] != anchorClassMasks[i] {
			t.Errorf("anchor %q (upper-cased) must pass the prefilter for every class it anchors: got %b, want %b", a, present, anchorClassMasks[i])
		}
	}

	// Unicode folds the regexes honour under (?i).
	for _, tc := range []struct{ name, in string }{
		{"long s in password", "paſſword=SuperSecret123 done"},
		{"kelvin sign in api key", "api_Key=SuperSecret123 done"},
		{"kelvin sign in token", "toKen: SuperSecret123 done"},
	} {
		if presentClasses(tc.in) == 0 {
			t.Errorf("%s: the prefilter rejected %q", tc.name, tc.in)
		}
		if got := ScrubText(tc.in); strings.Contains(got, "SuperSecret123") {
			t.Errorf("%s: ScrubText(%q) = %q, secret survived", tc.name, tc.in, got)
		}
	}
}
