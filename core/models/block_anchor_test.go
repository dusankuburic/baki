package models

import "testing"

func anchorDoc(lines ...int) *FlowDocument {
	d := &FlowDocument{Subflows: []Subflow{{ID: "sf", Name: "Main"}, {ID: "sf2", Name: "Other"}}}
	for i, l := range lines {
		d.Subflows[0].Blocks = append(d.Subflows[0].Blocks, Block{
			ID: string(rune('a' + i)), Name: "Invoke Url", RawType: "HTTPClient.InvokeUrl", LineNumber: l,
		})
	}
	d.Subflows[1].Blocks = []Block{{ID: "z", Name: "Invoke Url", RawType: "HTTPClient.InvokeUrl", LineNumber: 3}}
	d.RebuildIndexes()
	return d
}

// The anchor finds the same block in a re-parse whose ids are new and whose lines moved, even onto a peer's old
// line (a fix inserted one line above two adjacent blocks of the same type).
func TestResolveAnchor_SamePlaceAmongPeers(t *testing.T) {
	before := anchorDoc(2, 3, 20)
	a := before.AnchorOf("b") // line 3, the second of three
	if a == nil || a.Subflow != "Main" || a.Ordinal != 1 || a.Count != 3 {
		t.Fatalf("AnchorOf = %+v", a)
	}
	after := anchorDoc(3, 4, 21)
	if got := after.ResolveAnchor(*a); got != "b" {
		t.Errorf("ResolveAnchor = %q, want the second block (now on line 4), not the first (now on line 3)", got)
	}
	// Another subflow's block of the same type and name is never the answer.
	only := &FlowDocument{Subflows: []Subflow{{ID: "sf2", Name: "Other", Blocks: []Block{{ID: "z", Name: "Invoke Url", RawType: "HTTPClient.InvokeUrl", LineNumber: 3}}}}}
	only.RebuildIndexes()
	if got := only.ResolveAnchor(*a); got != "" {
		t.Errorf("ResolveAnchor crossed subflows: %q", got)
	}
}

// A fix that removed the block (remove-block on a duplicate) leaves its twin on the next line: the block is
// reported gone, never resolved to the twin.
func TestResolveAnchor_ChangedPeersIsMissing(t *testing.T) {
	a := anchorDoc(2, 3).AnchorOf("b") // the duplicate, second of two
	if got := anchorDoc(2).ResolveAnchor(*a); got != "" {
		t.Errorf("ResolveAnchor = %q, want nothing: the anchored block was removed", got)
	}
	if anchorDoc(2).AnchorOf("nope") != nil || (*FlowDocument)(nil).ResolveAnchor(BlockAnchor{RawType: "x"}) != "" {
		t.Error("a missing block or document must resolve to nothing")
	}
}
