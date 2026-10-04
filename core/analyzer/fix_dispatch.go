package analyzer

import (
	"fmt"

	"pad-core/models"
)

// PatchForFix builds the patch for a deterministic auto-fix, dispatching on
// fixType: the single source of truth shared by FlowService.generateFixPatch
// and the headless CLI. variable/property come from the finding's Metadata;
// ruleID is used only by the "suppress" fix. Unknown fixType ⇒ empty patch + error.
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

// behaviourChangingFixes are fix types that alter what the flow DOES at
// runtime, as opposed to how it handles failure. Nothing used to enforce
// that: the fix loop applied any finding with a non-empty AutoFix, so
// `bakicli fix` applied them unattended and the round-trip gate reported
// success because the finding had disappeared — never asking whether the
// flow still means the same thing.
//
//   - sanitize-command-vars replaces each %Var% in a command line with
//     NOTHING (`robocopy %Src% %Dst% /MIR` becomes a mirror-delete against
//     an empty source).
//   - mask-sensitive-variable substitutes '*** MASKED ***' for a credential
//     reference, so the request still goes out and now fails authentication.
//
// Deliberately NOT included: remove-block (deleting an unreachable block does
// not change a successful run — the risk is the ANALYSIS being wrong); and
// parameterize-sql (behaviour-changing only if PAD lacks bind-parameter
// support at that call site, which could not be verified — gating it on a
// guess would be its own kind of wrong).
var behaviourChangingFixes = map[string]bool{
	"sanitize-command-vars":   true,
	"mask-sensitive-variable": true,
	// replace-with-variable swaps a working literal for %input_<prop>% with
	// NO companion initialisation, leaving the flow referencing a variable
	// defined nowhere — the same failure mode mask-sensitive-variable is gated
	// for, and the analyzer calls the result clean (rule_uninitialized_variable
	// deliberately ignores the input_ prefix). It is the auto-fix for the
	// hardcoded-* security findings a user is most likely to accept from a
	// one-line approval card: parameterising is the right advice, doing it
	// unattended is not — it stays behind the explicit opt-in
	// (FixOptions.IncludeBehaviourChanging).
	"replace-with-variable": true,
}

// IsBehaviourChangingFix reports whether fixType alters runtime behaviour and so
// requires an explicit opt-in before an unattended loop applies it.
func IsBehaviourChangingFix(fixType string) bool {
	return behaviourChangingFixes[fixType]
}
