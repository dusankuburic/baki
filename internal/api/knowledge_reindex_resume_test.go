package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// TestKnowledgeReindex_CursorRoundTrip pins the resumable re-index at the
// HTTP layer (R-7): a corpus one chunk above the per-request ceiling answers
// {chunks, docs, truncated, nextCursor:{docId,id}}; posting that cursor back
// (the settings panel's loop: {force, cursor: nextCursor}) converts the
// remainder and answers without a cursor; each request writes its own audit
// row, the resumed one marked "resumed".
func TestKnowledgeReindex_CursorRoundTrip(t *testing.T) {
	prevCh := auditCh
	auditCh = nil
	t.Cleanup(func() { auditCh = prevCh })

	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &auditRecordingBackend{FakeBackend: &testutil.FakeBackend{}}
	const docs, ceiling = 2, 10000
	for d := range docs {
		_ = store.SaveKnowledgeDocument(bgCtx(), &storageif.KnowledgeDocument{ID: fmt.Sprintf("doc-%d", d), OrgID: org.ID, Filename: fmt.Sprintf("doc-%d.md", d)})
	}
	chunks := make([]storageif.KnowledgeChunk, ceiling+1)
	for i := range chunks {
		chunks[i] = storageif.KnowledgeChunk{ID: fmt.Sprintf("c-%06d", i), DocID: fmt.Sprintf("doc-%d", i%docs), Content: fmt.Sprintf("rule %d", i), Embedding: []float32{1, 0}}
	}
	_ = store.SaveKnowledgeChunks(bgCtx(), "", chunks)
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(directionEmbedder{vec: []float32{0, 1}}))
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, config.ModeLocal), nil, nil)
	r := chi.NewRouter()
	r.Post("/api/orgs/{id}/knowledge/reindex", h.handleKnowledgeReindex)
	path := "/api/orgs/" + org.ID + "/knowledge/reindex"

	// First request: the model differs, so it takes force.
	rr := postJSON(t, r, path, `{"force":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var first struct {
		Chunks     int             `json:"chunks"`
		Truncated  bool            `json:"truncated"`
		NextCursor json.RawMessage `json:"nextCursor"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Walk order is (doc_id, id): doc-0 holds the even ids, 5001 of them,
	// so the 10 000th chunk is doc-1's last but one.
	wantCursor := `{"docId":"doc-1","id":"c-009997"}`
	if first.Chunks != ceiling || !first.Truncated || string(first.NextCursor) != wantCursor {
		t.Fatalf("first answer = %s, want %d chunks, truncated and nextCursor %s", rr.Body.String(), ceiling, wantCursor)
	}

	// Resume with the cursor exactly as received; force travels along as
	// the panel sends it (the resumed guard ignores it for the same model).
	rr = postJSON(t, r, path, `{"force":true,"cursor":`+string(first.NextCursor)+`}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("resumed request: status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var second map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second["chunks"] != float64(1) || second["nextCursor"] != nil || second["truncated"] != nil {
		t.Errorf("resumed answer = %s, want the 1 remaining chunk and no cursor", rr.Body.String())
	}
	for id, c := range store.KnowledgeChunks {
		if c.Embedding[0] != 0 || c.Embedding[1] != 1 {
			t.Fatalf("chunk %s = %v after the resumed run, want the caller's model", id, c.Embedding)
		}
	}

	store.lastEvent(t, AuditActionKnowledgeReindex)
	deadline := time.Now().Add(3 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.events)
		store.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.events) != 2 {
		t.Fatalf("%d audit events, want one per request (2)", len(store.events))
	}
	if m := store.events[0].Meta; m["truncated"] != "true" || m["resumed"] != "" {
		t.Errorf("first audit meta = %v, want truncated and not resumed", m)
	}
	if m := store.events[1].Meta; m["resumed"] != "true" || m["truncated"] != "" {
		t.Errorf("resumed audit meta = %v, want resumed and not truncated", m)
	}
}

// TestKnowledgeReindex_BodyIsStrict: the body is optional ({} / empty /
// a null or empty cursor start from the top), but unknown fields, trailing
// data, a half-filled cursor and oversized or control-character components
// are a 400 before any embedding is bought.
func TestKnowledgeReindex_BodyIsStrict(t *testing.T) {
	h, store, orgID := newKnowledgeGuardRouter(t, []float32{1, 0}) // same model: no force needed
	path := "/api/orgs/" + orgID + "/knowledge/reindex"
	for _, body := range []string{"", "{}", `{"cursor":null}`, `{"cursor":{}}`, `{"cursor":{"docId":"","id":""}}`, `{"force":false}`} {
		if rr := postJSON(t, h, path, body); rr.Code != http.StatusOK {
			t.Errorf("body %q: status = %d (%s), want 200", body, rr.Code, rr.Body.String())
		}
	}
	long := strings.Repeat("x", maxReindexCursorField+1)
	for _, body := range []string{
		`{"force":true,"bogus":1}`,
		`{"cursor":{"docId":"doc-1","id":"c1","extra":1}}`,
		`{"cursor":{"docId":"doc-1"}}`,
		`{"cursor":{"id":"c1"}}`,
		`{"cursor":{"docId":"` + long + `","id":"c1"}}`,
		`{"cursor":{"docId":"doc-1","id":"c\u0000"}}`,
		`{"cursor":"doc-1/c1"}`,
		`{} {}`,
		`{"force":`,
	} {
		before := store.KnowledgeChunks["c1"].Embedding
		if rr := postJSON(t, h, path, body); rr.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d (%s), want 400", body, rr.Code, rr.Body.String())
		}
		if after := store.KnowledgeChunks["c1"].Embedding; &after[0] != &before[0] {
			t.Errorf("body %q: a refused request re-embedded the corpus", body)
		}
	}
	// A well-formed cursor resumes after it: c1 converted, only c2 walked.
	rr := postJSON(t, h, path, `{"cursor":{"docId":"doc-1","id":"c1"}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"chunks":1`) {
		t.Errorf("resume after c1: status = %d, body = %s; want 1 chunk", rr.Code, rr.Body.String())
	}
}

// deadlineEmbedder blocks until the request context is done and then fails
// the way a provider may report a cut-off call — as an outage, not as the
// context's error.
type deadlineEmbedder struct{ ai.Provider }

func (deadlineEmbedder) ID() string { return "deadline-embed" }
func (deadlineEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("embeddings: %w", ai.ErrProviderDown)
}

// TestKnowledge_DeadlineIs504: a knowledge request whose deadline runs out
// before anything committed answers 504 KNOWLEDGE_TIMEOUT — before the
// provider ladder, which would read the cut-off call as an outage (503) —
// on both bulk routes.
func TestKnowledge_DeadlineIs504(t *testing.T) {
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &testutil.FakeBackend{}
	_ = store.SaveKnowledgeDocument(bgCtx(), &storageif.KnowledgeDocument{ID: "doc-1", OrgID: org.ID, Filename: "policy.md"})
	_ = store.SaveKnowledgeChunks(bgCtx(), "", []storageif.KnowledgeChunk{{ID: "c1", DocID: "doc-1", Content: "retry every call", Embedding: []float32{1, 0}}})
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(deadlineEmbedder{}))
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, config.ModeLocal), nil, nil)
	r := chi.NewRouter()
	r.Post("/api/orgs/{id}/knowledge/upload", h.handleKnowledgeUpload)
	r.Post("/api/orgs/{id}/knowledge/reindex", h.handleKnowledgeReindex)

	for _, route := range []struct{ path, body string }{
		{"/api/orgs/" + org.ID + "/knowledge/reindex", `{"force":true}`},
		{"/api/orgs/" + org.ID + "/knowledge/upload", `{"filename":"new.md","content":"a new rule"}`},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		req := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body)).WithContext(ctx)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		cancel()
		if rr.Code != http.StatusGatewayTimeout || errorCode(t, rr) != "KNOWLEDGE_TIMEOUT" {
			t.Errorf("%s: status = %d, body = %s; want 504 KNOWLEDGE_TIMEOUT", route.path, rr.Code, rr.Body.String())
		}
	}
}

// TestBeginFinishedBody_CarriesServedModelAndRates (R-3): the begin answer
// for an already-finished stream hands the client the served model and the
// billed rates it missed with the done event, and omits them when the turn
// reported none.
func TestBeginFinishedBody_CarriesServedModelAndRates(t *testing.T) {
	raw, _ := json.Marshal(beginFinishedBody(&service.ResumeResult{
		Text: "answer", Done: true, TokensIn: 7, TokensOut: 3,
		ServedModel: "gpt-4o-2024-11-20", InputCostPerM: 2.5, OutputCostPerM: 10,
	}))
	for _, want := range []string{`"status":"finished"`, `"servedModel":"gpt-4o-2024-11-20"`, `"inputCostPerM":2.5`, `"outputCostPerM":10`, `"tokensIn":7`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("begin body %s lacks %s", raw, want)
		}
	}
	raw, _ = json.Marshal(beginFinishedBody(&service.ResumeResult{Error: "boom", ErrorCode: "PROVIDER_AUTH"}))
	if strings.Contains(string(raw), "servedModel") || strings.Contains(string(raw), "CostPerM") {
		t.Errorf("unreported fields must be omitted: %s", raw)
	}
}

// TestRenderKnowledgeEmbeddingError_ContextErrors: the ladder itself maps
// the context's errors to 504 KNOWLEDGE_TIMEOUT ahead of the provider
// causes they may be joined with, and keeps the budget codes first.
func TestRenderKnowledgeEmbeddingError_ContextErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"deadline", fmt.Errorf("%w: %w", rag.ErrEmbeddingFailed, context.DeadlineExceeded), http.StatusGatewayTimeout, "KNOWLEDGE_TIMEOUT"},
		{"canceled", context.Canceled, http.StatusGatewayTimeout, "KNOWLEDGE_TIMEOUT"},
		{"joined with outage", errors.Join(fmt.Errorf("%w: %w", rag.ErrEmbeddingFailed, ai.ErrProviderDown), context.DeadlineExceeded), http.StatusGatewayTimeout, "KNOWLEDGE_TIMEOUT"},
		{"joined with rate limit", errors.Join(fmt.Errorf("%w: %w", rag.ErrEmbeddingFailed, &ai.RateLimitError{}), context.DeadlineExceeded), http.StatusGatewayTimeout, "KNOWLEDGE_TIMEOUT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			if !renderKnowledgeEmbeddingError(rr, tc.err) {
				t.Fatalf("not rendered: %v", tc.err)
			}
			if rr.Code != tc.status || errorCode(t, rr) != tc.code {
				t.Errorf("status = %d code = %q, want %d %s", rr.Code, errorCode(t, rr), tc.status, tc.code)
			}
		})
	}
	// withRequestContextErr folds a done request context in, and leaves a
	// live one alone.
	live := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := withRequestContextErr(live, rag.ErrKnowledgeBusy); !errors.Is(err, rag.ErrKnowledgeBusy) || err.Error() != rag.ErrKnowledgeBusy.Error() {
		t.Errorf("live request: err = %v, want it unchanged", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := live.WithContext(ctx)
	if err := withRequestContextErr(done, ai.ErrProviderDown); !errors.Is(err, context.Canceled) || !errors.Is(err, ai.ErrProviderDown) {
		t.Errorf("done request: err = %v, want both causes", err)
	}
}
