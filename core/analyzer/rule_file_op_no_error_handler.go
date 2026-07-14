package analyzer

import (
	"strings"

	"pad-core/models"
)

var fileOperationPrefixes = []string{
	"File.Read",
	"File.Write",
	"File.Delete",
	"File.Move",
	"File.Copy",
	"File.Rename",
	"File.Create",
	"File.OpenTextFile",
	"File.CloseTextFile",
	"Folder.Create",
	"Folder.Delete",
	"Folder.Move",
	"Folder.Copy",
	"Folder.GetSubfolders",
	"Folder.GetFiles",
}

type FileOpNoErrorHandlerRule struct{}

func (r *FileOpNoErrorHandlerRule) ID() string   { return "file-op-no-error-handler" }
func (r *FileOpNoErrorHandlerRule) Name() string { return "File operation without error handler" }
func (r *FileOpNoErrorHandlerRule) Description() string {
	return "File/Folder operations that commonly fail (locks, permissions, missing paths) without an error handler."
}
func (r *FileOpNoErrorHandlerRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *FileOpNoErrorHandlerRule) Category() string                 { return "Reliability" }

func (r *FileOpNoErrorHandlerRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeAction {
		return nil
	}

	if !isFileOperation(block.RawType) {
		return nil
	}

	if HasErrorHandlerAncestor(ctx, block) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "File operation without error handler",
		Description: "This file/folder operation can fail due to missing files, permission issues, or file locks but has no error handler.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Wrap this action in an 'On Block Error' handler to manage missing files, permission errors, and file lock issues gracefully.",
		Metadata:    map[string]interface{}{"rawType": block.RawType},
	}}
}

func isFileOperation(rawType string) bool {
	for _, prefix := range fileOperationPrefixes {
		if strings.HasPrefix(rawType, prefix) {
			return true
		}
	}
	return false
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&FileOpNoErrorHandlerRule{}) }
