package rag

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// censusStore counts the identity census and the stranded-by-document aggregate.
type censusStore struct {
	*tu.FakeBackend
	census   atomic.Int32
	stranded atomic.Int32
}

func (c *censusStore) CountKnowledgeChunksByIdentity(ctx context.Context, orgID, identity string) (interfaces.KnowledgeChunkCounts, error) {
	c.census.Add(1)
	return c.FakeBackend.CountKnowledgeChunksByIdentity(ctx, orgID, identity)
}

func (c *censusStore) CountStrandedKnowledgeChunksByDocument(ctx context.Context, orgID, identity string) (map[string]int, error) {
	c.stranded.Add(1)
	return c.FakeBackend.CountStrandedKnowledgeChunksByDocument(ctx, orgID, identity)
}

// keyCountingFactory is a real factory whose key lookup counts its reads (one per credential it resolves): openai
// is keyed for every scope, nothing else.
func keyCountingFactory(reads *atomic.Int32) *ai.ProviderFactory {
	return ai.NewProviderFactory(func(_ string, provider string) (string, error) {
		reads.Add(1)
		if provider == "openai" {
			return "test-key", nil
		}
		return "", nil
	}, nil, nil, nil)
}

// R-knowledge-1: a member whose embedder's identity sees no chunk of a corpus that has some (every chunk embedded under
// another model) gets ErrEmbedderMismatch before any Embed call — the query embedding would be paid for nothing — and
// the stranded search is counted as before. A legacy row (no identity) is visible to every caller, so it searches.
func TestSearch_IdentityMismatchRefusedBeforeEmbedding(t *testing.T) {
	store := &censusStore{FakeBackend: &tu.FakeBackend{EmbeddingDim: 1}}
	store.SeedOrgs("org-1")
	seedIdentityChunks(store.FakeBackend, []float32{1}, "B", "B")
	emb := &namedEmbedder{identity: "A", vec: []float32{1}}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))

	stranded := testutil.ToFloat64(ragStrandedSearchTotal)
	for i := 0; i < 2; i++ {
		got, err := svc.Search(context.Background(), "u1", "org-1", "what is the retry policy?")
		if !errors.Is(err, ErrEmbedderMismatch) || got != "" {
			t.Fatalf("Search %d = %q, %v; want ErrEmbedderMismatch", i, got, err)
		}
	}
	if emb.calls != 0 {
		t.Errorf("embedded %d queries for a corpus the caller's model cannot see, want 0", emb.calls)
	}
	if d := testutil.ToFloat64(ragStrandedSearchTotal) - stranded; d != 2 {
		t.Errorf("pad_rag_stranded_search_total rose by %v, want 2", d)
	}
	if n := store.census.Load(); n != 1 {
		t.Errorf("the identity census ran %d times in two searches, want 1 (cached)", n)
	}

	// The corpus changes (an upload, a re-index): the census is read again, and a legacy row makes it searchable.
	seedIdentityChunks(store.FakeBackend, []float32{1}, "B", "")
	svc.InvalidateCorpus("org-1")
	got, err := svc.Search(context.Background(), "u1", "org-1", "what is the retry policy?")
	if err != nil || !strings.Contains(got, "guideline 1") {
		t.Errorf("Search after the corpus changed = %q, %v; want the legacy chunk", got, err)
	}
	if n := store.census.Load(); n != 2 {
		t.Errorf("the census ran %d times, want 2 (the corpus invalidation drops it)", n)
	}
}

// R-observability-5: the resolved embedder is memoised per (scope, org): two searches of the same caller in the
// same org read the key store once; another caller resolves their own; a credential change of the caller, a
// settings (policy) change and a peer's invalidation drop the memo.
func TestSearch_MemoisesTheEmbedderPerScopeAndOrg(t *testing.T) {
	var reads atomic.Int32
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1")
	// A corpus under another identity: the search stops before the (network) embed, after the resolution.
	seedIdentityChunks(store, []float32{1}, "someone-else")
	bus := eventbus.New(nil, 0)
	svc := NewKnowledgeService(store, keyCountingFactory(&reads), stubSettings{&models.AppSettings{}}, WithEventBus(bus))
	SubscribeEmbedderCache(bus, svc)
	search := func(scope string) {
		t.Helper()
		if _, err := svc.Search(context.Background(), scope, "org-1", "what is the retry policy?"); !errors.Is(err, ErrEmbedderMismatch) {
			t.Fatalf("Search(%s) = %v; want ErrEmbedderMismatch", scope, err)
		}
	}

	search("u1")
	first := reads.Load()
	if first == 0 {
		t.Fatal("the embedder was resolved without a key read")
	}
	search("u1")
	if n := reads.Load(); n != first {
		t.Errorf("a second search within the memo's TTL read the key store %d more times, want 0", n-first)
	}
	search("u2")
	if n := reads.Load(); n != 2*first {
		t.Errorf("another caller's search read %d keys, want %d (their own resolution)", n-first, first)
	}

	for _, change := range []struct {
		name    string
		publish func()
	}{
		{"credential", func() {
			bus.Publish(context.Background(), events.CredentialChanged{Scope: "u1", ProviderID: "openai", Kind: events.CredentialSaved})
		}},
		{"org credential", func() {
			bus.Publish(context.Background(), events.CredentialChanged{Scope: ai.OrgScope("org-1"), OrgID: "org-1", Kind: events.CredentialSaved})
		}},
		{"deployment settings", func() {
			bus.Publish(context.Background(), events.SettingsChanged{Scope: events.DeploymentSettings})
		}},
		{"org settings", func() {
			bus.Publish(context.Background(), events.SettingsChanged{Scope: events.OrgSettings, ID: "org-1"})
		}},
		{"membership", func() {
			bus.Publish(context.Background(), events.OrgMemberChanged{OrgID: "org-1", UserID: "u1", Kind: events.OrgMemberRemoved})
		}},
		{"account erased", func() { bus.Publish(context.Background(), events.AccountErased{UserID: "u1", KeyScope: "u1"}) }},
		{"peer", func() { bus.DeliverRemote("rag.embedders", eventbus.NewScope("scope", "u1")) }},
		{"peer resync", func() { bus.DeliverRemote("rag.embedders", nil) }},
	} {
		before := reads.Load()
		change.publish()
		search("u1")
		if reads.Load() == before {
			t.Errorf("%s change: the next search used the memoised embedder; want a fresh resolution", change.name)
		}
	}

	// A change of another caller's credential leaves u1's memo alone.
	search("u1")
	before := reads.Load()
	bus.Publish(context.Background(), events.CredentialChanged{Scope: "u2", ProviderID: "openai", Kind: events.CredentialSaved})
	search("u1")
	if n := reads.Load(); n != before {
		t.Errorf("another caller's credential change dropped u1's memo (%d reads)", n-before)
	}
}

// R-observability-5: a search names the time it spent in each phase, and an empty result's census for ops (the
// stranded and width signals) runs only when the caller asks for it, after delivering the result.
func TestSearchDetailed_PhasesAndTheEmptyResultCensusAsAFollowUp(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1")
	// One chunk at the caller's identity pointing away from the query (a miss), one under another identity.
	seedIdentityChunks(store, []float32{1, 0}, "A", "B")
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(&namedEmbedder{identity: "A", vec: []float32{0, 1}}))

	stranded := testutil.ToFloat64(ragStrandedSearchTotal)
	res, err := svc.SearchDetailed(context.Background(), "u1", "org-1", "unrelated question about nothing")
	if err != nil || res.Guidelines != "" {
		t.Fatalf("SearchDetailed = %+v, %v; want a miss", res, err)
	}
	var phases []string
	for _, p := range res.Phases {
		phases = append(phases, p.Name)
	}
	if !slices.Equal(phases, []string{PhaseResolve, PhaseEmbed, PhaseSearch}) {
		t.Errorf("phases = %v, want resolve, embed, search", phases)
	}
	if d := testutil.ToFloat64(ragStrandedSearchTotal) - stranded; d != 0 {
		t.Errorf("the census ran inside the search (stranded +%v); it is the caller's follow-up", d)
	}
	follow := res.FollowUp()
	if follow == nil {
		t.Fatal("an empty result with stranded chunks has no follow-up")
	}
	follow(context.Background())
	if d := testutil.ToFloat64(ragStrandedSearchTotal) - stranded; d != 1 {
		t.Errorf("the follow-up counted %v stranded searches, want 1", d)
	}

	// A hit has nothing to explain.
	hit := NewKnowledgeService(store, nil, nil, WithEmbedder(&namedEmbedder{identity: "A", vec: []float32{1, 0}}))
	res, err = hit.SearchDetailed(context.Background(), "u1", "org-1", "a guideline question")
	if err != nil || res.Guidelines == "" || res.FollowUp() != nil {
		t.Errorf("hit = %+v, %v; want guidelines and no follow-up", res, err)
	}
}

// R-knowledge-8: a question naming an exact action reaches the chunk that names it even when the embedding scores
// that chunk under the vector floor — the lexical half of the search finds it and the fusion keeps it.
func TestSearch_LexicalHitBelowTheVectorFloor(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1")
	_ = store.SaveKnowledgeDocument(context.Background(), "", &interfaces.KnowledgeDocument{ID: "doc-1", OrgID: "org-1", Filename: "policy.md"})
	_ = store.SaveKnowledgeChunks(context.Background(), "", []interfaces.KnowledgeChunk{
		{ID: "c-dos", DocID: "doc-1", Content: "Never use Run DOS command with admin rights.", Embedding: []float32{1, 0}, EmbeddingIdentity: "A"},
		{ID: "c-near", DocID: "doc-1", Content: "Retry a failed web request three times.", Embedding: []float32{0, 1}, EmbeddingIdentity: "A"},
	})
	// The query vector is orthogonal to the DOS chunk: cosine 0, far under the 0.5 floor.
	emb := &namedEmbedder{identity: "A", vec: []float32{0, 1}}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))

	got, err := svc.Search(context.Background(), "u1", "org-1", "Is Run DOS command allowed in this flow?")
	if err != nil || !strings.Contains(got, "Never use Run DOS command") {
		t.Fatalf("Search = %q, %v; want the chunk naming the action", got, err)
	}
	if !strings.Contains(got, "Retry a failed web request") {
		t.Errorf("Search = %q; the dense hit must stay beside the lexical one", got)
	}
}

// lexicalPhrases picks what in a question a chunk would name verbatim: runs of two or three words not starting or
// ending on a stop word, identifier-like tokens (%Variables%, file names, error codes, rule ids, CamelCase and
// acronyms) and quoted text — never a lone common word, which would match half the corpus.
func TestLexicalPhrases(t *testing.T) {
	got := lexicalPhrases("How should I configure Run DOS command for %Invoice_Total% in flow.pad? It fails with 0x80070005 and \"Access is denied\"; see hardcoded-credential.")
	for _, want := range []string{"run dos command", "dos command", "run dos", "%invoice_total%", "flow.pad", "0x80070005", "access is denied", "hardcoded-credential"} {
		if !slices.Contains(got, want) {
			t.Errorf("phrases %q lack %q", got, want)
		}
	}
	for _, unwanted := range []string{"how should", "should i", "i configure", "for", "it fails", "in", "see"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("phrases %q hold %q", got, unwanted)
		}
	}
	if len(got) > maxLexicalPhrases {
		t.Errorf("%d phrases, want at most %d", len(got), maxLexicalPhrases)
	}
	if got := lexicalPhrases("thanks"); len(got) != 0 {
		t.Errorf("a lone word gave phrases %q", got)
	}
}

// Reciprocal rank fusion: a chunk both lists rank goes first, then each list's own hits by rank, the dense list's
// first on a tie; each chunk once; at most limit.
func TestFuseRanked(t *testing.T) {
	c := func(id string) interfaces.KnowledgeChunk { return interfaces.KnowledgeChunk{ID: id} }
	dense := []interfaces.KnowledgeChunk{c("d1"), c("both"), c("d3")}
	lexical := []interfaces.KnowledgeChunk{c("l1"), c("both")}
	var ids []string
	for _, x := range fuseRanked(dense, lexical, 4) {
		ids = append(ids, x.ID)
	}
	if want := []string{"both", "d1", "l1", "d3"}; !slices.Equal(ids, want) {
		t.Errorf("fused = %v, want %v", ids, want)
	}
	if got := fuseRanked(dense, nil, 2); len(got) != 2 || got[0].ID != "d1" {
		t.Errorf("dense only = %v, want its first two in order", got)
	}
}

// R-knowledge-15: the knowledge list resolves the viewer's embedder once per memo TTL and aggregates the stranded
// chunks per document once per corpus generation; a corpus change (an upload on any replica) drops the aggregate, so
// the next list shows it.
func TestListDocuments_MemoisesTheViewerIdentityAndTheStrandedMap(t *testing.T) {
	var reads atomic.Int32
	store := &censusStore{FakeBackend: &tu.FakeBackend{EmbeddingDim: 1}}
	store.SeedOrgs("org-1")
	seedIdentityChunks(store.FakeBackend, []float32{1}, "someone-else", "someone-else")
	bus := eventbus.New(nil, 0)
	svc := NewKnowledgeService(store, keyCountingFactory(&reads), stubSettings{&models.AppSettings{}}, WithEventBus(bus))
	SubscribeCorpusCache(bus, svc)
	SubscribeEmbedderCache(bus, svc)

	list := func() map[string]int {
		t.Helper()
		docs, err := svc.ListDocuments(context.Background(), "u1", "org-1", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, d := range docs {
			out[d.ID] = d.EmbeddingStranded
		}
		return out
	}
	if got := list(); got["doc-1"] != 2 {
		t.Fatalf("stranded = %v, want doc-1: 2", got)
	}
	resolved := reads.Load()
	if got := list(); got["doc-1"] != 2 {
		t.Fatalf("second list stranded = %v", got)
	}
	if n := reads.Load(); n != resolved {
		t.Errorf("the second list resolved the embedder again (%d key reads), want the memo", n-resolved)
	}
	if n := store.stranded.Load(); n != 1 {
		t.Errorf("two lists aggregated the stranded chunks %d times, want 1", n)
	}

	// Another document lands under the other identity, and its upload is announced.
	_ = store.SaveKnowledgeDocument(context.Background(), "", &interfaces.KnowledgeDocument{ID: "doc-2", OrgID: "org-1", Filename: "new.md"})
	_ = store.SaveKnowledgeChunks(context.Background(), "", []interfaces.KnowledgeChunk{{ID: "new-1", DocID: "doc-2", Content: "new", Embedding: []float32{1}, EmbeddingIdentity: "someone-else"}})
	bus.Publish(context.Background(), events.KnowledgeDocChanged{OrgID: "org-1", DocID: "doc-2", Kind: events.KnowledgeDocAdded})
	if got := list(); got["doc-2"] != 1 {
		t.Errorf("after the upload stranded = %v, want doc-2: 1", got)
	}
}

// SearchLimited returns at most limit chunks (the search_knowledge tool's page); 0 or more than a page is a page.
func TestSearchLimited_CapsTheChunks(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1")
	seedIdentityChunks(store, []float32{1}, "A", "A", "A")
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(&namedEmbedder{identity: "A", vec: []float32{1}}))
	for _, tc := range []struct{ limit, want int }{{1, 1}, {2, 2}, {0, 3}, {99, 3}} {
		res, err := svc.SearchLimited(context.Background(), "u1", "org-1", "what do the guidelines say?", tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(res.Guidelines, "\n- "); got != tc.want {
			t.Errorf("limit %d: %d chunks, want %d:\n%s", tc.limit, got, tc.want, res.Guidelines)
		}
	}
}

// R-observability-5 review: a key store that cannot be read (a database blip, a credential that does not decrypt) is
// not "no key": the resolution answers ErrEmbeddingSetupUnavailable instead of falling back to another provider of
// the member's (whose vectors the corpus may not hold) or calling the member keyless, and the answer is not memoised,
// so the next search after the store recovers resolves the configured provider.
func TestEmbedder_KeyStoreReadFailureIsNotNoEmbedderAndNotMemoised(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	f := ai.NewProviderFactory(func(_ string, provider string) (string, error) {
		if provider == "openai" && failing.Load() {
			return "", errors.New("keystore: connection reset")
		}
		if provider == "openai" || provider == "glm" {
			return "k", nil
		}
		return "", nil
	}, nil, nil, nil)
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1")
	seedIdentityChunks(store, []float32{1}, "someone-else")
	svc := NewKnowledgeService(store, f, stubSettings{&models.AppSettings{}})

	_, err := svc.Search(context.Background(), "u1", "org-1", "what is the retry policy?")
	if !errors.Is(err, ErrEmbeddingSetupUnavailable) || errors.Is(err, ErrEmbeddingUnavailable) {
		t.Fatalf("Search with an unreadable key store = %v; want ErrEmbeddingSetupUnavailable, not ErrEmbeddingUnavailable", err)
	}
	failing.Store(false)
	p, err := svc.resolvedEmbedder(context.Background(), "u1", "org-1")
	if err != nil || p.ID() != "openai" {
		t.Errorf("after the key store recovered: %v, %v; want the configured openai (the failure was memoised or fell back)", p, err)
	}
}

// A chunk's source is shown to the model as "[name]": a name with brackets cannot close that attribution early, and
// a document stored before uploads refused them keeps no bidi override, zero-width mark or line separator there.
func TestAssembleGuidelines_SourceNameCannotBreakItsAttribution(t *testing.T) {
	got := assembleGuidelines([]interfaces.KnowledgeChunk{
		{ID: "c1", DocID: "d1", Filename: "Policy [v2]\u202Egnp.md\u2028x", Content: "Retry three times."},
	})
	if !strings.Contains(got, "- [Policy (v2)gnp.mdx] Retry three times.") {
		t.Errorf("guidelines = %q; want the source with brackets as parentheses and no invisible characters", got)
	}
}
