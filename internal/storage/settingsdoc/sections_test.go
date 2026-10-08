package settingsdoc_test

import (
	"slices"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/storage/settingsdoc"
	"pad-core/models"
)

// TestSections_CoverTheDocument: every top-level key of both AppSettings types is a declared section and every
// section is a key of both, so a new top-level key cannot be stored without a version.
func TestSections_CoverTheDocument(t *testing.T) {
	for name, doc := range map[string]any{"models": models.AppSettings{}, "storage": interfaces.AppSettings{}} {
		var keys []string
		for _, k := range settingsdoc.DocumentKeys(doc) {
			if k != settingsdoc.DocVersionKey {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		if !slices.Equal(keys, settingsdoc.Default.Keys()) {
			t.Errorf("%s AppSettings has sections %v, declared %v", name, keys, settingsdoc.Default.Keys())
		}
	}
}
