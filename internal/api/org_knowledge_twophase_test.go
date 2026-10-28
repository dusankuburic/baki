package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// downAfterEmbedder answers its first `ok` calls with vec and then fails as an outage.
type downAfterEmbedder struct {
	ai.Provider
	ok    int32
	calls atomic.Int32
	vec   []float32
}

func (d *downAfterEmbedder) ID() string { return "down-after-embed" }
func (d *downAfterEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	if d.calls.Add(1) > d.ok {
		return nil, fmt.Errorf("embeddings: %w", ai.ErrProviderDown)
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = append([]float32(nil), d.vec...)
	}
	return out, nil
}

// knowledgeRouterWith serves the knowledge routes of org (its admin the local-mode caller) over store, embedding with emb.
func knowledgeRouterWith(t *testing.T, orgSvc *collaboration.OrgService, store *auditRecordingBackend, emb ai.Provider) http.Handler {
	t.Helper()
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(emb))
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	return mountedAs(h, "u-admin")
}

// paragraphs is a plain-text document chunker v2 cuts into exactly n chunks.
func paragraphs(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("Paragraph %05d. ", i) + strings.Repeat("w", 900)
	}
	return strings.Join(out, "\n\n")
}

type uploadAnswer struct {
	ID             string                          `json:"id"`
	Filename       string                          `json:"filename"`
	State          string                          `json:"state"`
	ChunkerVersion int                             `json:"chunkerVersion"`
	Chunks         int                             `json:"chunks"`
	Embedded       int                             `json:"embedded"`
	PendingChunks  int                             `json:"pendingChunks"`
	Truncated      bool                            `json:"truncated"`
	NextCursor     *storageif.KnowledgeChunkCursor `json:"nextCursor"`
}

// TestKnowledgeUpload_TwoPhaseOverHTTP (R-knowledge-2): an upload answers 201 with the document; one the provider
// fails mid-way answers the provider's code with the stored document's id, what is left and the cursor in the
// details; posting its id back to the upload route continues it (200) and turns it ready; an unknown document is 404, a
// cursor naming another document 400; the listing reports state and pendingChunks; each request is audited with
// the document's id.
func TestKnowledgeUpload_TwoPhaseOverHTTP(t *testing.T) {
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatal(err)
	}
	store := &auditRecordingBackend{FakeBackend: &testutil.FakeBackend{}}
	store.SeedOrgs(org.ID)
	base := "/api/orgs/" + org.ID + "/knowledge"

	small := postJSON(t, knowledgeRouterWith(t, orgSvc, store, directionEmbedder{vec: []float32{1, 0}}), base+"/upload", `{"filename":"small.md","content":"# Rules\n\nRetry every call."}`)
	var done uploadAnswer
	if small.Code != http.StatusCreated || json.Unmarshal(small.Body.Bytes(), &done) != nil || done.ID == "" || done.State != "ready" || done.Chunks != 1 || done.PendingChunks != 0 || done.ChunkerVersion != 2 || done.NextCursor != nil {
		t.Fatalf("small upload: %d %s; want 201 with the ready document", small.Code, small.Body.String())
	}
	if e := store.lastEvent(t, audit.ActionKnowledgeUpload); e.Meta["docId"] != done.ID || e.Meta["filename"] != "small.md" {
		t.Errorf("audited upload = %v; want its document id and name", e.Meta)
	}

	// The provider goes down after the guard sample and the first batch: the document stays, pending, with what it bought.
	failing := knowledgeRouterWith(t, orgSvc, store, &downAfterEmbedder{ok: 2, vec: []float32{1, 0}})
	rr := postJSON(t, failing, base+"/upload", `{"filename":"big.txt","content":`+jsonQuote(paragraphs(250))+`}`)
	env := errorEnvelope(t, rr)
	if rr.Code != http.StatusServiceUnavailable || env.Code != "EMBEDDING_PROVIDER_UNAVAILABLE" {
		t.Fatalf("failing upload: %d %s; want 503 EMBEDDING_PROVIDER_UNAVAILABLE", rr.Code, rr.Body.String())
	}
	var details struct {
		ID            string                          `json:"id"`
		PendingChunks int                             `json:"pendingChunks"`
		NextCursor    *storageif.KnowledgeChunkCursor `json:"nextCursor"`
	}
	raw, _ := json.Marshal(env.Details)
	if err := json.Unmarshal(raw, &details); err != nil || details.ID == "" || details.PendingChunks != 150 || details.NextCursor == nil || details.NextCursor.DocID != details.ID {
		t.Fatalf("failing upload's details = %s; want the document id, 150 chunks left and its cursor", raw)
	}

	var listed []map[string]any
	if rr := getPath(failing, base); rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &listed) != nil {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	found := false
	for _, d := range listed {
		if d["id"] == details.ID {
			found = true
			if d["state"] != "pending" || d["pendingChunks"] != float64(150) || d["chunkerVersion"] != float64(2) || d["hasSource"] != true {
				t.Errorf("listed upload = %v; want state pending, pendingChunks 150, chunkerVersion 2, hasSource", d)
			}
		}
	}
	if !found {
		t.Fatalf("the pending upload is not listed: %v", listed)
	}

	working := knowledgeRouterWith(t, orgSvc, store, directionEmbedder{vec: []float32{1, 0}})
	if rr := postJSON(t, working, base+"/upload", `{"docId":"no-such-doc"}`); rr.Code != http.StatusNotFound || errorCode(t, rr) != "KNOWLEDGE_DOCUMENT_NOT_FOUND" {
		t.Errorf("continue an unknown document: %d %s; want 404 KNOWLEDGE_DOCUMENT_NOT_FOUND", rr.Code, rr.Body.String())
	}
	if rr := postJSON(t, working, base+"/upload", `{"docId":"`+details.ID+`","cursor":{"docId":"another","id":"x"}}`); rr.Code != http.StatusBadRequest {
		t.Errorf("continue with another document's cursor: %d %s; want 400", rr.Code, rr.Body.String())
	}
	if rr := postJSON(t, working, base+"/upload", `{"docId":"`+details.ID+`","filename":"big.txt"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("continue with a document beside it: %d %s; want 400", rr.Code, rr.Body.String())
	}
	cursor, _ := json.Marshal(details.NextCursor)
	rr = postJSON(t, working, base+"/upload", `{"docId":"`+details.ID+`","cursor":`+string(cursor)+`}`)
	var cont uploadAnswer
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &cont) != nil || cont.ID != details.ID || cont.State != "ready" || cont.Embedded != 150 || cont.PendingChunks != 0 || cont.NextCursor != nil {
		t.Fatalf("continue: %d %s; want 200 with the rest embedded and the document ready", rr.Code, rr.Body.String())
	}
	// The recorder writes asynchronously (slowly under a loaded full-suite run): wait for the continuation's row, not
	// the failed upload's.
	var continued *storageif.AuditEvent
	for deadline := time.Now().Add(10 * time.Second); continued == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if e := store.lastEvent(t, audit.ActionKnowledgeUpload); e.Meta["continued"] == "true" {
			continued = e
		}
	}
	if continued == nil || continued.Meta["docId"] != details.ID || continued.Meta["failed"] != "" {
		t.Errorf("audited continuation = %+v; want a row with its document id and continued", continued)
	}
	// Nothing left: the route answers the ready document again.
	if rr := postJSON(t, working, base+"/upload", `{"docId":"`+details.ID+`"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"embedded":0`) {
		t.Errorf("continue a ready document: %d %s", rr.Code, rr.Body.String())
	}
}

// TestKnowledgeReindex_RechunkBody: {"rechunk": true} runs the rechunk re-index; it takes neither force nor background.
func TestKnowledgeReindex_RechunkBody(t *testing.T) {
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatal(err)
	}
	store := &auditRecordingBackend{FakeBackend: &testutil.FakeBackend{}}
	store.SeedOrgs(org.ID)
	const source = "# Margins\n\nKeep a 3.5 mm margin in flow.pad."
	if err := store.SaveKnowledgeDocumentWithChunks(bgCtx(), "", &storageif.KnowledgeDocument{ID: "doc-v1", OrgID: org.ID, Filename: "margins.md", ChunkerVersion: 1, Content: source},
		[]storageif.KnowledgeChunk{{ID: "c1", DocID: "doc-v1", Content: source, Embedding: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	r := knowledgeRouterWith(t, orgSvc, store, directionEmbedder{vec: []float32{1, 0}})
	path := "/api/orgs/" + org.ID + "/knowledge/reindex"
	for _, body := range []string{`{"rechunk":true,"force":true}`, `{"rechunk":true,"background":true}`} {
		if rr := postJSON(t, r, path, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", body, rr.Code, rr.Body.String())
		}
	}
	rr := postJSON(t, r, path, `{"rechunk":true}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"rechunked":1`) {
		t.Fatalf("rechunk: %d %s; want 200 with one document rechunked", rr.Code, rr.Body.String())
	}
	if e := store.lastEvent(t, audit.ActionKnowledgeReindex); e.Meta["rechunk"] != "true" {
		t.Errorf("audited rechunk = %v; want rechunk true", e.Meta)
	}
	docs, _ := store.ListKnowledgeDocuments(bgCtx(), org.ID, 0, 0)
	if len(docs) != 1 || docs[0].ChunkerVersion != 2 || docs[0].Filename != "margins.md" {
		t.Errorf("documents after the rechunk = %+v; want margins.md at version 2", docs)
	}
}

// jsonQuote is s as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// getPath serves a GET of path.
func getPath(h http.Handler, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}
