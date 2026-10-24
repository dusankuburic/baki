package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/ai"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// countingChatProvider answers every Chat and counts them.
type countingChatProvider struct {
	*testutil.FakeProvider
	id    string
	chats int
}

func (p *countingChatProvider) ID() string { return p.id }
func (p *countingChatProvider) Chat(context.Context, ai.Request) (*ai.Response, error) {
	p.chats++
	return &ai.Response{Content: "ok", TokensIn: 4, TokensOut: 2}, nil
}

// recordingProviderService builds a provider service with every usage row forwarded to the returned channel.
func recordingProviderService(t *testing.T, secrets *testutil.FakeSecretStore, orgKeys *testutil.FakeOrgSecretStore) (*ProviderService, <-chan *storageif.UsageMetric) {
	t.Helper()
	rows := make(chan *storageif.UsageMetric, 8)
	rec := func(_ context.Context, m *storageif.UsageMetric) error { rows <- m; return nil }
	getKey := func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}
	factory := ai.NewProviderFactory(getKey, ai.NewCopilotAuth(), rec, nil)
	if orgKeys != nil {
		factory.SetOrgKeyLookup(orgKeys.OrgKeyLookup)
	}
	return NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, orgKeys, nil), rows
}
