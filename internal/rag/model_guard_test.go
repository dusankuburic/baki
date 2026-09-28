package rag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// fixedEmbedder answers every input with the same vector — a stand-in for
// "some embedding model", distinguishable from another one by direction.
type fixedEmbedder struct {
	ai.Provider
	vec   []float32
	calls int
}

func (f *fixedEmbedder) ID() string { return "fixed-embed" }
func (f *fixedEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	f.calls++
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = append([]float32(nil), f.vec...)
	}
	return out, nil
}

// seedCorpus stores one document of org-1 with chunks whose stored embedding
// is vec — a corpus some earlier model indexed.
func seedCorpus(store *testutil.FakeBackend, vec []float32, contents ...string) {
	_ = store.SaveKnowledgeDocument(context.Background(), &interfaces.KnowledgeDocument{ID: "doc-1", OrgID: "org-1", Filename: "policy.md"})
	chunks := make([]interfaces.KnowledgeChunk, len(contents))
	for i, c := range contents {
		chunks[i] = interfaces.KnowledgeChunk{ID: "chunk-" + string(rune('a'+i)), DocID: "doc-1", Content: c, Embedding: append([]float32(nil), vec...)}
	}
	_ = store.SaveKnowledgeChunks(context.Background(), "", chunks)
}

func storedEmbeddings(store *testutil.FakeBackend) map[string][]float32 {
	out := map[string][]float32{}
	for id, c := range store.KnowledgeChunks {
		out[id] = append([]float32(nil), c.Embedding...)
	}
	return out
}

func sameEmbeddings(a, b map[string][]float32) bool {
	if len(a) != len(b) {
		return false
	}
	for id, v := range a {
		w, ok := b[id]
		if !ok || len(v) != len(w) {
			return false
		}
		for i := range v {
			if v[i] != w[i] {
				return false
			}
		}
	}
	return true
}

func guardService(store *testutil.FakeBackend, emb ai.Provider) *KnowledgeService {
	return NewKnowledgeService(store, nil, nil, WithEmbedder(emb))
}

// TestReindexOrg_ModelChangedRefusedUnlessForced pins #35: a re-index by an
// admin whose embedding model differs from the corpus's (same width, so the
// width guards cannot see it) is refused and leaves the store untouched;
// force rewrites every chunk with the caller's model.
func TestReindexOrg_ModelChangedRefusedUnlessForced(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedCorpus(store, []float32{1, 0}, "alpha rule", "beta rule", "gamma rule", "delta rule")
	before := storedEmbeddings(store)
	emb := &fixedEmbedder{vec: []float32{0, 1}}
	svc := guardService(store, emb)

	_, err := svc.ReindexOrg(context.Background(), "scope", "org-1", false)
	if !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Fatalf("err = %v, want ErrEmbeddingModelChanged", err)
	}
	if !sameEmbeddings(before, storedEmbeddings(store)) {
		t.Error("a refused re-index changed stored embeddings")
	}
	if emb.calls != 1 {
		t.Errorf("embed calls = %d, want 1 (the sample only, not the corpus)", emb.calls)
	}

	res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true)
	if err != nil {
		t.Fatalf("forced re-index: %v", err)
	}
	if res.Chunks != 4 || res.Docs != 1 {
		t.Errorf("result = %+v, want 4 chunks in 1 doc", res)
	}
	for id, v := range storedEmbeddings(store) {
		if len(v) != 2 || v[0] != 0 || v[1] != 1 {
			t.Errorf("chunk %s = %v, want the caller's model [0 1]", id, v)
		}
	}
	// The corpus now matches the caller: an unforced re-index passes.
	if _, err := svc.ReindexOrg(context.Background(), "scope", "org-1", false); err != nil {
		t.Errorf("re-index after the switch: %v", err)
	}
}

// TestReindexOrg_SameModelPasses: the model that indexed the corpus
// reproduces its vectors (up to noise) — no confirmation needed.
func TestReindexOrg_SameModelPasses(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedCorpus(store, []float32{1, 0}, "alpha rule", "beta rule")
	svc := guardService(store, &fixedEmbedder{vec: []float32{0.999, 0.02}})

	res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", false)
	if err != nil {
		t.Fatalf("same-model re-index refused: %v", err)
	}
	if res.Chunks != 2 {
		t.Errorf("chunks = %d, want 2", res.Chunks)
	}
}

// TestReindexOrg_WidthChangePassesWithoutForce: a corpus stranded at another
// width — nothing of it in the vector index at the deployment's width — is
// the recovery case the re-index button exists for, so no force is needed.
func TestReindexOrg_WidthChangePassesWithoutForce(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 3} // the width-2 corpus is stranded
	seedCorpus(store, []float32{1, 0}, "alpha rule", "beta rule")
	svc := guardService(store, &fixedEmbedder{vec: []float32{0, 0, 1}})

	if _, err := svc.ReindexOrg(context.Background(), "scope", "org-1", false); err != nil {
		t.Fatalf("width-change re-index refused: %v", err)
	}
	for id, v := range storedEmbeddings(store) {
		if len(v) != 3 {
			t.Errorf("chunk %s not re-embedded at the new width: %v", id, v)
		}
	}
}

// TestAddDocument_ModelChangedRefused pins #34/#35 on upload: a document
// embedded with another model would be unfindable for everyone on the
// corpus's model, so the upload is refused before the document is embedded
// and nothing is written.
func TestAddDocument_ModelChangedRefused(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedCorpus(store, []float32{1, 0}, "alpha rule", "beta rule")
	before := storedEmbeddings(store)
	emb := &fixedEmbedder{vec: []float32{0, 1}}
	svc := guardService(store, emb)

	err := svc.AddDocument(context.Background(), "scope", "org-1", "new.md", "a brand new guideline")
	if !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Fatalf("err = %v, want ErrEmbeddingModelChanged", err)
	}
	if len(store.KnowledgeDocs) != 1 || !sameEmbeddings(before, storedEmbeddings(store)) {
		t.Errorf("refused upload wrote to the store: %d docs, chunks %v", len(store.KnowledgeDocs), storedEmbeddings(store))
	}
	if emb.calls != 1 {
		t.Errorf("embed calls = %d, want 1 (the sample; the document is never embedded)", emb.calls)
	}

	// Same model: accepted. Another org's corpus is not consulted.
	ok := guardService(store, &fixedEmbedder{vec: []float32{1, 0}})
	if err := ok.AddDocument(context.Background(), "scope", "org-1", "new.md", "a brand new guideline"); err != nil {
		t.Errorf("same-model upload refused: %v", err)
	}
	if err := svc.AddDocument(context.Background(), "scope", "org-2", "new.md", "first document of another org"); err != nil {
		t.Errorf("upload into an empty org refused: %v", err)
	}
}

// TestCheckCorpusModel_WorstSameWidthPairDecides (the inverse of the former
// best-of rule): a partially re-indexed or mixed corpus is refused as soon
// as ONE comparable sample disagrees — passing on the one matching sample
// let a mixed corpus accept uploads from both models and never be repaired.
// Zero vectors are ignored (undefined similarity), so a sample of only
// zero vectors is corpusEmpty, and a sample with no same-width pair is
// corpusOtherWidth.
func TestCheckCorpusModel_WorstSameWidthPairDecides(t *testing.T) {
	store := &testutil.FakeBackend{}
	for _, d := range []string{"d1", "d2", "d3"} {
		_ = store.SaveKnowledgeDocument(context.Background(), &interfaces.KnowledgeDocument{ID: d, OrgID: "org-1"})
	}
	// One chunk per document so every one of them is sampled.
	_ = store.SaveKnowledgeChunks(context.Background(), "", []interfaces.KnowledgeChunk{
		{ID: "a", DocID: "d1", Content: "one", Embedding: []float32{0, 1}}, // other model
		{ID: "b", DocID: "d2", Content: "two", Embedding: []float32{1, 0}}, // caller's model
		{ID: "c", DocID: "d3", Content: "three", Embedding: []float32{0, 0}},
	})
	svc := guardService(store, nil)
	verdict, err := svc.checkCorpusModel(context.Background(), &fixedEmbedder{vec: []float32{1, 0}}, "org-1")
	if err != nil || verdict != corpusOtherModel {
		t.Errorf("mixed corpus: verdict = %v, %v; want corpusOtherModel (the worst pair decides)", verdict, err)
	}

	zeros := &testutil.FakeBackend{}
	_ = zeros.SaveKnowledgeDocument(context.Background(), &interfaces.KnowledgeDocument{ID: "d", OrgID: "org-1"})
	_ = zeros.SaveKnowledgeChunks(context.Background(), "", []interfaces.KnowledgeChunk{
		{ID: "a", DocID: "d", Content: "one", Embedding: []float32{0, 0}},
	})
	verdict, err = guardService(zeros, nil).checkCorpusModel(context.Background(), &fixedEmbedder{vec: []float32{1, 0}}, "org-1")
	if err != nil || verdict != corpusEmpty {
		t.Errorf("only an all-zero stored vector (undefined similarity): verdict = %v, %v; want corpusEmpty", verdict, err)
	}

	verdict, err = guardService(store, nil).checkCorpusModel(context.Background(), &fixedEmbedder{vec: []float32{0, 0, 1}}, "org-1")
	if err != nil || verdict != corpusOtherWidth {
		t.Errorf("no same-width pair: verdict = %v, %v; want corpusOtherWidth", verdict, err)
	}
	if verdict, err := guardService(&testutil.FakeBackend{}, nil).checkCorpusModel(context.Background(), &fixedEmbedder{vec: []float32{1, 0}}, "org-1"); err != nil || verdict != corpusEmpty {
		t.Errorf("empty corpus: verdict = %v, %v; want corpusEmpty", verdict, err)
	}
}

// TestCheckCorpusModel_MixedCorpusRefusesBothModels: with one sample per
// document and the worst pair deciding, a corpus half on each model refuses
// uploads from EITHER model and turns an unforced re-index into the force
// confirm — the only action that repairs the mix.
func TestCheckCorpusModel_MixedCorpusRefusesBothModels(t *testing.T) {
	newMixed := func() *testutil.FakeBackend {
		store := &testutil.FakeBackend{}
		_ = store.SaveKnowledgeDocument(context.Background(), &interfaces.KnowledgeDocument{ID: "doc-a", OrgID: "org-1", Filename: "a.md"})
		_ = store.SaveKnowledgeDocument(context.Background(), &interfaces.KnowledgeDocument{ID: "doc-b", OrgID: "org-1", Filename: "b.md"})
		_ = store.SaveKnowledgeChunks(context.Background(), "", []interfaces.KnowledgeChunk{
			{ID: "a1", DocID: "doc-a", Content: "alpha", Embedding: []float32{1, 0}},
			{ID: "a2", DocID: "doc-a", Content: "alpha two", Embedding: []float32{1, 0}},
			{ID: "b1", DocID: "doc-b", Content: "beta", Embedding: []float32{0, 1}},
			{ID: "b2", DocID: "doc-b", Content: "beta two", Embedding: []float32{0, 1}},
		})
		return store
	}
	for _, caller := range [][]float32{{1, 0}, {0, 1}} {
		store := newMixed()
		svc := guardService(store, &fixedEmbedder{vec: caller})
		if verdict, err := svc.checkCorpusModel(context.Background(), &fixedEmbedder{vec: caller}, "org-1"); err != nil || verdict != corpusOtherModel {
			t.Errorf("caller %v: verdict = %v, %v; want corpusOtherModel", caller, verdict, err)
		}
		if err := svc.AddDocument(context.Background(), "scope", "org-1", "c.md", "gamma"); !errors.Is(err, ErrEmbeddingModelChanged) {
			t.Errorf("caller %v: upload into a mixed corpus: err = %v, want ErrEmbeddingModelChanged", caller, err)
		}
		if _, err := svc.ReindexOrg(context.Background(), "scope", "org-1", false); !errors.Is(err, ErrEmbeddingModelChanged) {
			t.Errorf("caller %v: unforced re-index of a mixed corpus: err = %v, want ErrEmbeddingModelChanged", caller, err)
		}
		res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true)
		if err != nil || res.Chunks != 4 || res.Docs != 2 {
			t.Fatalf("caller %v: forced re-index = %+v, %v; want 4 chunks in 2 docs", caller, res, err)
		}
		for id, v := range storedEmbeddings(store) {
			if v[0] != caller[0] || v[1] != caller[1] {
				t.Errorf("caller %v: chunk %s = %v after the forced re-index", caller, id, v)
			}
		}
		if err := svc.AddDocument(context.Background(), "scope", "org-1", "c.md", "gamma"); err != nil {
			t.Errorf("caller %v: upload after the repair: %v", caller, err)
		}
	}
}

// TestAddDocument_WidthChangeRefused pins the width half of the guard on
// upload: an upload at another width than the indexed corpus is never a
// recovery — it always produces a mixed corpus — so it is refused before
// the document is embedded, whatever the vector index holds.
func TestAddDocument_WidthChangeRefused(t *testing.T) {
	for _, dim := range []int{0, 2, 3} {
		store := &testutil.FakeBackend{EmbeddingDim: dim}
		seedCorpus(store, []float32{1, 0}, "alpha rule", "beta rule")
		before := storedEmbeddings(store)
		emb := &fixedEmbedder{vec: []float32{0, 0, 1}}
		svc := guardService(store, emb)

		err := svc.AddDocument(context.Background(), "scope", "org-1", "new.md", "a brand new guideline")
		if !errors.Is(err, ErrEmbeddingModelChanged) {
			t.Fatalf("EmbeddingDim=%d: err = %v, want ErrEmbeddingModelChanged", dim, err)
		}
		if len(store.KnowledgeDocs) != 1 || !sameEmbeddings(before, storedEmbeddings(store)) {
			t.Errorf("EmbeddingDim=%d: refused upload wrote to the store", dim)
		}
		if emb.calls != 1 {
			t.Errorf("EmbeddingDim=%d: embed calls = %d, want 1 (the sample only)", dim, emb.calls)
		}
	}
}

// TestReindexOrg_WidthChangeRefusedWhenCorpusIndexed: a caller at another
// width than a corpus that IS in the vector index is switching the org's
// model, not recovering it — refused unforced like a same-width switch;
// force re-embeds everything at the caller's width.
func TestReindexOrg_WidthChangeRefusedWhenCorpusIndexed(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 2} // the width-2 corpus is indexed
	seedCorpus(store, []float32{1, 0}, "alpha rule", "beta rule")
	before := storedEmbeddings(store)
	emb := &fixedEmbedder{vec: []float32{0, 0, 1}}
	svc := guardService(store, emb)

	_, err := svc.ReindexOrg(context.Background(), "scope", "org-1", false)
	if !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Fatalf("unforced: err = %v, want ErrEmbeddingModelChanged", err)
	}
	if !sameEmbeddings(before, storedEmbeddings(store)) {
		t.Error("a refused re-index changed stored embeddings")
	}
	if emb.calls != 1 {
		t.Errorf("embed calls = %d, want 1 (the sample only)", emb.calls)
	}

	res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true)
	if err != nil || res.Chunks != 2 || res.Docs != 1 {
		t.Fatalf("forced: %+v, %v; want 2 chunks in 1 doc", res, err)
	}
	for id, v := range storedEmbeddings(store) {
		if len(v) != 3 || v[2] != 1 {
			t.Errorf("chunk %s = %v, want the caller's width-3 vector", id, v)
		}
	}
}

// TestAddDocument_ReplacingOnlyDocumentSkipsGuard: re-uploading the only
// document of a corpus after a model switch replaces it — nothing survives
// to be mixed with, and delete-then-upload reaches the same state unguarded
// — while the same caller is still refused when another document remains.
func TestAddDocument_ReplacingOnlyDocumentSkipsGuard(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedCorpus(store, []float32{1, 0}, "alpha rule", "beta rule")
	emb := &fixedEmbedder{vec: []float32{0, 1}}
	svc := guardService(store, emb)

	if err := svc.AddDocument(context.Background(), "scope", "org-1", "policy.md", "the rewritten policy"); err != nil {
		t.Fatalf("replacing the only document: %v", err)
	}
	docs, _ := store.ListKnowledgeDocuments(context.Background(), "org-1", 0)
	if len(docs) != 1 || docs[0].Filename != "policy.md" || docs[0].ID == "doc-1" {
		t.Errorf("docs after the replace = %+v, want exactly the new policy.md", docs)
	}
	for id, v := range storedEmbeddings(store) {
		if len(v) != 2 || v[0] != 0 || v[1] != 1 {
			t.Errorf("chunk %s = %v, want the caller's model [0 1]", id, v)
		}
	}
	if emb.calls != 1 {
		t.Errorf("embed calls = %d, want 1 (the document only; no sample was needed)", emb.calls)
	}

	// Another document survives the replace: the guard protects it.
	two := &testutil.FakeBackend{}
	seedCorpus(two, []float32{1, 0}, "alpha rule")
	_ = two.SaveKnowledgeDocument(context.Background(), &interfaces.KnowledgeDocument{ID: "doc-2", OrgID: "org-1", Filename: "other.md"})
	_ = two.SaveKnowledgeChunks(context.Background(), "", []interfaces.KnowledgeChunk{{ID: "chunk-o", DocID: "doc-2", Content: "other rule", Embedding: []float32{1, 0}}})
	before := storedEmbeddings(two)
	err := guardService(two, &fixedEmbedder{vec: []float32{0, 1}}).AddDocument(context.Background(), "scope", "org-1", "policy.md", "the rewritten policy")
	if !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Fatalf("replacing one of two documents: err = %v, want ErrEmbeddingModelChanged", err)
	}
	if len(two.KnowledgeDocs) != 2 || !sameEmbeddings(before, storedEmbeddings(two)) {
		t.Error("the refused replace wrote to the store")
	}
}

// azureDoc is a valid Azure AI Foundry credential document (validation only;
// nothing here dials it).
func azureDoc(embeddingDeployment string) string {
	doc := `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"gpt-4o"`
	if embeddingDeployment != "" {
		doc += `,"embeddingDeployment":"` + embeddingDeployment + `"`
	}
	return doc + `}`
}

// newKeysFactory is a ProviderFactory whose key store is the given map
// (provider key name → stored key or document).
func newKeysFactory(keys map[string]string) *ai.ProviderFactory {
	return ai.NewProviderFactory(func(_ string, provider string) (string, error) {
		return keys[provider], nil
	}, nil, nil, nil)
}

// TestEmbedder_AzureNeverWithoutOperatorOptIn pins #34: with default settings
// a member whose only embedding-capable configuration is Azure AI Foundry
// never embeds the shared corpus with their own deployment — not through the
// fallback and not through the settings value alone.
func TestEmbedder_AzureNeverWithoutOperatorOptIn(t *testing.T) {
	ai.ResetAzureFoundryCaches()
	keys := map[string]string{"claude": "k", "azure-foundry": azureDoc("text-embedding-ada-002")}

	svc := NewKnowledgeService(nil, newKeysFactory(keys), stubSettings{&models.AppSettings{}})
	if p, err := svc.embedder("scope", ""); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Errorf("default settings: embedder = %v, %v; want ErrEmbeddingUnavailable (Azure is opt-in)", p, err)
	}

	settings := &models.AppSettings{}
	settings.AI.EmbeddingProvider = "azure-foundry"
	svc = NewKnowledgeService(nil, newKeysFactory(keys), stubSettings{settings})
	_, err := svc.embedder("scope", "")
	if !errors.Is(err, ErrEmbeddingUnavailable) || !strings.Contains(err.Error(), "PAD_EMBEDDING_PROVIDER") {
		t.Errorf("settings alone: err = %v, want ErrEmbeddingUnavailable naming PAD_EMBEDDING_PROVIDER", err)
	}

	// Another keyed embedder still serves the member; Azure is skipped.
	keys["glm"] = "k"
	svc = NewKnowledgeService(nil, newKeysFactory(keys), stubSettings{&models.AppSettings{}})
	p, err := svc.embedder("scope", "")
	if err != nil || p.ID() != "glm" {
		t.Errorf("fallback = %v, %v; want glm, never azure-foundry", p, err)
	}
}

// TestEmbedder_AzureOperatorOptIn: PAD_EMBEDDING_PROVIDER=azure-foundry makes
// Azure the knowledge-base embedder (and wins over the settings value).
func TestEmbedder_AzureOperatorOptIn(t *testing.T) {
	ai.ResetAzureFoundryCaches()
	keys := map[string]string{"openai": "k", "azure-foundry": azureDoc("kb-embed")}
	settings := &models.AppSettings{}
	settings.AI.EmbeddingProvider = "openai"
	svc := NewKnowledgeService(nil, newKeysFactory(keys), stubSettings{settings}, WithEmbeddingProvider("azure-foundry"))

	p, err := svc.embedder("scope", "")
	if err != nil || p.ID() != "azure-foundry" {
		t.Fatalf("opt-in: embedder = %v, %v; want azure-foundry", p, err)
	}

	// The pin wins over the settings value for any provider.
	keys["glm"] = "k"
	svc = NewKnowledgeService(nil, newKeysFactory(keys), stubSettings{settings}, WithEmbeddingProvider("glm"))
	if p, err := svc.embedder("scope", ""); err != nil || p.ID() != "glm" {
		t.Errorf("pinned glm: embedder = %v, %v; want glm", p, err)
	}
}

// TestEmbedder_AzureWithoutEmbeddingDeployment pins #16: an opted-in Azure
// configuration that names no embedding deployment is not an embedder — the
// caller gets the machine-readable ErrEmbeddingUnavailable (HTTP 400
// EMBEDDING_NOT_CONFIGURED) instead of a masked 500 at Embed time, or the
// next keyed provider.
func TestEmbedder_AzureWithoutEmbeddingDeployment(t *testing.T) {
	ai.ResetAzureFoundryCaches()
	keys := map[string]string{"claude": "k", "azure-foundry": azureDoc("")}
	svc := NewKnowledgeService(&testutil.FakeBackend{}, newKeysFactory(keys), stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("azure-foundry"))

	_, err := svc.embedder("scope", "")
	if !errors.Is(err, ErrEmbeddingUnavailable) || !errors.Is(err, ai.ErrEmbeddingDeploymentMissing) {
		t.Errorf("err = %v, want ErrEmbeddingUnavailable wrapping ai.ErrEmbeddingDeploymentMissing", err)
	}
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "a.md", "content"); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Errorf("AddDocument err = %v, want ErrEmbeddingUnavailable (→ 400 EMBEDDING_NOT_CONFIGURED)", err)
	}

	// A deployment-wide embedding-model override counts as a deployment.
	settings := &models.AppSettings{}
	settings.AI.EmbeddingModel = "shared-embed"
	svc = NewKnowledgeService(nil, newKeysFactory(keys), stubSettings{settings}, WithEmbeddingProvider("azure-foundry"))
	if p, err := svc.embedder("scope", ""); err != nil || p.ID() != "azure-foundry" {
		t.Errorf("with the model override: embedder = %v, %v; want azure-foundry", p, err)
	}

	// Another keyed embedder takes over when Azure cannot embed.
	keys["gemini"] = "k"
	svc = NewKnowledgeService(nil, newKeysFactory(keys), stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("azure-foundry"))
	if p, err := svc.embedder("scope", ""); err != nil || p.ID() != "gemini" {
		t.Errorf("fallback past a chat-only Azure document = %v, %v; want gemini", p, err)
	}
}
