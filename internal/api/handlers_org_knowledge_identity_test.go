package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// identityDirectionEmbedder is directionEmbedder with an embedding identity.
type identityDirectionEmbedder struct {
	directionEmbedder
	identity string
}

func (e identityDirectionEmbedder) EmbeddingIdentity() string { return e.identity }

// newKnowledgeIdentityRouter serves the knowledge routes for an org whose admin is the caller and whose corpus
// holds two chunks at [1 0] under identity corpusIdentity; the caller embeds as callerVec under callerIdentity.
func newKnowledgeIdentityRouter(t *testing.T, corpusIdentity string, callerIdentity string, callerVec []float32) (http.Handler, *auditRecordingBackend, string) {
	t.Helper()
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &auditRecordingBackend{FakeBackend: &testutil.FakeBackend{EmbeddingDim: 2}}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	_ = store.SaveKnowledgeDocument(bgCtx(), "", &storageif.KnowledgeDocument{ID: "doc-1", OrgID: org.ID, Filename: "policy.md"})
	_ = store.SaveKnowledgeChunks(bgCtx(), "", []storageif.KnowledgeChunk{
		{ID: "c1", DocID: "doc-1", Content: "retry every call", Embedding: []float32{1, 0}, EmbeddingIdentity: corpusIdentity},
		{ID: "c2", DocID: "doc-1", Content: "name flows by team", Embedding: []float32{1, 0}, EmbeddingIdentity: corpusIdentity},
	})
	emb := identityDirectionEmbedder{directionEmbedder: directionEmbedder{vec: callerVec}, identity: callerIdentity}
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(emb))
	t.Cleanup(knowledge.StopJobs)
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	return mountedAs(h, "u-admin"), store, org.ID
}

// TestKnowledgeList_CarriesEmbeddingStranded: the document list tells the admin how many of a document's chunks
// were indexed under another embedding model than theirs (additive field; absent when none are).
func TestKnowledgeList_CarriesEmbeddingStranded(t *testing.T) {
	h, _, orgID := newKnowledgeIdentityRouter(t, "other-model", "my-model", []float32{1, 0})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/orgs/"+orgID+"/knowledge", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var docs []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &docs); err != nil || len(docs) != 1 {
		t.Fatalf("body = %s (%v), want one document", rr.Body.String(), err)
	}
	if docs[0]["embeddingStranded"] != float64(2) || docs[0]["chunkCount"] != float64(2) {
		t.Errorf("document = %v, want embeddingStranded 2 of chunkCount 2", docs[0])
	}

	same, _, sameOrg := newKnowledgeIdentityRouter(t, "my-model", "my-model", []float32{1, 0})
	rr = httptest.NewRecorder()
	same.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/orgs/"+sameOrg+"/knowledge", nil))
	var current []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &current)
	if len(current) != 1 {
		t.Fatalf("body = %s, want one document", rr.Body.String())
	}
	if _, present := current[0]["embeddingStranded"]; present {
		t.Errorf("document at the caller's model carries embeddingStranded: %v", current[0])
	}
}

// TestKnowledgeReindex_BackgroundAnswers202AndRunsJob: {"background": true} starts the knowledge_reembed job and
// answers 202 with its name at once; the job converts the corpus to the caller's model and the audit row names
// the background start. A cursor cannot be combined with it (the job resumes on its own).
func TestKnowledgeReindex_BackgroundAnswers202AndRunsJob(t *testing.T) {
	h, store, orgID := newKnowledgeIdentityRouter(t, "other-model", "my-model", []float32{0, 1})
	path := "/api/orgs/" + orgID + "/knowledge/reindex"

	rr := postJSON(t, h, path, `{"background":true,"cursor":{"docId":"doc-1","id":"c1"}}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("background with a cursor: status = %d, want 400", rr.Code)
	}

	rr = postJSON(t, h, path, `{"background":true,"force":true}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s; want 202", rr.Code, rr.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body["job"] != "knowledge_reembed" || body["status"] != "accepted" {
		t.Errorf("body = %s (%v), want {status: accepted, job: knowledge_reembed}", rr.Body.String(), err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		counts, _ := store.CountKnowledgeChunksByIdentity(context.Background(), orgID, "my-model")
		if counts.Current == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the background job did not convert the corpus: %+v", counts)
		}
		time.Sleep(5 * time.Millisecond)
	}
	ev := store.lastEvent(t, audit.ActionKnowledgeReindex)
	if ev.Meta["background"] != "true" || ev.Meta["force"] != "true" {
		t.Errorf("audit meta = %v, want background=true and force=true", ev.Meta)
	}
}

// TestKnowledgeReindex_BackgroundRefusesPersonalKey: the background run spends unattended, so it runs on the
// organisation's shared credential only — a member's personal key answers 400 EMBEDDING_ORG_NOT_CONFIGURED and
// nothing starts.
func TestKnowledgeReindex_BackgroundRefusesPersonalKey(t *testing.T) {
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &auditRecordingBackend{FakeBackend: &testutil.FakeBackend{}}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	_ = store.SaveKnowledgeDocument(bgCtx(), "", &storageif.KnowledgeDocument{ID: "doc-1", OrgID: org.ID, Filename: "policy.md"})
	_ = store.SaveKnowledgeChunks(bgCtx(), "", []storageif.KnowledgeChunk{{ID: "c1", DocID: "doc-1", Content: "retry every call", Embedding: []float32{1, 0}}})
	factory := ai.NewProviderFactory(func(_, provider string) (string, error) {
		if provider == "openai" {
			return "sk-personal", nil
		}
		return "", nil
	}, nil, nil, nil)
	knowledge := rag.NewKnowledgeService(store, factory, nil)
	t.Cleanup(knowledge.StopJobs)
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	r := mountedAs(h, "u-admin")

	rr := postJSON(t, r, "/api/orgs/"+org.ID+"/knowledge/reindex", `{"background":true}`)
	if rr.Code != http.StatusBadRequest || errorCode(t, rr) != "EMBEDDING_ORG_NOT_CONFIGURED" {
		t.Fatalf("status = %d, body = %s; want 400 EMBEDDING_ORG_NOT_CONFIGURED", rr.Code, rr.Body.String())
	}
	if last, _ := store.JobLastSuccess(bgCtx(), "knowledge_reembed"); !last.IsZero() {
		t.Error("a refused background start ran the job")
	}
}
