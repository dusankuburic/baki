package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/testutil"

	"pad-core/cache"
	"pad-core/models"
	"pad-core/parser"
)

// countingCache wraps the AST cache so a test can count what a folder load
// did: a Get miss followed by a Set is one member parse (parseFolderMember), a
// Get hit is a member served from the cache, a Set without a Get is a writer
// seeding its own parse (cacheFolderMember).
type countingCache struct {
	inner            cache.Cache
	gets, hits, sets atomic.Int64
}

func newCountingCache(t *testing.T) *countingCache {
	t.Helper()
	inner, err := cache.NewLRUCache(100)
	if err != nil {
		t.Fatal(err)
	}
	return &countingCache{inner: inner}
}

func (c *countingCache) Get(ctx context.Context, key string) (any, bool) {
	c.gets.Add(1)
	v, ok := c.inner.Get(ctx, key)
	if ok {
		c.hits.Add(1)
	}
	return v, ok
}

func (c *countingCache) Set(ctx context.Context, key string, value any, ttl time.Duration) {
	c.sets.Add(1)
	c.inner.Set(ctx, key, value, ttl)
}

func (c *countingCache) Delete(ctx context.Context, key string) { c.inner.Delete(ctx, key) }

func (c *countingCache) reset() {
	c.gets.Store(0)
	c.hits.Store(0)
	c.sets.Store(0)
}

// misses is the number of member parses a folder load performed.
func (c *countingCache) misses() int64 { return c.gets.Load() - c.hits.Load() }

func writeMembers(t *testing.T, dir string, members map[string]string) {
	t.Helper()
	for name, src := range members {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func newCachedFolderSvc(t *testing.T) (*FlowService, *countingCache) {
	t.Helper()
	cc := newCountingCache(t)
	return NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, cc), cc
}

// assertSameFolderShape checks got against ParseFolder's own merge of the
// same directory: member order (Main first, then by name), SourceFile on
// every subflow, the blocks of each, the file list and the summed metadata —
// the merge semantics a cached member must not change.
func assertSameFolderShape(t *testing.T, got, want *models.FlowDocument) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("nil doc: got=%v want=%v", got == nil, want == nil)
	}
	if !got.IsFolder || got.Name != want.Name || got.FilePath != want.FilePath {
		t.Fatalf("doc identity: isFolder=%v name=%q path=%q, want folder %q at %q", got.IsFolder, got.Name, got.FilePath, want.Name, want.FilePath)
	}
	if len(got.Subflows) != len(want.Subflows) {
		t.Fatalf("subflows = %d, want %d", len(got.Subflows), len(want.Subflows))
	}
	for i := range want.Subflows {
		g, w := got.Subflows[i], want.Subflows[i]
		if g.Name != w.Name || g.SourceFile != w.SourceFile || len(g.Blocks) != len(w.Blocks) {
			t.Fatalf("subflow %d = %q from %q with %d blocks, want %q from %q with %d", i, g.Name, g.SourceFile, len(g.Blocks), w.Name, w.SourceFile, len(w.Blocks))
		}
		for j := range w.Blocks {
			if g.Blocks[j].Name != w.Blocks[j].Name || g.Blocks[j].LineNumber != w.Blocks[j].LineNumber {
				t.Fatalf("subflow %q block %d = %q@%d, want %q@%d", w.Name, j, g.Blocks[j].Name, g.Blocks[j].LineNumber, w.Blocks[j].Name, w.Blocks[j].LineNumber)
			}
		}
		if got.SubflowsByID[g.ID] != &got.Subflows[i] {
			t.Fatalf("subflow %q is not indexed in SubflowsByID", g.Name)
		}
		for j := range g.Blocks {
			if got.BlocksByID[g.Blocks[j].ID] == nil || got.BlockSubflow[g.Blocks[j].ID] != &got.Subflows[i] {
				t.Fatalf("block %q of %q is not indexed", g.Blocks[j].Name, g.Name)
			}
		}
	}
	if len(got.Files) != len(want.Files) {
		t.Fatalf("files = %d, want %d", len(got.Files), len(want.Files))
	}
	for i := range want.Files {
		if got.Files[i] != want.Files[i] {
			t.Fatalf("file %d = %+v, want %+v", i, got.Files[i], want.Files[i])
		}
	}
	if got.Metadata.BlockCount != want.Metadata.BlockCount || got.Metadata.SubflowCount != want.Metadata.SubflowCount ||
		got.Metadata.FileSize != want.Metadata.FileSize || got.Metadata.RawLineCount != want.Metadata.RawLineCount ||
		got.Metadata.MaxDepth != want.Metadata.MaxDepth || len(got.ParseErrors) != len(want.ParseErrors) {
		t.Fatalf("metadata %+v (%d parse errors), want %+v (%d)", got.Metadata, len(got.ParseErrors), want.Metadata, len(want.ParseErrors))
	}
}

// subflowIDsByFile maps each member file to the subflow ID its subflow got —
// a member served from the cache keeps its IDs, a re-parsed one mints new ones.
func subflowIDsByFile(doc *models.FlowDocument) map[string]string {
	out := map[string]string{}
	for _, sf := range doc.Subflows {
		out[sf.SourceFile] = sf.ID
	}
	return out
}

// TestLoadFlowFolder_ReparsesOnlyChangedMembers: a desktop folder flow is
// re-loaded after every fix click, suppression, undo, source save and watcher
// re-import, and every reload re-read and re-parsed the WHOLE folder — no
// per-member cache, while single-file flows had the content-hash AST cache.
// Loading a 3-member folder, then loading it again after one member changed,
// must parse exactly that one member; the merge stays ParseFolder's.
func TestLoadFlowFolder_ReparsesOnlyChangedMembers(t *testing.T) {
	dir := t.TempDir()
	members := map[string]string{
		"Main.txt":  "#Region \"Main\"\nWAIT 1\n#EndRegion\n",
		"Alpha.txt": "#Region \"Alpha\"\nWAIT 2\n#EndRegion\n",
		"Beta.txt":  "#Region \"Beta\"\nWAIT 3\n#EndRegion\n",
	}
	writeMembers(t, dir, members)
	svc, cc := newCachedFolderSvc(t)

	first, err := svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatalf("LoadFlowFolder: %v", err)
	}
	if cc.misses() != 3 || cc.sets.Load() != 3 {
		t.Fatalf("cold load: %d misses, %d sets; want every member parsed once (3/3)", cc.misses(), cc.sets.Load())
	}
	ref, err := parser.ParseFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSameFolderShape(t, first, ref)

	// Nothing changed: the reload parses nothing.
	cc.reset()
	second, err := svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatalf("LoadFlowFolder (warm): %v", err)
	}
	if cc.hits.Load() != 3 || cc.sets.Load() != 0 {
		t.Fatalf("warm reload: %d hits, %d parses; want 3 hits and no parse", cc.hits.Load(), cc.sets.Load())
	}
	assertSameFolderShape(t, second, ref)
	if ids1, ids2 := subflowIDsByFile(first), subflowIDsByFile(second); ids1["Main.txt"] != ids2["Main.txt"] || ids1["Alpha.txt"] != ids2["Alpha.txt"] || ids1["Beta.txt"] != ids2["Beta.txt"] {
		t.Fatalf("an unchanged member was re-parsed (new subflow IDs): %v vs %v", ids1, ids2)
	}

	// One member changed: exactly that member is parsed.
	cc.reset()
	writeMembers(t, dir, map[string]string{"Beta.txt": "#Region \"Beta\"\nWAIT 3\nWAIT 4\n#EndRegion\n"})
	third, err := svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatalf("LoadFlowFolder (one member changed): %v", err)
	}
	if cc.sets.Load() != 1 || cc.misses() != 1 || cc.hits.Load() != 2 {
		t.Fatalf("after one member changed: %d parses (%d misses, %d hits); want exactly 1 parse, 2 hits", cc.sets.Load(), cc.misses(), cc.hits.Load())
	}
	ref2, err := parser.ParseFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSameFolderShape(t, third, ref2)
	if third.Metadata.BlockCount != first.Metadata.BlockCount+1 {
		t.Fatalf("block count = %d, want %d: the reload does not reflect the edited member", third.Metadata.BlockCount, first.Metadata.BlockCount+1)
	}
	ids1, ids3 := subflowIDsByFile(first), subflowIDsByFile(third)
	if ids1["Main.txt"] != ids3["Main.txt"] || ids1["Alpha.txt"] != ids3["Alpha.txt"] {
		t.Fatalf("unchanged members were re-parsed: %v vs %v", ids1, ids3)
	}
	if ids1["Beta.txt"] == ids3["Beta.txt"] {
		t.Fatal("the edited member was served from the cache (same subflow ID)")
	}
	if svc.docProvider.CurrentDoc() != third {
		t.Fatal("the reloaded folder is not the current document")
	}
}

// TestLoadFlowFolder_NoCacheStillParses: the member parser degrades to a
// plain parse without an AST cache (tests, or a deployment without one).
func TestLoadFlowFolder_NoCacheStillParses(t *testing.T) {
	dir := t.TempDir()
	writeMembers(t, dir, map[string]string{"Main.txt": desktopMainSrc, "Subflow.txt": desktopSubflowSrc})
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil)
	doc, err := svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatalf("LoadFlowFolder: %v", err)
	}
	ref, err := parser.ParseFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSameFolderShape(t, doc, ref)
}

// TestPatchFlow_FolderReloadReusesTheGateParse: a fix click on a folder flow
// (here a suppression) writes one member and re-loads the folder. The gate
// already parsed the bytes it wrote; seeded under the member's name, the
// reload parses NOTHING — every member is a cache hit — and still reflects
// the patched file.
func TestPatchFlow_FolderReloadReusesTheGateParse(t *testing.T) {
	dir := t.TempDir()
	writeMembers(t, dir, map[string]string{"Main.txt": desktopMainSrc, "Subflow.txt": desktopSubflowSrc})
	svc, cc := newCachedFolderSvc(t)
	doc, err := svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatalf("LoadFlowFolder: %v", err)
	}
	var setBlock string
	for _, sf := range doc.Subflows {
		if sf.SourceFile == "Subflow.txt" && len(sf.Blocks) > 0 {
			setBlock = sf.Blocks[0].ID
		}
	}
	if setBlock == "" {
		t.Fatal("fixture rot: Subflow.txt has no block")
	}

	cc.reset()
	updated, err := svc.SuppressFindingInSource(context.Background(), doc, setBlock, "redundant-action")
	if err != nil {
		t.Fatalf("SuppressFindingInSource: %v", err)
	}
	if got := readMember(t, dir, "Subflow.txt"); !strings.Contains(got, "pad-ignore[redundant-action]") {
		t.Fatalf("Subflow.txt was not patched:\n%s", got)
	}
	// One seed (the gate's parse of the written member), then a reload that
	// parses nothing: 2 members, 2 hits, 0 misses.
	if cc.misses() != 0 || cc.hits.Load() != 2 || cc.sets.Load() != 1 {
		t.Fatalf("folder reload after a patch: %d misses, %d hits, %d sets; want 0 parses, 2 hits, 1 seed", cc.misses(), cc.hits.Load(), cc.sets.Load())
	}
	ref, err := parser.ParseFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSameFolderShape(t, updated, ref)
	if updated.Metadata.BlockCount != doc.Metadata.BlockCount+1 {
		t.Fatalf("block count = %d, want %d (the directive is a comment block)", updated.Metadata.BlockCount, doc.Metadata.BlockCount+1)
	}
}

// TestApplyFixBatch_FolderReloadReusesTheLoopParse: the desktop folder batch
// seeds each fixed member's final parse, so its folder reload parses nothing.
func TestApplyFixBatch_FolderReloadReusesTheLoopParse(t *testing.T) {
	dir := t.TempDir()
	writeMembers(t, dir, map[string]string{"Main.txt": desktopMainSrc, "Subflow.txt": desktopSubflowSrc})
	svc, cc := newCachedFolderSvc(t)
	doc, err := svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatalf("LoadFlowFolder: %v", err)
	}

	cc.reset()
	out, err := svc.ApplyFixBatchResult(context.Background(), doc, map[string]bool{"redundant-action": true}, 10)
	if err != nil || out.Applied == 0 || out.Partial {
		t.Fatalf("ApplyFixBatchResult = %+v, %v; want fixes applied, not partial", out, err)
	}
	if got := readMember(t, dir, "Subflow.txt"); strings.Contains(got, "SET X TO %X%") {
		t.Fatalf("Subflow.txt still carries the redundant SET:\n%s", got)
	}
	if cc.misses() != 0 || cc.hits.Load() != 2 || cc.sets.Load() != 1 {
		t.Fatalf("folder reload after a batch: %d misses, %d hits, %d sets; want 0 parses, 2 hits, 1 seed", cc.misses(), cc.hits.Load(), cc.sets.Load())
	}
	ref, err := parser.ParseFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSameFolderShape(t, out.Doc, ref)
}

// TestFolderMemberCacheKey_IsPerNameAndContent: the key must separate members
// with equal bytes but different names (the parse depends on the name) and
// never collide with loadAndParse's content-only single-file entries.
func TestFolderMemberCacheKey_IsPerNameAndContent(t *testing.T) {
	a := folderMemberCacheKey("A.txt", "WAIT 1\n")
	if a == folderMemberCacheKey("B.txt", "WAIT 1\n") {
		t.Error("two members with the same bytes and different names share a key")
	}
	if a == folderMemberCacheKey("A.txt", "WAIT 2\n") {
		t.Error("two versions of the same member share a key")
	}
	if !strings.HasPrefix(a, "ast:") || len(a) != len("ast:")+64 {
		t.Errorf("key %q is not in the AST cache's ast:<sha256> form", a)
	}
}
