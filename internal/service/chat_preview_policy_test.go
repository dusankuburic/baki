package service

import (
	"context"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-core/models"
)

// orgRecordingPolicy admits every provider and remembers the org each build named.
type orgRecordingPolicy struct {
	mu   sync.Mutex
	orgs []string
}

func (p *orgRecordingPolicy) Allowed(_ context.Context, _, orgID, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.orgs = append(p.orgs, orgID)
	return nil
}

// Phase 4b review T4: the context preview builds the turn's provider as the turn would, so an outsider's own key
// on an org flow is judged against the organisation's allowed-provider list there too — the worker seeds the
// budget scope on the stream context; the preview must as well.
func TestPreviewContext_PolicySeesTheFlowsOrg(t *testing.T) {
	registerFakeProvider()
	policy := &orgRecordingPolicy{}
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "own-key", nil }, nil, nil, nil, ai.WithProviderPolicy(policy))
	svc := &ChatService{
		factory:  factory,
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 100}}},
		backend:  &roleBackend{},
	}
	doc := &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}
	if _, err := svc.PreviewContext(context.Background(), "u-collab", doc, nil, models.ChatRequest{Provider: "fake"}); err != nil {
		t.Fatalf("collaborator preview on their own key: %v", err)
	}
	policy.mu.Lock()
	defer policy.mu.Unlock()
	if len(policy.orgs) == 0 || policy.orgs[0] != "org-A" {
		t.Errorf("the policy was asked for orgs %q, want the flow's org-A (the preview must accept exactly what the turn would)", policy.orgs)
	}
}
