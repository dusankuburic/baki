package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// newKnowledgeWidthRouter serves the knowledge routes of an org whose corpus holds two chunks of width len(vec)
// under identity "model", for a caller embedding as vec under the same identity, on a deployment whose
// embedding width (PAD_EMBEDDING_DIM) is configured; noPgvector models a store without a vector index.
func newKnowledgeWidthRouter(t *testing.T, configured int, vec []float32, noPgvector bool) (http.Handler, string) {
	t.Helper()
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &auditRecordingBackend{FakeBackend: &testutil.FakeBackend{EmbeddingDim: configured, NoPgvector: noPgvector}}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	_ = store.SaveKnowledgeDocument(bgCtx(), "", &storageif.KnowledgeDocument{ID: "doc-1", OrgID: org.ID, Filename: "policy.md"})
	_ = store.SaveKnowledgeChunks(bgCtx(), "", []storageif.KnowledgeChunk{
		{ID: "c1", DocID: "doc-1", Content: "retry every call", Embedding: vec, EmbeddingIdentity: "model"},
		{ID: "c2", DocID: "doc-1", Content: "name flows by team", Embedding: vec, EmbeddingIdentity: "model"},
	})
	factory := ai.NewProviderFactory(nil, nil, nil, nil)
	factory.SetEmbeddingDimensions(configured)
	emb := identityDirectionEmbedder{directionEmbedder: directionEmbedder{vec: vec}, identity: "model"}
	knowledge := rag.NewKnowledgeService(store, factory, nil, rag.WithEmbedder(emb))
	t.Cleanup(knowledge.StopJobs)
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	return mountedAs(h, "u-admin"), org.ID
}

// listKnowledge GETs the org's knowledge listing and decodes its one document.
func listKnowledge(t *testing.T, h http.Handler, orgID string) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/orgs/"+orgID+"/knowledge", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var docs []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &docs); err != nil || len(docs) != 1 {
		t.Fatalf("body = %s (%v), want one document", rr.Body.String(), err)
	}
	return docs[0]
}

// TestKnowledgeList_ReportsEmbeddingWidthMismatch (R-config-8): a document whose chunks are outside the vector
// index because their width is not PAD_EMBEDDING_DIM carries both widths and which side is off in the listing,
// so the admin can see which to change; a document at the configured width carries nothing.
func TestKnowledgeList_ReportsEmbeddingWidthMismatch(t *testing.T) {
	h, orgID := newKnowledgeWidthRouter(t, 2, []float32{1, 0, 0}, false)
	doc := listKnowledge(t, h, orgID)
	if doc["chunkCount"] != float64(2) || doc["vectorIndexed"] != nil {
		t.Errorf("document = %v, want chunkCount 2 and no vectorIndexed", doc)
	}
	mismatch, _ := doc["embeddingWidthMismatch"].(map[string]any)
	if mismatch["configured"] != float64(2) || mismatch["observed"] != float64(3) || mismatch["source"] != "stored" {
		t.Errorf("document = %v, want embeddingWidthMismatch {configured: 2, observed: 3, source: stored}", doc)
	}
	if _, present := doc["noVectorIndex"]; present {
		t.Errorf("document = %v, want no noVectorIndex on a deployment with a vector index", doc)
	}

	same, sameOrg := newKnowledgeWidthRouter(t, 2, []float32{1, 0}, false)
	fits := listKnowledge(t, same, sameOrg)
	if _, present := fits["embeddingWidthMismatch"]; present || fits["vectorIndexed"] != float64(2) {
		t.Errorf("document at the configured width = %v, want vectorIndexed 2 and no embeddingWidthMismatch", fits)
	}
}

// TestKnowledgeList_NoVectorIndex (R-config-8): on a deployment without pgvector no chunk is ever in a vector
// index, which says nothing about searchability (the in-process search finds them at the embedder's width), so the
// listing carries no width report and says the deployment has no index, for the panel to leave "Not searchable" out.
func TestKnowledgeList_NoVectorIndex(t *testing.T) {
	h, orgID := newKnowledgeWidthRouter(t, 2, []float32{1, 0, 0}, true)
	doc := listKnowledge(t, h, orgID)
	if doc["chunkCount"] != float64(2) || doc["vectorIndexed"] != nil {
		t.Errorf("document = %v, want chunkCount 2 and no vectorIndexed", doc)
	}
	if _, present := doc["embeddingWidthMismatch"]; present {
		t.Errorf("document = %v, want no embeddingWidthMismatch without a vector index", doc)
	}
	if doc["noVectorIndex"] != true {
		t.Errorf("document = %v, want noVectorIndex true", doc)
	}
}

// TestKnowledgeReindex_EmbedderOffWidth409UnlessForced (R-config-8): a re-index whose embedder is not
// PAD_EMBEDDING_DIM wide cannot make a chunk searchable, so it answers 409 EMBEDDING_WIDTH_OFF with both widths and
// the remedy in the message; {"force": true} runs it, and so does an unforced request on a deployment without a
// vector index, where nothing is outside anything.
func TestKnowledgeReindex_EmbedderOffWidth409UnlessForced(t *testing.T) {
	h, orgID := newKnowledgeWidthRouter(t, 2, []float32{1, 0, 0}, false)
	path := "/api/orgs/" + orgID + "/knowledge/reindex"
	rr := postJSON(t, h, path, "{}")
	if rr.Code != http.StatusConflict || errorCode(t, rr) != "EMBEDDING_WIDTH_OFF" {
		t.Fatalf("status = %d, body = %s; want 409 EMBEDDING_WIDTH_OFF", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "PAD_EMBEDDING_DIM is 2") || !strings.Contains(body, "set PAD_EMBEDDING_DIM to 3") {
		t.Errorf("the refusal does not name both widths and the remedy: %s", body)
	}
	if rr := postJSON(t, h, path, `{"force":true}`); rr.Code != http.StatusOK {
		t.Errorf("forced re-index: status = %d, body = %s", rr.Code, rr.Body.String())
	}

	plain, plainOrg := newKnowledgeWidthRouter(t, 2, []float32{1, 0, 0}, true)
	if rr := postJSON(t, plain, "/api/orgs/"+plainOrg+"/knowledge/reindex", "{}"); rr.Code != http.StatusOK {
		t.Errorf("unforced re-index without a vector index: status = %d, body = %s", rr.Code, rr.Body.String())
	}
}
