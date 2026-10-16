package rag

import (
	"context"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// recordingPolicy admits every provider and remembers the org each build named.
type recordingPolicy struct {
	mu   sync.Mutex
	orgs []string
}

func (p *recordingPolicy) Allowed(_ context.Context, _, orgID, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.orgs = append(p.orgs, orgID)
	return nil
}

func (p *recordingPolicy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.orgs...)
}

// TestKnowledge_PolicySeesTheOrgOutsideARun (Phase 4b review T5): the embedder is resolved for the document
// list's stranded count and for a background start outside any run, where nothing seeds the budget scope; the
// settings-chosen provider is then built by the personal builder, which names the org only through the context.
// Both must carry it, so the organisation's allowed-provider list applies before a member's key is read.
func TestKnowledge_PolicySeesTheOrgOutsideARun(t *testing.T) {
	policy := &recordingPolicy{}
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "sk-member", nil }, nil, nil, nil, ai.WithProviderPolicy(policy))
	store := testutil.NewFakeBackend()
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	_ = store.SaveKnowledgeDocument(context.Background(), "", &interfaces.KnowledgeDocument{ID: "doc-1", OrgID: "org-1", Filename: "a.md"})
	settings := &models.AppSettings{}
	settings.AI.EmbeddingProvider = "openai" // chosen in settings, not pinned by PAD_EMBEDDING_PROVIDER
	svc := NewKnowledgeService(store, factory, stubSettings{settings})
	t.Cleanup(svc.StopJobs)

	if _, err := svc.ListDocuments(context.Background(), "u-member", "org-1", 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := policy.seen(); len(got) == 0 || got[0] != "org-1" {
		t.Errorf("ListDocuments: the policy was asked for orgs %q, want org-1 first", got)
	}

	policy.orgs = nil
	_, _ = svc.ReindexInBackground(context.Background(), "u-member", "org-1", false) // refused (personal key) after the policy
	if got := policy.seen(); len(got) == 0 || got[0] != "org-1" {
		t.Errorf("ReindexInBackground: the policy was asked for orgs %q, want org-1 first", got)
	}
}
