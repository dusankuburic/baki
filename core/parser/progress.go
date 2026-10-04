package parser

import "pad-core/models"

// ProgressCallback receives percent-done updates (0-100) and a short status
// message during a long-running parse.
type ProgressCallback func(percent int, message string)

// ParseTextWithProgress parses text while reporting progress via onProgress.
//
// For files below 1 MB (or a nil callback) it delegates to ParseText and never
// invokes the callback — small parses are fast enough that percent accounting
// would dwarf the work. For larger inputs it runs the same Parser.Parse
// pipeline threaded through WithProgress, so the progress and non-progress
// paths can never drift (they were once two copies, and the progress copy
// silently dropped the EOF unclosed-block flush).
func ParseTextWithProgress(text, fileName string, fileSize int64, onProgress ProgressCallback) (*models.FlowDocument, error) {
	if onProgress == nil || fileSize < 1_000_000 {
		return ParseText(text, fileName, fileSize)
	}
	p := NewParser(text, fileName, fileSize, WithProgress(onProgress))
	return p.Parse()
}
