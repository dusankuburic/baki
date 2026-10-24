package api

import (
	"net/http"
	"strings"
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

// TestKnowledgeUpload_AuditsTheStoredFilename (R-knowledge-7): AddDocument masks the filename for secret patterns
// before it is stored, and the audit row records that name, not the one the request carried.
func TestKnowledgeUpload_AuditsTheStoredFilename(t *testing.T) {
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &auditRecordingBackend{FakeBackend: &testutil.FakeBackend{EmbeddingDim: 2}}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	factory := ai.NewProviderFactory(nil, nil, nil, nil)
	factory.SetEmbeddingDimensions(2)
	emb := identityDirectionEmbedder{directionEmbedder: directionEmbedder{vec: []float32{1, 0}}, identity: "model"}
	knowledge := rag.NewKnowledgeService(store, factory, nil, rag.WithEmbedder(emb))
	t.Cleanup(knowledge.StopJobs)
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	router := mountedAs(h, "u-admin")

	const secret = "hunter2secretx"
	rr := postJSON(t, router, "/api/orgs/"+org.ID+"/knowledge/upload", `{"filename":"db Password=`+secret+`.md","content":"retry every call three times"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload: status = %d, body = %s", rr.Code, rr.Body.String())
	}
	e := store.lastEvent(t, string(audit.ActionKnowledgeUpload))
	if audited := e.Meta["filename"]; strings.Contains(audited, secret) || !strings.Contains(audited, "[REDACTED]") {
		t.Errorf("audited filename = %q, want the stored, masked name", audited)
	}
	for _, d := range store.KnowledgeDocs {
		if strings.Contains(d.Filename, secret) {
			t.Errorf("stored filename = %q, want it masked", d.Filename)
		}
	}
}
