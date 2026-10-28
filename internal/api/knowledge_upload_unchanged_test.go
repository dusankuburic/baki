package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// countingIdentityEmbedder counts its Embed calls and names an embedding identity, so a stored document's chunks
// can be matched to it.
type countingIdentityEmbedder struct {
	ai.Provider
	calls atomic.Int32
}

func (e *countingIdentityEmbedder) ID() string { return "counting-identity-embed" }
func (e *countingIdentityEmbedder) EmbeddingIdentity() string {
	return "test|https://embed.example|model|2"
}
func (e *countingIdentityEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	e.calls.Add(1)
	out := make([][]float32, len(text))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// TestKnowledgeUpload_IdenticalReuploadAnswersUnchanged (R-security-c22): re-uploading byte-identical content under
// the same filename answers 200 with the stored document and unchanged: true, makes no embedding call, and its audit
// row says unchanged; the first upload answers 201 as before.
func TestKnowledgeUpload_IdenticalReuploadAnswersUnchanged(t *testing.T) {
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &testutil.FakeBackend{}
	store.SeedOrgs(org.ID)
	emb := &countingIdentityEmbedder{}
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(emb))
	h := mountedAs(NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil), "u-admin")
	rows := captureAuditEvents(t, 8)
	const body = `{"filename":"policy.md","content":"Variables use camelCase.\n\nEvery action that can fail sets an error handler."}`

	first := postJSON(t, h, "/api/orgs/"+org.ID+"/knowledge/upload", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload: status %d, body %s", first.Code, first.Body.String())
	}
	var stored rag.UploadResult
	if err := json.Unmarshal(first.Body.Bytes(), &stored); err != nil || stored.Unchanged {
		t.Fatalf("first upload body %s (%v): want a stored document", first.Body.String(), err)
	}
	if ev := nextAuditEvent(t, rows); ev.Action != audit.ActionKnowledgeUpload || ev.Meta["unchanged"] != "" {
		t.Errorf("first upload audit = %s %v", ev.Action, ev.Meta)
	}
	calls := emb.calls.Load()

	again := postJSON(t, h, "/api/orgs/"+org.ID+"/knowledge/upload", body)
	if again.Code != http.StatusOK {
		t.Fatalf("identical re-upload: status %d, body %s; want 200", again.Code, again.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(again.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["unchanged"] != true || got["id"] != stored.ID || got["state"] != "ready" || got["embedded"] != float64(0) || got["pendingChunks"] != float64(0) {
		t.Errorf("identical re-upload body = %s; want the stored document %s, unchanged", again.Body.String(), stored.ID)
	}
	if n := emb.calls.Load() - calls; n != 0 {
		t.Errorf("the identical re-upload made %d embedding calls, want none", n)
	}
	if ev := nextAuditEvent(t, rows); ev.Action != audit.ActionKnowledgeUpload || ev.Meta["unchanged"] != "true" || ev.Meta["docId"] != stored.ID {
		t.Errorf("re-upload audit = %s %v; want unchanged for %s", ev.Action, ev.Meta, stored.ID)
	}
}
