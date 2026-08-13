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
