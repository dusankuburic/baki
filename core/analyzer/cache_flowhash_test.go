package analyzer

import (
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	"pad-core/models"
	"pad-core/parser"
)

// flowHashPinDoc builds a document exercising every FlowHash input: several
// subflows, nested children, properties (map order must not matter),
// variables, negative/zero/multi-digit indents, and parse errors with
// line/column/severity/message/snippet.
func flowHashPinDoc() *models.FlowDocument {
	return &models.FlowDocument{
		ID:   "pin-doc",
		Name: "Pin",
		Subflows: []models.Subflow{
			{
				ID:   "sf-main",
				Name: "Main",
				Blocks: []models.Block{
					{
						ID: "b1", RawType: "Variables.SetVariable", Name: "SET X TO 1", Indent: 0,
						Properties: map[string]string{"_var": "X", "_value": "1", "Zeta": "z", "alpha": "a=b"},
						Variables:  []string{"X", "Y"},
					},
					{
						ID: "b2", RawType: "LOOP", Name: "LOOP i FROM 1 TO 10", Indent: 4,
						Children: []models.Block{
							{ID: "b3", RawType: "Display.ShowMessageDialog", Name: "Show %i%", Indent: 12, Variables: []string{"i"}},
							{ID: "b4", RawType: "END", Name: "END", Indent: -1, Properties: map[string]string{}},
						},
					},
				},
			},
			{ID: "sf-empty", Name: "Empty"},
			{
				ID:   "sf-helper",
				Name: "Hélper \x01",
				Blocks: []models.Block{
					{ID: "b5", RawType: "CALL", Name: "CALL Main", Indent: 123456, Properties: map[string]string{"": "empty-key"}},
				},
			},
		},
		ParseErrors: []models.ParseError{
			{Line: 7, Column: 3, Message: "unexpected token", Severity: "error", Snippet: "CASE 'x'"},
			{Line: 1200, Severity: "warning", Message: "trailing", Snippet: ""},
		},
	}
}

// TestFlowHash_PinnedValues pins FlowHash output. The hash is the analysis
// cache key AND is persisted as AnalysisReport.ContentHash in history, so a
// faster implementation must produce byte-identical values. The expected
// strings were captured from the original fmt/io.WriteString implementation.
func TestFlowHash_PinnedValues(t *testing.T) {
	if got, want := FlowHash(flowHashPinDoc()), "fb632b1e5c17432a"; got != want {
		t.Errorf("FlowHash(pin doc) = %q, want %q", got, want)
	}
	if got, want := FlowHash(&models.FlowDocument{}), "e3b0c44298fc1c14"; got != want {
		t.Errorf("FlowHash(empty doc) = %q, want %q", got, want)
	}
	// A large flow crosses the hasher's internal buffer flush threshold.
	if got, want := FlowHash(buildLargeFlow(3000)), "4c479fea9fc0b858"; got != want {
		t.Errorf("FlowHash(large doc) = %q, want %q", got, want)
	}
	long := flowHashPinDoc()
	long.Subflows[0].Blocks[0].Properties["big"] = strings.Repeat("0123456789", 20000)
	if got, want := FlowHash(long), "727c31d166ff5cb2"; got != want {
		t.Errorf("FlowHash(doc with 200 KB property) = %q, want %q", got, want)
	}
}

// TestComputeFlowHash_AllocsIndependentOfBlockCount: the walk used to allocate
// a property-key slice plus fmt/[]byte temporaries per block (~3 allocs per
// block, 38k on a 378 KB flow) — nearly all of a cache hit's cost.
func TestComputeFlowHash_AllocsIndependentOfBlockCount(t *testing.T) {
	doc := buildLargeFlow(2000)
	for i := range doc.Subflows[0].Blocks {
		doc.Subflows[0].Blocks[i].Properties = map[string]string{"b": "2", "a": "1", "c": "3"}
	}
	allocs := testing.AllocsPerRun(5, func() { _, _ = computeFlowHashes(doc) })
	if allocs > 20 {
		t.Fatalf("computeFlowHashes allocated %.0f times for 2000 blocks; want a small constant (<= 20)", allocs)
	}
}

// TestFlowHash_MemoizedPerDocument: hashing the same unchanged document again
// (every cache hit does) must not re-walk it.
func TestFlowHash_MemoizedPerDocument(t *testing.T) {
	doc := flowHashPinDoc()
	before := flowHashComputes.Load()
	h1 := FlowHash(doc)
	h2 := FlowHash(doc)
	if h1 != h2 {
		t.Fatalf("hash changed between calls: %s vs %s", h1, h2)
	}
	if n := flowHashComputes.Load() - before; n != 1 {
		t.Fatalf("two FlowHash calls on one unchanged doc walked it %d times, want 1", n)
	}
}

// TestFlowHash_MemoNeverServesStaleHash: the memo is keyed on the document
// object and guarded by its Subflows/ParseErrors slice headers. A shallow copy
// with different content (ScrubDocument, service-layer copies), a reassigned
// Subflows slice, or appended parse errors must all hash afresh.
func TestFlowHash_MemoNeverServesStaleHash(t *testing.T) {
	doc := flowHashPinDoc()
	orig := FlowHash(doc)

	// Shallow copy with a cloned, edited block tree: a different object.
	cp := *doc
	cp.Subflows = append([]models.Subflow(nil), doc.Subflows...)
	cp.Subflows[0].Blocks = append([]models.Block(nil), doc.Subflows[0].Blocks...)
	cp.Subflows[0].Blocks[0].Name = "SET X TO 2"
	if FlowHash(&cp) == orig {
		t.Fatal("edited shallow copy served the original's memoized hash")
	}
	if got, fresh := FlowHash(&cp), freshFlowHash(&cp); got != fresh {
		t.Fatalf("copy hash %s != fresh walk %s", got, fresh)
	}

	// Same object, Subflows reassigned to edited content.
	doc.Subflows = cp.Subflows
	if got, want := FlowHash(doc), freshFlowHash(doc); got != want || got == orig {
		t.Fatalf("reassigned Subflows: memo returned %s, fresh walk %s (orig %s)", got, want, orig)
	}

	// Same object, a parse error appended.
	before := FlowHash(doc)
	doc.ParseErrors = append(doc.ParseErrors, models.ParseError{Line: 9, Message: "late"})
	if got := FlowHash(doc); got == before || got != freshFlowHash(doc) {
		t.Fatalf("appended parse error: memo returned %s (before %s)", got, before)
	}

	// The original value, re-created, still hashes to the pinned value.
	if got := FlowHash(flowHashPinDoc()); got != orig {
		t.Fatalf("pin doc hash drifted: %s vs %s", got, orig)
	}
}

// TestFlowHash_MemoReleasesCollectedDocuments: memo keys are weak, so a
// collected document's entry is removed rather than accumulating forever.
func TestFlowHash_MemoReleasesCollectedDocuments(t *testing.T) {
	keys := make([]weak.Pointer[models.FlowDocument], 0, 50)
	docs := make([]*models.FlowDocument, 0, 50)
	for i := 0; i < 50; i++ {
		doc := flowHashPinDoc()
		_ = FlowHash(doc)
		keys = append(keys, weak.Make(doc))
		docs = append(docs, doc)
	}
	held := func() int {
		flowHashes.mu.Lock()
		defer flowHashes.mu.Unlock()
		n := 0
		for _, k := range keys {
			if _, ok := flowHashes.m[k]; ok {
				n++
			}
		}
		return n
	}
	if held() != len(keys) {
		t.Fatal("memo did not record the hashed documents")
	}
	// docs is dead after this point, so every document is collectable.
	runtime.KeepAlive(docs)
	deadline := time.Now().Add(5 * time.Second)
	for held() > 0 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if n := held(); n > 0 {
		t.Fatalf("memo still holds %d entries for collected documents", n)
	}
}

// TestFlowHash_MemoDoesNotRetainTheBlockTree: an entry used to hold a strong
// pointer into the document's Subflows (and through them every block), so a
// dropped document's whole tree outlived it until its cleanup had run and
// another GC cycle passed. One cycle must now free it.
func TestFlowHash_MemoDoesNotRetainTheBlockTree(t *testing.T) {
	subflows := hashAndDrop()
	runtime.GC()
	if subflows.Value() != nil {
		t.Fatal("the memo kept a collected document's subflows (its block tree) alive past one GC")
	}
}

// hashAndDrop hashes a fresh document and returns a weak pointer to its
// subflows; the document itself is unreachable once it returns.
func hashAndDrop() weak.Pointer[models.Subflow] {
	doc := flowHashPinDoc()
	_ = FlowHash(doc)
	return weak.Make(&doc.Subflows[0])
}

// BenchmarkComputeFlowHash measures the uncached walk (BenchmarkFlowHash on a
// single document now measures memo hits).
func BenchmarkComputeFlowHash(b *testing.B) {
	flow := buildLargeFlow(8000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = computeFlowHashes(flow)
	}
}

// freshFlowHash is FlowHash computed by a fresh walk, bypassing the memo.
func freshFlowHash(doc *models.FlowDocument) string {
	h, _ := computeFlowHashes(doc)
	return h
}

// TestParseIdentity: the content part's second half. The same document (or a copy of it) keeps it; a re-parse of
// the same text, a whitespace-only edit (line numbers move, FlowHash does not) and a renamed copy each change it;
// it is memoized with FlowHash, one walk for both.
func TestParseIdentity(t *testing.T) {
	src := "#Region \"Main\"\nSET A TO 1\nIF A = 1 THEN\n    SET B TO 2\nEND\n#EndRegion\n"
	parse := func(text string) *models.FlowDocument {
		doc, err := parser.ParseText(text, "Main.txt", int64(len(text)))
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	doc := parse(src)
	before := flowHashComputes.Load()
	id := parseIdentity(doc)
	_ = FlowHash(doc)
	if parseIdentity(doc) != id {
		t.Fatal("parse identity changed between calls on one document")
	}
	if n := flowHashComputes.Load() - before; n != 1 {
		t.Fatalf("FlowHash and parseIdentity walked the document %d times, want 1", n)
	}
	cp := *doc
	if parseIdentity(&cp) != id {
		t.Error("a shallow copy (same ids) must keep the identity")
	}
	renamed := *doc
	renamed.Name = "Other"
	if parseIdentity(&renamed) == id {
		t.Error("the document name is part of the identity")
	}

	again := parse(src)
	if FlowHash(again) != FlowHash(doc) || parseIdentity(again) == id {
		t.Error("a re-parse must keep FlowHash and change the parse identity")
	}
	shifted := parse("\n\n" + src)
	if FlowHash(shifted) != FlowHash(doc) {
		t.Fatal("fixture rot: blank lines must not change FlowHash")
	}
	shifted.ID, shifted.Subflows = doc.ID, append([]models.Subflow(nil), shifted.Subflows...)
	for i := range shifted.Subflows {
		shifted.Subflows[i].ID = doc.Subflows[i].ID
	}
	copyIDs(shifted.Subflows[0].Blocks, doc.Subflows[0].Blocks)
	if parseIdentity(shifted) == id {
		t.Error("moved line numbers (same ids) must change the parse identity")
	}
}

// copyIDs gives dst's block tree src's ids (the trees have the same shape).
func copyIDs(dst, src []models.Block) {
	for i := range dst {
		dst[i].ID, dst[i].SubflowID = src[i].ID, src[i].SubflowID
		copyIDs(dst[i].Children, src[i].Children)
	}
}
