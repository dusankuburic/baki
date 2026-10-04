package rag

import (
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-core/models"
)

// orgKeysFactory is newKeysFactory with an organisation credential store: it
// records which store was consulted so a test can tell whose credential the
// embedder resolved (the wrapped provider hides the raw document).
func orgKeysFactory(member, org map[string]string, memberCalls, orgCalls *int) *ai.ProviderFactory {
	f := ai.NewProviderFactory(func(_ string, provider string) (string, error) {
		*memberCalls++
		return member[provider], nil
	}, nil, nil, nil)
	f.SetOrgKeyLookup(func(_ string, provider string) (string, error) {
		*orgCalls++
		return org[provider], nil
	})
	return f
}

// G7: with PAD_EMBEDDING_PROVIDER=azure-foundry the org's SHARED document
// embeds the corpus for every member — even one who configured their own
// Azure document — and a member without a document of their own embeds with
// the org's; without an org document the member's own is used.
func TestEmbedder_AzureOrgCredentialFirst(t *testing.T) {
	ai.ResetAzureFoundryCaches()
	memberDoc := azureDoc("member-embed")
	orgDoc := azureDoc("org-embed")

	t.Run("org document wins over the member's", func(t *testing.T) {
		memberCalls, orgCalls := 0, 0
		f := orgKeysFactory(map[string]string{"azure-foundry": memberDoc}, map[string]string{"azure-foundry": orgDoc}, &memberCalls, &orgCalls)
		svc := NewKnowledgeService(nil, f, stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("azure-foundry"))
		p, err := svc.embedder("u-own", "org-1")
		if err != nil || p.ID() != "azure-foundry" {
			t.Fatalf("embedder = %v, %v; want azure-foundry", p, err)
		}
		if orgCalls != 1 || memberCalls != 0 {
			t.Errorf("org consulted %d times, member %d; want the org's document only (one corpus, one resource)", orgCalls, memberCalls)
		}
	})

	t.Run("member without a document embeds with the org's", func(t *testing.T) {
		memberCalls, orgCalls := 0, 0
		f := orgKeysFactory(map[string]string{}, map[string]string{"azure-foundry": orgDoc}, &memberCalls, &orgCalls)
		svc := NewKnowledgeService(nil, f, stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("azure-foundry"))
		p, err := svc.embedder("u-none", "org-1")
		if err != nil || p.ID() != "azure-foundry" {
			t.Fatalf("embedder = %v, %v; want azure-foundry from the org", p, err)
		}
		if orgCalls != 1 || memberCalls != 0 {
			t.Errorf("org consulted %d times, member %d", orgCalls, memberCalls)
		}
	})

	t.Run("no org document falls back to the member's", func(t *testing.T) {
		memberCalls, orgCalls := 0, 0
		f := orgKeysFactory(map[string]string{"azure-foundry": memberDoc}, map[string]string{}, &memberCalls, &orgCalls)
		svc := NewKnowledgeService(nil, f, stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("azure-foundry"))
		p, err := svc.embedder("u-own", "org-1")
		if err != nil || p.ID() != "azure-foundry" {
			t.Fatalf("embedder = %v, %v; want the member's azure-foundry", p, err)
		}
		if orgCalls != 1 || memberCalls != 1 {
			t.Errorf("org consulted %d times, member %d; want both, org first", orgCalls, memberCalls)
		}
	})

	t.Run("personal scope never consults the org store", func(t *testing.T) {
		memberCalls, orgCalls := 0, 0
		f := orgKeysFactory(map[string]string{}, map[string]string{"azure-foundry": orgDoc}, &memberCalls, &orgCalls)
		svc := NewKnowledgeService(nil, f, stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("azure-foundry"))
		if _, err := svc.embedder("u-none", ""); !errors.Is(err, ErrEmbeddingUnavailable) {
			t.Errorf("err = %v, want ErrEmbeddingUnavailable without an org", err)
		}
		if orgCalls != 0 {
			t.Errorf("org store consulted %d times without an org id", orgCalls)
		}
	})
}

// The operator's pinned provider is org-first too; the default (settings)
// provider and the fallback scan stay per member — an org shares one
// deliberate credential, not a fallback.
func TestEmbedder_PinnedProviderOrgFirst_DefaultStaysPersonal(t *testing.T) {
	memberCalls, orgCalls := 0, 0
	f := orgKeysFactory(map[string]string{"glm": "member-k"}, map[string]string{"glm": "org-k", "openai": "org-k"}, &memberCalls, &orgCalls)

	svc := NewKnowledgeService(nil, f, stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("glm"))
	if p, err := svc.embedder("u1", "org-1"); err != nil || p.ID() != "glm" {
		t.Fatalf("pinned glm: embedder = %v, %v", p, err)
	}
	if orgCalls != 1 || memberCalls != 0 {
		t.Errorf("pinned provider: org consulted %d times, member %d; want the org's credential first", orgCalls, memberCalls)
	}

	// Default openai from the settings: the org's openai key is NOT used; the
	// member has no openai key, so the fallback scan finds their glm key.
	memberCalls, orgCalls = 0, 0
	svc = NewKnowledgeService(nil, f, stubSettings{&models.AppSettings{}})
	if p, err := svc.embedder("u1", "org-1"); err != nil || p.ID() != "glm" {
		t.Fatalf("default: embedder = %v, %v; want the member's glm via the fallback", p, err)
	}
	if orgCalls != 0 {
		t.Errorf("org store consulted %d times for an unpinned provider, want 0", orgCalls)
	}
}

// The org's shared Azure credential without an embedding deployment
// refuses instead of falling back to the member's personal key — a member's
// key must not seed the org corpus, and the error names the org admin's fix.
func TestEmbedder_OrgCredentialWithoutEmbeddingDeployment_RefusesFallback(t *testing.T) {
	ai.ResetAzureFoundryCaches()
	memberCalls, orgCalls := 0, 0
	f := orgKeysFactory(map[string]string{"openai": "member-openai-key"}, map[string]string{"azure-foundry": azureDoc("")}, &memberCalls, &orgCalls)
	svc := NewKnowledgeService(nil, f, stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("azure-foundry"))

	p, err := svc.embedder("u-own", "org-1")
	if p != nil {
		t.Fatalf("embedder = %v, want none", p)
	}
	if !errors.Is(err, ErrEmbeddingUnavailable) || !errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing) {
		t.Fatalf("err = %v, want ErrEmbeddingUnavailable wrapping ai.ErrOrgEmbeddingDeploymentMissing", err)
	}
	if orgCalls != 1 || memberCalls != 0 {
		t.Errorf("org consulted %d times, member %d; want the org's document once and no personal fallback", orgCalls, memberCalls)
	}
}
