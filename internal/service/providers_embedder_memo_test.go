package service

import (
	"slices"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/testutil"
)

// The credential changes ProviderService makes itself (the GitHub and Copilot sign-ins, the local and Azure
// configurations, an organisation's Azure credentials) are announced as CredentialChanged, so every cache derived
// from credentials hears them — the knowledge base's memoised embedders (rag.embedders) as well as the model
// listings — on this replica and, through their replicated subscribers, on the others (R-observability-5).
func TestProviderService_OwnCredentialChangesAreAnnounced(t *testing.T) {
	bus, rec := replicatedBus(func(b *eventbus.Bus) {})
	events := eventbus.NewRecorder(bus)
	secrets := testutil.NewFakeSecretStore()
	factory := ai.NewProviderFactory(func(scope, provider string) (string, error) { v, _ := secrets.Get(scope, provider); return v, nil }, ai.NewCopilotAuth(), nil, nil)
	svc := NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, nil, nil, WithProviderEvents(bus))
	SubscribeModelCache(bus, svc)
	rag.SubscribeEmbedderCache(bus, rag.NewKnowledgeService(nil, factory, nil))

	if err := svc.RevokeGitHubAuth("u1"); err != nil {
		t.Fatal(err)
	}
	got := eventbusRecordedCredentials(events)
	if len(got) != 1 || got[0].Scope != "u1" || got[0].ProviderID != ai.GitHubModelsProviderID {
		t.Fatalf("RevokeGitHubAuth announced %+v, want one CredentialChanged of u1's github-models", got)
	}
	want := []string{"providers.model-listings{provider=" + ai.GitHubModelsProviderID + ",scope=u1}", "rag.embedders{scope=u1}"}
	if p := rec.Published(); !slices.Equal(p, want) {
		t.Errorf("the other replicas were told %v, want %v", p, want)
	}

	rec.Reset()
	events.Reset()
	svc.invalidateScopeEverywhere(ai.OrgScope("o1"))
	if got := eventbusRecordedCredentials(events); len(got) != 1 || got[0].Scope != ai.OrgScope("o1") || got[0].OrgID != "o1" || got[0].ProviderID != "" {
		t.Errorf("an org scope drop announced %+v, want one CredentialChanged of org:o1", got)
	}
	if p, want := rec.Published(), []string{"providers.model-listings{scope=org:o1}", "rag.embedders{org=o1}"}; !slices.Equal(p, want) {
		t.Errorf("the other replicas were told %v, want %v", p, want)
	}
}

func eventbusRecordedCredentials(r *eventbus.Recorder) []events.CredentialChanged {
	return eventbus.Recorded[events.CredentialChanged](r)
}
