package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"weak"

	"pad-analyzer/internal/ai"
	"pad-core/ai/scrubber"
	"pad-core/models"
)

// countScrubs routes scrubDocument through a counter for the test.
func countScrubs(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	orig := scrubDocument
	scrubDocument = func(doc *models.FlowDocument) (*models.FlowDocument, error) {
		n.Add(1)
		return orig(doc)
	}
	t.Cleanup(func() { scrubDocument = orig })
	return &n
}

// One flow, many turns on different blocks and prompts: every entry points
// into ONE scrubbed clone — the per-turn entries used to hold a clone each
// (32 × 30 MB on a big flow). Scopes still get their own clone, and an
// invalidation a fresh one.
func TestCachedContextCore_SharesOneScrubbedCloneAcrossBlocks(t *testing.T) {
	scrubs := countScrubs(t)
	svc := &ChatService{chatCtxCache: newChatContextCache()}
	doc := toolLoopDoc()
	provider := ctxCacheStubProvider{}

	base := models.ChatRequest{Model: "m1", UserMessage: "q"}
	block := base
	block.ContextBlockID = "b1"
	prompt := base
	prompt.SystemPrompt = "answer tersely"

	first := svc.contextCoreForTest(context.Background(), "scope-1", provider, doc, nil, base)
	onBlock := svc.contextCoreForTest(context.Background(), "scope-1", provider, doc, nil, block)
	withPrompt := svc.contextCoreForTest(context.Background(), "scope-1", provider, doc, nil, prompt)
	if first.scrubbedDoc == nil || onBlock.scrubbedDoc != first.scrubbedDoc || withPrompt.scrubbedDoc != first.scrubbedDoc {
		t.Fatal("turns on the same flow and generation must share one scrubbed clone")
	}
	if first.contextText == onBlock.contextText {
		t.Fatal("the block turn must still get its own context text")
	}
	if n := scrubs.Load(); n != 1 {
		t.Fatalf("ScrubDocument called %d times for three turns on one flow, want 1", n)
	}

	// Another scope: its own clone (no cross-scope reuse).
	other := svc.contextCoreForTest(context.Background(), "scope-2", provider, doc, nil, base)
	if other.scrubbedDoc == first.scrubbedDoc || scrubs.Load() != 2 {
		t.Fatal("a different scope must not reuse another scope's clone")
	}

	// An in-place edit (generation bump): a fresh clone for the next turn.
	svc.InvalidateChatContext(doc.ID)
	fresh := svc.contextCoreForTest(context.Background(), "scope-1", provider, doc, nil, block)
	if fresh.scrubbedDoc == first.scrubbedDoc || scrubs.Load() != 3 {
		t.Fatal("an invalidated flow must be scrubbed again")
	}
	// The scrubbed clone is a copy: the caller's document is untouched by
	// the sharing (BlocksByID rebuilt on the clone, not aliased).
	if fresh.scrubbedDoc == doc || fresh.scrubbedDoc.BlocksByID["b1"] == doc.BlocksByID["b1"] {
		t.Fatal("the shared clone aliases the caller's document")
	}
}

// The doc cache is a small LRU: the fifth flow evicts the first, whose next
// turn on a new block scrubs again.
func TestCachedContextCore_ScrubbedDocCacheIsBounded(t *testing.T) {
	scrubs := countScrubs(t)
	svc := &ChatService{chatCtxCache: newChatContextCache()}
	provider := ctxCacheStubProvider{}
	req := models.ChatRequest{Model: "m1", UserMessage: "q"}

	docs := make([]*models.FlowDocument, maxScrubbedDocCache+1)
	for i := range docs {
		docs[i] = toolLoopDoc()
		docs[i].ID = fmt.Sprintf("flow-%d", i)
		svc.contextCoreForTest(context.Background(), "s", provider, docs[i], nil, req)
	}
	if n := scrubs.Load(); int(n) != len(docs) {
		t.Fatalf("scrubbed %d flows, want %d", n, len(docs))
	}
	onBlock := req
	onBlock.ContextBlockID = "b1"
	svc.contextCoreForTest(context.Background(), "s", provider, docs[0], nil, onBlock)
	if n := scrubs.Load(); int(n) != len(docs)+1 {
		t.Fatalf("the evicted flow's clone was reused (%d scrubs), want a fresh scrub", n)
	}
	// The most recent flow is still held: no scrub for its next block.
	svc.contextCoreForTest(context.Background(), "s", provider, docs[len(docs)-1], nil, onBlock)
	if n := scrubs.Load(); int(n) != len(docs)+1 {
		t.Fatalf("a held clone was scrubbed again (%d scrubs)", n)
	}
}

// A clone above maxCachedScrubbedBlocks is never pinned: neither the doc
// cache nor the text entries hold it, so a turn on such a flow rebuilds it
// (and the memoised texts are reused).
func TestCachedContextCore_HugeCloneIsNotPinned(t *testing.T) {
	scrubs := countScrubs(t)
	svc := &ChatService{chatCtxCache: newChatContextCache()}
	provider := ctxCacheStubProvider{}
	blocks := make([]models.Block, maxCachedScrubbedBlocks+1)
	for i := range blocks {
		blocks[i] = models.Block{ID: fmt.Sprintf("b%d", i), Name: "Step", Type: models.BlockTypeAction, RawType: "Foo.Bar"}
	}
	doc := &models.FlowDocument{ID: "huge", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}
	doc.RebuildIndexes()
	req := models.ChatRequest{Model: "m1", UserMessage: "q"}

	first := svc.contextCoreForTest(context.Background(), "s", provider, doc, nil, req)
	again := svc.contextCoreForTest(context.Background(), "s", provider, doc, nil, req)
	if first.scrubbedDoc == nil || len(first.scrubbedDoc.BlocksByID) != len(blocks) {
		t.Fatal("the turn must still get a complete scrubbed clone")
	}
	if first.contextText != again.contextText || first.sysPrompt != again.sysPrompt {
		t.Fatal("the texts of a huge flow must be memoised")
	}
	if n := scrubs.Load(); n != 2 {
		t.Fatalf("a huge clone was pinned: %d scrubs for two turns, want 2", n)
	}
	cc := svc.chatCtxCache
	if _, held := cc.docs.Get(scrubbedDocKey("s", "huge", 0)); held {
		t.Fatal("the doc cache holds a clone above the ceiling")
	}
	key := svc.chatContextKeyAt("s", "huge", 0, req, provider.ID(), req.Model, nil, "", svc.contextTokenBudget(context.Background(), provider, req.Model), ai.ToolAccessNone)
	if _, ok := cc.texts.Get(key); !ok {
		t.Fatal("the text entry was not cached")
	}
}

// The clones alive are bounded by the doc cache alone: a flow edited between
// turns gets a new generation — and a new clone — per turn, and the 32 text
// entries used to pin one each (32 × ~40 MiB on a flow just under the
// ceiling). Six generations of one flow leave at most maxScrubbedDocCache
// clones reachable once the garbage collector has run.
func TestCachedContextCore_TextEntriesDoNotPinClones(t *testing.T) {
	svc := &ChatService{chatCtxCache: newChatContextCache()}
	provider := ctxCacheStubProvider{}
	doc := toolLoopDoc()
	req := models.ChatRequest{Model: "m1", UserMessage: "q"}
	const generations = maxScrubbedDocCache + 2

	var clones []weak.Pointer[models.FlowDocument]
	for g := 0; g < generations; g++ {
		if g > 0 {
			svc.InvalidateChatContext(doc.ID)
		}
		cv := svc.contextCoreForTest(context.Background(), "s", provider, doc, nil, req)
		// A second turn on the same generation: a text hit that must return
		// the SAME clone while the doc cache holds it.
		if again := svc.contextCoreForTest(context.Background(), "s", provider, doc, nil, req); again.scrubbedDoc != cv.scrubbedDoc {
			t.Fatalf("generation %d: a text hit returned a different clone while the doc cache holds one", g)
		}
		clones = append(clones, weak.Make(cv.scrubbedDoc))
	}
	cc := svc.chatCtxCache
	for g := 0; g < generations; g++ {
		if _, ok := cc.texts.Get(svc.chatContextKeyAt("s", doc.ID, uint64(g), req, provider.ID(), req.Model, nil, "", svc.contextTokenBudget(context.Background(), provider, req.Model), ai.ToolAccessNone)); !ok {
			t.Fatalf("generation %d: its text entry was evicted; the texts LRU no longer covers every generation", g)
		}
	}
	runtime.GC()
	runtime.GC()
	alive := 0
	for _, c := range clones {
		if c.Value() != nil {
			alive++
		}
	}
	if alive > maxScrubbedDocCache {
		t.Fatalf("%d clones alive after %d generations, want at most %d (the text entries pin them)", alive, generations, maxScrubbedDocCache)
	}
	// The evicted generations' turns still work: the clone is rebuilt.
	svc.InvalidateChatContext(doc.ID)
	if cv := svc.contextCoreForTest(context.Background(), "s", provider, doc, nil, req); cv.scrubbedDoc == nil || cv.scrubbedDoc == doc {
		t.Fatal("a turn after eviction must get a fresh scrubbed clone")
	}
}

// padSourceSvc synthesises size bytes of PAD-like source (see the ai package's
// padSource) for the source-file bound below.
func padSourceSvc(size int) string {
	var b strings.Builder
	b.Grow(size + 512)
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "    Variables.SetVariable NewValue: $'''Value %d''' Name: Var%d\r\n", i*7919%10000, i%97)
		fmt.Fprintf(&b, "    WebAutomation.NavigateTo Url: $'''https://example.com/page%d''' BrowserInstance: %%Browser%%\r\n", i)
	}
	return b.String()
}

// A selected 5 MB source file is bounded BEFORE the AST scrub: the scrub
// (a parse of the whole text, ~0.1–0.8 s per turn on a file this size) and
// the tokeniser see at most the budget × 16 bytes of it, and what they see
// is the file's head, whole lines, marked as cut.
func TestComputeContextCore_BoundsSourceFilesBeforeScrubbing(t *testing.T) {
	dir := t.TempDir()
	src := padSourceSvc(5 << 20)
	if err := os.WriteFile(filepath.Join(dir, "Main.txt"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := &models.FlowDocument{ID: "f1", FilePath: filepath.Join(dir, "flow.txt"), Name: "f1"}
	doc.RebuildIndexes()

	var seen atomic.Int64
	orig := scrubSourceText
	scrubSourceText = func(text string) string {
		seen.Add(int64(len(text)))
		return orig(text)
	}
	t.Cleanup(func() { scrubSourceText = orig })

	// The budget pinned at the old default: the bound is budget × 16 bytes, 64 KB here.
	pinned := &staticSettings{s: models.AppSettings{AI: models.AISettings{Providers: map[string]models.AIProviderConfig{
		ctxCacheStubProvider2{}.ID(): {ContextTokenBudget: defaultContextTokenBudget},
	}}}}
	svc := &ChatService{chatCtxCache: newChatContextCache(), flowCache: &FlowService{}, settings: pinned}
	req := models.ChatRequest{Model: "m1", UserMessage: "q", SelectedSourceFiles: []string{"Main.txt"}}
	cv := svc.contextCoreForTest(context.Background(), "scope", ctxCacheStubProvider2{}, doc, nil, req)
	if n := seen.Load(); n == 0 || n >= 128<<10 {
		t.Errorf("the scrub was handed %d bytes of a %d-byte file, want under 128 KB", n, len(src))
	}
	if !strings.Contains(cv.contextText, "#### Main.txt") || !strings.Contains(cv.contextText, "Variables.SetVariable NewValue: $'''Value 0'''") {
		t.Errorf("the file's head is missing from the context: %q", cv.contextText[:min(300, len(cv.contextText))])
	}
	// The AST scrub still applies to what is kept: PAD's `SET Password TO`
	// literal (which no key=value regex matches) in the kept head is masked,
	// and the bound's own cut keeps the file parseable for it.
	const literal = "super-secret-password-r4"
	secret := "FUNCTION Main\r\n    SET Password TO $'''" + literal + "'''\r\n" + padSourceSvc(200<<10) + "END\r\n"
	if masked := scrubber.ScrubSourceText(secret[:120]); strings.Contains(masked, literal) {
		t.Fatalf("fixture rot: the scrubber no longer masks the fixture (%q)", masked)
	}
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "Main.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	secretDoc := &models.FlowDocument{ID: "f2", FilePath: filepath.Join(secretDir, "flow.txt"), Name: "f2"}
	secretDoc.RebuildIndexes()
	cv = svc.contextCoreForTest(context.Background(), "scope", ctxCacheStubProvider2{}, secretDoc, nil, req)
	if strings.Contains(cv.contextText, literal) {
		t.Error("a secret in the kept head reached the context unmasked")
	}
	if !strings.Contains(cv.contextText, "SET Password TO") {
		t.Errorf("the kept head lost its structure: %q", cv.contextText[:min(300, len(cv.contextText))])
	}
}
