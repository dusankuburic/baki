package analyzer

import (
	"fmt"

	"pad-core/models"
)

// PatchForFix builds the patch for a deterministic auto-fix, dispatching on
// fixType. It is the single source of truth for the fixType → patch mapping,
// shared by FlowService.generateFixPatch (desktop/server apply-fix + preview)
// and the headless CLI (bakicli fix). Keeping the dispatch in the analyzer
// package means the CLI can apply fixes without depending on FlowService
// (which carries desktop file-path/storage wiring).
//
// variable and property come from the finding's Metadata (keys "variable" /
// "property"); only the fixers that need them read them. ruleID is used only by
// the "suppress" fix (to write the pad-ignore directive). Returns an empty
// patch (no ops) and an error for an unknown fixType.
func PatchForFix(block *models.Block, fixType, ruleID, variable, property string) (models.Patch, error) {
	switch fixType {
	case "wrap-error-handler":
		return WrapInErrorHandlerPatch(block), nil
	case "insert-close":
		return InsertClosePatch(block), nil
	case "set-timeout":
		return SetTimeoutPatch(block), nil
	case "insert-delay":
		return InsertDelayPatch(block), nil
	case "insert-delay-in-loop":
		return InsertDelayInLoopPatch(block), nil
	case "insert-handler-log":
		return InsertHandlerLogPatch(block), nil
	case "init-variable":
		return InsertVariableInitPatch(block, variable), nil
	case "insert-error-log":
		return InsertErrorLogPatch(block), nil
	case "replace-with-variable":
		return ReplaceWithVariablePatch(block, property), nil
	case "wrap-in-retry":
		return WrapInRetryPatch(block), nil
	case "insert-exit-condition":
		return InsertExitConditionPatch(block), nil
	case "remove-block":
		return RemoveBlockPatch(block), nil
	case "parameterize-sql":
		return ParameterizeSqlPatch(block, property), nil
	case "append-output":
		return AppendOutputPatch(block), nil
	case "mask-sensitive-variable":
		return MaskSensitiveVariablePatch(block, variable), nil
	case "upgrade-to-https":
		return UpgradeToHttpsPatch(block), nil
	case "sanitize-command-vars":
		return SanitizeCommandVarsPatch(block), nil
	case "insert-default":
		return InsertDefaultPatch(block), nil
	case "suppress":
		return SuppressFindingPatch(block, ruleID), nil
	default:
		return models.Patch{}, fmt.Errorf("unknown fix type %q", fixType)
	}
}

// behaviourChangingFixes are fix types that alter what the flow DOES at runtime,
// as opposed to how it handles failure.
//
// Both entries say so in their own doc comments — they are "stop the bleed"
// fixes the user is expected to follow up on. Nothing enforced that: the fix
// loop applies any finding carrying a non-empty AutoFix, so `bakicli fix` and
// the UI's "fix all" applied them unattended, and the round-trip gate reported
// success because the finding had indeed disappeared. What the gate never asked
// was whether the flow still means the same thing.
//
//   - sanitize-command-vars replaces each %Var% in a command line with NOTHING,
//     so `robocopy %Src% %Dst% /MIR` becomes `robocopy   /MIR` — a mirror-delete
//     against an empty source.
//   - mask-sensitive-variable substitutes '*** MASKED ***' for a credential
//     reference, so the request still goes out and now fails authentication.
//
// Deliberately NOT included: remove-block. Deleting a block the analyzer has
// shown to be unreachable or redundant does not change a successful run — the
// risk there is the ANALYSIS being wrong, which is a correctness problem in the
// rule, not a reason to gate the fix.
//
// Also not included: parameterize-sql. It rewrites %Var% to @Var, which is
// behaviour-changing only if PAD has no bind-parameter support at that call
// site. I could not verify that, and gating it on a guess would be its own kind
// of wrong.
var behaviourChangingFixes = map[string]bool{
	"sanitize-command-vars":   true,
	"mask-sensitive-variable": true,
	// replace-with-variable swaps a working literal for %input_<prop>% and
	// emits NO companion initialisation, so the flow is left referencing a
	// variable that is defined nowhere. Measured on the default fix loop:
	//
	//	HTTPClient.InvokeUrl Url: $'''http://10.1.2.3/api'''
	//	  -> HTTPClient.InvokeUrl Url: $'''%input_url%'''      (never defined)
	//	File.ReadFromFile Path: $'''C:\Users\bob\in.txt'''
	//	  -> File.ReadFromFile Path: $'''%input_path%'''       (never defined)
	//
	// That is exactly the failure mode mask-sensitive-variable is gated for —
	// the action still runs and now fails — and the analyzer calls the result
	// clean, because rule_uninitialized_variable deliberately ignores the
	// input_ prefix. It is the auto-fix for hardcoded-credential,
	// hardcoded-url, hardcoded-ip, hardcoded-filepath and
	// hardcoded-ui-coordinates, i.e. the security findings a user is most
	// likely to accept from a one-line approval card.
	//
	// Parameterising a flow is the right advice; doing it unattended is not.
	// It stays available under the explicit opt-in (bakicli fix -unsafe /
	// FixOptions.IncludeBehaviourChanging).
	"replace-with-variable": true,
}

// IsBehaviourChangingFix reports whether fixType alters runtime behaviour and so
// requires an explicit opt-in before an unattended loop applies it.
func IsBehaviourChangingFix(fixType string) bool {
	return behaviourChangingFixes[fixType]
}
