package scrubber

import (
	"strings"
	"testing"
)

// PERF-7: ScrubText's prefilter skips the regex scans for text that holds
// none of the patterns' lead-ins, and stays a strict superset of them —
// including the two runes the regexes' (?i) folds that a plain lower-casing
// does not (U+017F long s → 's', U+212A Kelvin sign → 'k').
func TestMayContainSecret_Prefilter(t *testing.T) {
	clean := strings.Repeat("The flow reads a spreadsheet, filters the rows and writes a report. ", 40)
	if mayContainSecret(clean) {
		t.Error("clean prose must not pass the prefilter")
	}
	if got := ScrubText(clean); got != clean {
		t.Error("clean prose must come back unchanged")
	}

	for _, c := range secretSamples {
		if !mayContainSecret(c.in) {
			t.Errorf("%s: the prefilter rejected a text ScrubText masks: %q", c.name, c.in)
		}
		if strings.Contains(ScrubText(c.in), c.secret) {
			t.Errorf("%s: secret survived ScrubText", c.name)
		}
	}
	for _, a := range anchorLiterals {
		if !mayContainSecret("x " + strings.ToUpper(a) + " y") {
			t.Errorf("anchor %q (upper-cased) must pass the prefilter", a)
		}
	}

	// Unicode folds the regexes honour under (?i).
	for _, tc := range []struct{ name, in string }{
		{"long s in password", "paſſword=SuperSecret123 done"},
		{"kelvin sign in api key", "api_Key=SuperSecret123 done"},
		{"kelvin sign in token", "toKen: SuperSecret123 done"},
	} {
		if !mayContainSecret(tc.in) {
			t.Errorf("%s: the prefilter rejected %q", tc.name, tc.in)
		}
		if got := ScrubText(tc.in); strings.Contains(got, "SuperSecret123") {
			t.Errorf("%s: ScrubText(%q) = %q, secret survived", tc.name, tc.in, got)
		}
	}
}
