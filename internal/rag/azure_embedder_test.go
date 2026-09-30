package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// fakeAzureSource is an AzureEmbedderSource answering a fixed provider or
// error, recording its calls.
type fakeAzureSource struct {
	p     ai.Provider
	err   error
	calls []string
}

func (f *fakeAzureSource) KnowledgeEmbedder(_ context.Context, scope, orgID string) (ai.Provider, error) {
	f.calls = append(f.calls, scope+"/"+orgID)
	return f.p, f.err
}

// With the Azure AI Foundry model registry, the pinned Azure embedder is the
// registry's "Use for knowledge base" model: an upload embeds with it; the
// organisation's refusal is final (no personal fallback); a caller with
// nothing designated falls through to the usual scan; a registry failure is
// never replaced by another provider; without the operator's pin the
// registry is never asked.
func TestEmbedder_AzureRegistryDesignation(t *testing.T) {
	ctx := context.Background()
	emb := azureEmbedder{&fixedEmbedder{vec: []float32{1, 0}}}

	t.Run("the designated model embeds", func(t *testing.T) {
		src := &fakeAzureSource{p: emb}
		store := testutil.NewFakeBackend()
		svc := NewKnowledgeService(store, newKeysFactory(nil), stubSettings{&models.AppSettings{}},
			WithEmbeddingProvider("azure-foundry"), WithAzureEmbedder(src))
		if !svc.AzureEmbedderEnabled() {
			t.Error("AzureEmbedderEnabled = false")
		}
		if err := svc.AddDocument(ctx, "u1", "org-1", "a.md", "some guideline"); err != nil {
			t.Fatal(err)
		}
		if emb.calls == 0 || len(src.calls) != 1 || src.calls[0] != "u1/org-1" {
			t.Errorf("embed calls %d, source calls %v", emb.calls, src.calls)
		}
	})

	t.Run("an organisation without a designation refuses", func(t *testing.T) {
		src := &fakeAzureSource{err: ai.ErrOrgEmbeddingDeploymentMissing}
		// The member has an OpenAI key: it must not seed the org's corpus.
		svc := NewKnowledgeService(nil, newKeysFactory(map[string]string{"openai": "k"}), stubSettings{&models.AppSettings{}},
			WithEmbeddingProvider("azure-foundry"), WithAzureEmbedder(src))
		_, err := svc.embedder("u1", "org-1")
		if !errors.Is(err, ErrEmbeddingUnavailable) || !errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing) {
			t.Errorf("err = %v, want the org refusal", err)
		}
	})

	t.Run("nothing designated for the caller looks further", func(t *testing.T) {
		src := &fakeAzureSource{err: ai.ErrEmbeddingDeploymentMissing}
		svc := NewKnowledgeService(nil, newKeysFactory(map[string]string{"openai": "k"}), stubSettings{&models.AppSettings{}},
			WithEmbeddingProvider("azure-foundry"), WithAzureEmbedder(src))
		p, err := svc.embedder("u1", "")
		if err != nil || p.ID() != "openai" {
			t.Errorf("embedder = %v, %v; want the fallback scan's openai", p, err)
		}
	})

	t.Run("a registry failure never falls back and never shows its text", func(t *testing.T) {
		const driver = "failed to connect to `host=10.1.2.3 user=baki_app database=baki`: dial error"
		for _, cause := range []error{errors.New(driver), ErrEmbeddingSetupUnavailable} {
			src := &fakeAzureSource{err: cause}
			svc := NewKnowledgeService(nil, newKeysFactory(map[string]string{"openai": "k"}), stubSettings{&models.AppSettings{}},
				WithEmbeddingProvider("azure-foundry"), WithAzureEmbedder(src))
			p, err := svc.embedder("u1", "")
			if p != nil || !errors.Is(err, ErrEmbeddingSetupUnavailable) || errors.Is(err, ErrEmbeddingUnavailable) {
				t.Errorf("%v: embedder = %v, %v; want the transient ErrEmbeddingSetupUnavailable (503), not a configuration refusal", cause, p, err)
			}
			if err != nil && strings.Contains(err.Error(), "10.1.2.3") {
				t.Errorf("the refusal carries the driver's text: %v", err)
			}
		}
	})

	t.Run("a designated resource whose settings no longer validate is a configuration gap", func(t *testing.T) {
		src := &fakeAzureSource{err: fmt.Errorf("azure-foundry: %w: host not allowed", ai.ErrProviderConfigInvalid)}
		svc := NewKnowledgeService(nil, newKeysFactory(map[string]string{"openai": "k"}), stubSettings{&models.AppSettings{}},
			WithEmbeddingProvider("azure-foundry"), WithAzureEmbedder(src))
		if p, err := svc.embedder("u1", ""); p != nil || !errors.Is(err, ErrEmbeddingUnavailable) || !strings.Contains(err.Error(), "host not allowed") {
			t.Errorf("embedder = %v, %v; want ErrEmbeddingUnavailable naming the setting, never the fallback", p, err)
		}
	})

	t.Run("without the operator's pin the registry is not asked", func(t *testing.T) {
		src := &fakeAzureSource{p: emb}
		svc := NewKnowledgeService(nil, newKeysFactory(map[string]string{"openai": "k"}), stubSettings{&models.AppSettings{}},
			WithAzureEmbedder(src))
		if svc.AzureEmbedderEnabled() {
			t.Error("AzureEmbedderEnabled without the pin")
		}
		if p, err := svc.embedder("u1", ""); err != nil || p.ID() != "openai" || len(src.calls) != 0 {
			t.Errorf("embedder = %v, %v; source calls %v", p, err, src.calls)
		}
	})

	t.Run("a source answering nothing is not configured", func(t *testing.T) {
		src := &fakeAzureSource{}
		svc := NewKnowledgeService(nil, nil, stubSettings{&models.AppSettings{}},
			WithEmbeddingProvider("azure-foundry"), WithAzureEmbedder(src))
		if _, err := svc.embedder("u1", ""); !errors.Is(err, ErrEmbeddingUnavailable) || !errors.Is(err, ai.ErrKeyNotConfigured) {
			t.Errorf("err = %v", err)
		}
	})
}
