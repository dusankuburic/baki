package ai

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// recordingPolicy refuses every provider (wrapping ErrProviderNotAllowed) and records what it was asked.
type recordingPolicy struct {
	asked []string // "scope|org|provider"
}

func (p *recordingPolicy) Allowed(_ context.Context, scope, orgID, providerID string) error {
	p.asked = append(p.asked, scope+"|"+orgID+"|"+providerID)
	return fmt.Errorf("%w: %s", ErrProviderNotAllowed, providerID)
}

// ADR 0028: every builder asks the policy before it reads any credential — a refused provider never sees a key
// (member or org, document or OAuth token) and the refusal keeps its sentinel. The org builders name their
// organisation; the personal builders name the organisation of the context's budget scope, if any (an org
// flow on a member's own key), else none.
func TestFactory_PolicyRefusesEveryBuilderBeforeTheKeyRead(t *testing.T) {
	registerOrgStub()
	policy := &recordingPolicy{}
	keyReads := 0
	f := NewProviderFactory(func(string, string) (string, error) { keyReads++; return "k", nil }, nil, nil, nil, WithProviderPolicy(policy))
	f.SetOrgKeyLookup(func(string, string) (string, error) { keyReads++; return "org-k", nil })
	ctx := context.Background()
	entry := AzureRegisteredModel{Kind: AzureModelKindEmbedding, Deployment: "text-embedding-3-small"}

	builders := []struct {
		name  string
		build func() (Provider, error)
		want  string
	}{
		{"For", func() (Provider, error) { return f.For(ctx, "u1", "org-stub") }, "u1||org-stub"},
		{"ForEmbedding", func() (Provider, error) { return f.ForEmbedding(ctx, "u1", "org-stub", "") }, "u1||org-stub"},
		{"ForOrg", func() (Provider, error) { return f.ForOrg(ctx, "u1", "org-1", "org-stub") }, "u1|org-1|org-stub"},
		{"ForOrgCredential", func() (Provider, error) { return f.ForOrgCredential(ctx, "u1", "org-1", "org-stub") }, "u1|org-1|org-stub"},
		{"ForEmbeddingOrg", func() (Provider, error) { return f.ForEmbeddingOrg(ctx, "u1", "org-1", "org-stub", "") }, "u1|org-1|org-stub"},
		{"ForAzureConnection", func() (Provider, error) {
			return f.ForAzureConnection(ctx, "u1", AzureConnectionRef{OrgID: "org-1", ConnID: AzureDefaultConnectionID}, nil)
		}, "u1|org-1|" + AzureFoundryProviderID},
		{"ForAzureConnectionEmbedding", func() (Provider, error) {
			return f.ForAzureConnectionEmbedding(ctx, "u1", AzureConnectionRef{ConnID: AzureDefaultConnectionID}, entry)
		}, "u1||" + AzureFoundryProviderID},
		{"For on an org flow", func() (Provider, error) {
			return f.For(WithUsageScope(ctx, "u1", "org-9"), "u1", "org-stub")
		}, "u1|org-9|org-stub"},
		{"ForAzureConnection on an org flow", func() (Provider, error) {
			return f.ForAzureConnection(WithUsageScope(ctx, "u1", "org-9"), "u1", AzureConnectionRef{ConnID: AzureDefaultConnectionID}, nil)
		}, "u1|org-9|" + AzureFoundryProviderID},
	}
	for i, b := range builders {
		p, err := b.build()
		if !errors.Is(err, ErrProviderNotAllowed) {
			t.Errorf("%s: %v, want ErrProviderNotAllowed", b.name, err)
		}
		if p != nil {
			t.Errorf("%s: a provider was built for a refused id", b.name)
		}
		if len(policy.asked) != i+1 || policy.asked[i] != b.want {
			t.Errorf("%s: policy asked %v, want %q as call %d", b.name, policy.asked, b.want, i+1)
		}
	}
	if keyReads != 0 {
		t.Errorf("%d credentials were read for refused providers", keyReads)
	}

	// An unknown id is still PROVIDER_UNKNOWN, decided before the policy; without a policy every build proceeds.
	if _, err := f.For(ctx, "u1", "no-such-provider"); !errors.Is(err, ErrUnknownProvider) || len(policy.asked) != len(builders) {
		t.Errorf("unknown provider: %v (policy asked %d times)", err, len(policy.asked))
	}
	open := NewProviderFactory(func(string, string) (string, error) { return "k", nil }, nil, nil, nil)
	if _, err := open.ForOrg(ctx, "u1", "org-1", "org-stub"); err != nil {
		t.Errorf("no policy: %v", err)
	}
}
