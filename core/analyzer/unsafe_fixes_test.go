package analyzer

import (
	"strings"
	"testing"
)

// commandInjectionFlow trips command-injection-risk. Its fixer replaces every
// %Var% with nothing, which for a real command line means deleting its
// arguments — here a mirror-copy loses both its source and its destination and
// becomes `robocopy   /MIR`, i.e. "mirror an empty source", which deletes the
// contents of whatever it is pointed at.
const commandInjectionFlow = "#Region \"Main\"\n" +
	"System.RunDOSCommand DOSCommandOrApplication: $'''robocopy %SourceDir% %ArchiveDir% /MIR /R:2'''\n" +
	"#EndRegion\n"

// sensitiveExposureFlow trips sensitive-exposure, whose fixer swaps the real
// credential reference for a literal '*** MASKED ***' — so the request still
// goes out, now guaranteed to fail authentication.
const sensitiveExposureFlow = "#Region \"Main\"\n" +
	"SET ApiToken TO 'abc123'\n" +
	"HTTPClient.InvokeWebService Url: $'''https://api.corp/orders''' " +
	"CustomHeaders: $'''Authorization: Bearer %ApiToken%''' => Resp\n" +
	"#EndRegion\n"

// TestFixLoop_SkipsBehaviourChangingFixesByDefault is the gate on unattended
// application.
//
// Both of these fixers document, in their own doc comments, that they change
// what the flow DOES — "stop the bleed" fixes the user is expected to follow up
// on. Nothing enforced that: ApplyFixesToSourceDoc applies any finding carrying
// a non-empty AutoFix, so `bakicli fix` and the UI's "fix all" applied them
// unattended, with the round-trip gate reporting success because the finding had
// indeed disappeared.
func TestFixLoop_SkipsBehaviourChangingFixesByDefault(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		mustKeep []string
	}{
		{
			name:     "command arguments are not deleted",
			source:   commandInjectionFlow,
			mustKeep: []string{"%SourceDir%", "%ArchiveDir%"},
		},
		{
			name:     "a live credential reference is not masked",
			source:   sensitiveExposureFlow,
			mustKeep: []string{"%ApiToken%"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.source
			if _, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{Limit: 20}); err != nil {
				t.Fatalf("ApplyFixesToSource: %v", err)
			}
			for _, keep := range tc.mustKeep {
				if !strings.Contains(src, keep) {
					t.Errorf("the default fix loop removed %s — that changes what the flow does at "+
						"runtime and must require an explicit opt-in.\nresult:\n%s", keep, src)
				}
			}
		})
	}
}

// TestFixLoop_AppliesBehaviourChangingFixesWhenOptedIn is the other half: the
// capability must still exist, or this is a removal dressed as a safety fix.
func TestFixLoop_AppliesBehaviourChangingFixesWhenOptedIn(t *testing.T) {
	src := commandInjectionFlow
	n, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{Limit: 20, IncludeBehaviourChanging: true})
	if err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}
	if n == 0 {
		t.Fatalf("opting in applied nothing — the fix is unreachable, not gated.\nresult:\n%s", src)
	}
	if strings.Contains(src, "%SourceDir%") {
		t.Errorf("opted in, but the command variables were not sanitised:\n%s", src)
	}
}

// TestFixLoop_SafeFixesStillApplyByDefault guards against the gate being drawn
// too wide. The ordinary reliability fixes are the reason the loop exists.
func TestFixLoop_SafeFixesStillApplyByDefault(t *testing.T) {
	src := "#Region \"Main\"\n" +
		"HTTPClient.InvokeUrl Method: GET Url: $'''https://api.example.com/x'''\n" +
		"#EndRegion\n"
	n, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{Limit: 20})
	if err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}
	if n == 0 {
		t.Errorf("no safe fix applied by default — the gate is too wide:\n%s", src)
	}
}

// TestIsBehaviourChangingFix names the set explicitly, so widening it is a
// deliberate edit rather than a side effect.
func TestIsBehaviourChangingFix(t *testing.T) {
	for _, ft := range []string{"sanitize-command-vars", "mask-sensitive-variable"} {
		if !IsBehaviourChangingFix(ft) {
			t.Errorf("%q should be gated", ft)
		}
	}
	for _, ft := range []string{
		"wrap-error-handler", "set-timeout", "insert-delay", "wrap-in-retry",
		"upgrade-to-https", "insert-close", "suppress",
	} {
		if IsBehaviourChangingFix(ft) {
			t.Errorf("%q should NOT be gated — it is an ordinary reliability fix", ft)
		}
	}
}

// TestBehaviourChangingGate_HasOneDefinition guards against the gate being added
// in one place and forgotten in the others.
//
// When this gate was first added it was consulted in exactly ONE place — the
// batch fix loop. Three other call sites reach PatchForFix directly: the cloud
// folder fix, the single-finding apply, and the assistant's proposal builder.
// The assistant one mattered most: its approval card shows a one-line summary
// with no risk signal, so "Approve" could not convey what was being accepted.
//
// This test exists so the SET is authoritative. If a fix type is added here,
// every gated path picks it up; whether a given path consults the gate is
// asserted in that path's own package.
func TestBehaviourChangingGate_HasOneDefinition(t *testing.T) {
	// A fix type not in the set must be applicable everywhere, or the gate has
	// silently widened.
	if IsBehaviourChangingFix("wrap-error-handler") {
		t.Error("wrap-error-handler is an ordinary reliability fix and must not be gated")
	}
	// And the set must not be empty — an empty set would make every caller's
	// check a no-op while looking correct.
	var gated int
	for _, ft := range []string{
		"sanitize-command-vars", "mask-sensitive-variable", "wrap-error-handler",
		"set-timeout", "insert-delay", "remove-block", "upgrade-to-https",
	} {
		if IsBehaviourChangingFix(ft) {
			gated++
		}
	}
	if gated == 0 {
		t.Error("no fix type is gated — every caller's check is a no-op")
	}
}

// undefinedVarFlows are the shapes where replace-with-variable applies cleanly
// and leaves the flow broken.
var undefinedVarFlows = map[string]string{
	"hardcoded-ip":       "#Region \"Main\"\nHTTPClient.InvokeUrl Url: $'''http://10.1.2.3/api'''\n#EndRegion\n",
	"hardcoded-filepath": "#Region \"Main\"\nFile.ReadFromFile Path: $'''C:\\\\Users\\\\bob\\\\in.txt'''\n#EndRegion\n",
}

// TestReplaceWithVariable_NotAppliedUnattended is the regression gate for the
// third behaviour-changing fixer, found after the first two were gated.
//
// replace-with-variable swaps a working literal for %input_<prop>% and emits NO
// companion initialisation, so the flow is left referencing a variable defined
// nowhere — the action still runs and now fails. That is precisely the failure
// mode mask-sensitive-variable was gated for. It went unnoticed because
// rule_uninitialized_variable deliberately ignores the input_ prefix, so the
// analyzer calls the broken result clean.
//
// It is the auto-fix for hardcoded-credential, -url, -ip, -filepath and
// -ui-coordinates: the security findings a user is most likely to accept from a
// one-line approval card.
func TestReplaceWithVariable_NotAppliedUnattended(t *testing.T) {
	for rule, flow := range undefinedVarFlows {
		t.Run(rule, func(t *testing.T) {
			src := flow
			if _, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{
				RuleFilter: map[string]bool{rule: true}, Limit: 20,
			}); err != nil {
				t.Fatalf("ApplyFixesToSource: %v", err)
			}
			lower := strings.ToLower(src)
			if strings.Contains(lower, "%input_") && !strings.Contains(lower, "set input_") {
				t.Errorf("the default fix loop introduced a reference to an undefined variable — "+
					"the action still runs and now fails:\n%s", src)
			}
		})
	}
}

// TestReplaceWithVariable_StillAvailableWhenOptedIn keeps the capability:
// parameterising a flow is the right advice, it just must not happen
// unattended.
func TestReplaceWithVariable_StillAvailableWhenOptedIn(t *testing.T) {
	src := undefinedVarFlows["hardcoded-ip"]
	n, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{
		RuleFilter:               map[string]bool{"hardcoded-ip": true},
		Limit:                    20,
		IncludeBehaviourChanging: true,
	})
	if err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}
	if n == 0 || !strings.Contains(strings.ToLower(src), "%input_") {
		t.Errorf("opting in applied nothing — the fix is unreachable rather than gated:\n%s", src)
	}
}
