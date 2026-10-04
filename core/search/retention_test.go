package search

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"pad-core/models"
)

// within reports whether s's bytes start inside src's backing array.
func within(s, src string) bool {
	if s == "" || src == "" {
		return false
	}
	lo := uintptr(unsafe.Pointer(unsafe.StringData(src)))
	p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	return p >= lo && p < lo+uintptr(len(src))
}

// TestIndexTokens_DoNotAliasIndexedText guards the retention amplifier in the
// token map. Its keys used to come straight from tokenize, and a part that
// was already lower-case was a substring of the text it came from, so every
// cached SearchIndex (FlowService keeps up to 64) could hold a long name,
// variable or property value alive through a handful of short keys. tokenize
// now slices the indexed text itself (there is no scratch copy any more), so
// the check is that no key shares a backing array with any indexed string.
// Two blocks share lower-case tokens so the second store into an existing
// key — which overwrites the stored key — is covered too.
func TestIndexTokens_DoNotAliasIndexedText(t *testing.T) {
	// Premise: the splitter really does hand back substrings of its input,
	// otherwise the check below could never see a leak.
	in := "click element"
	var parts []string
	eachPart(in, func(p string) { parts = append(parts, p) })
	if len(parts) == 0 || !within(parts[0], in) {
		t.Fatalf("fixture rot: eachPart(%q) = %q no longer aliases its input", in, parts)
	}

	script := "set the value of the element " + strings.Repeat("then wait and retry ", 32)
	doc := &models.FlowDocument{
		ID: "retention-flow",
		Subflows: []models.Subflow{{
			ID: "sf-main",
			Blocks: []models.Block{
				{
					ID: "b1", Name: "click element", Type: models.BlockTypeAction,
					RawType: "WebAutomation.Click", Variables: []string{"webpage"},
					Properties: map[string]string{"Script": script},
				},
				{
					ID: "b2", Name: "click element again", Type: models.BlockTypeAction,
					RawType:    "WebAutomation.Click",
					Properties: map[string]string{"Script": script},
				},
			},
		}},
	}
	var sources []string
	for _, b := range doc.Subflows[0].Blocks {
		sources = append(sources, b.Name, string(b.Type), b.RawType)
		sources = append(sources, b.Variables...)
		for _, v := range b.Properties {
			sources = append(sources, v)
		}
	}

	idx := NewSearchIndex(doc.ID, doc)
	if got := idx.tokens["click"]; len(got) != 2 {
		t.Fatalf("fixture rot: token %q maps to %v, want both blocks", "click", got)
	}

	var leaks []string
	for tok := range idx.tokens {
		for _, s := range sources {
			if within(tok, s) {
				leaks = append(leaks, fmt.Sprintf("%q (text %.40q)", tok, s))
				break
			}
		}
	}
	if len(leaks) > 0 {
		t.Errorf("%d index key(s) share an indexed string's backing array, pinning the whole text:\n  %s",
			len(leaks), strings.Join(leaks, "\n  "))
	}
}
