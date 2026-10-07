package di

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/testutil"
)

// TestProvideAI_EmbeddingWidthDefaultsToStorageContract pins #11: with
// PAD_EMBEDDING_DIM unset the factory asks embedding models for the same
// 1536 the storage layer indexes (it used to pass the raw 0, so no
// `dimensions` was ever sent and text-embedding-3-large came back 3072 wide).
func TestProvideAI_EmbeddingWidthDefaultsToStorageContract(t *testing.T) {
	cfg := config.Default()
	_, _, factory := ProvideAI("", nil, cfg, testutil.NewFakeSecretStore(), nil)
	if got := factory.EmbeddingDimensions(); got != config.DefaultEmbeddingDim {
		t.Errorf("unset PAD_EMBEDDING_DIM: factory width = %d, want %d", got, config.DefaultEmbeddingDim)
	}

	cfg.Storage.EmbeddingDim = 3072
	_, _, factory = ProvideAI("", nil, cfg, testutil.NewFakeSecretStore(), nil)
	if got := factory.EmbeddingDimensions(); got != 3072 {
		t.Errorf("PAD_EMBEDDING_DIM=3072: factory width = %d, want 3072", got)
	}
}

// TestProvideKnowledgeService_PinsEmbeddingProvider pins the #34 plumbing:
// PAD_EMBEDDING_PROVIDER reaches the knowledge service. Observed without any
// network: a user whose only configuration is an Azure document without an
// embedding deployment gets Azure's own "no embedding deployment" error only
// when the operator pinned azure-foundry — otherwise Azure is never consulted.
func TestProvideKnowledgeService_PinsEmbeddingProvider(t *testing.T) {
	ai.ResetAzureFoundryCaches()
	keys := testutil.NewFakeSecretStore()
	_ = keys.Save("u1", "azure-foundry", `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"gpt-4o"}`)
	factory := ai.NewProviderFactory(keys.Get, nil, nil, nil)
	ctx := context.Background()

	cfg := config.Default()
	cfg.Storage.EmbeddingProvider = "azure-foundry"
	err := ProvideKnowledgeService(&testutil.FakeBackend{}, factory, nil, cfg, nil, nil, nil).AddDocument(ctx, "u1", "org-1", "a.md", "content")
	if !errors.Is(err, rag.ErrEmbeddingUnavailable) || !errors.Is(err, ai.ErrEmbeddingDeploymentMissing) {
		t.Errorf("pinned azure-foundry: err = %v, want Azure consulted (ErrEmbeddingDeploymentMissing)", err)
	}

	cfg.Storage.EmbeddingProvider = ""
	err = ProvideKnowledgeService(&testutil.FakeBackend{}, factory, nil, cfg, nil, nil, nil).AddDocument(ctx, "u1", "org-1", "a.md", "content")
	if !errors.Is(err, rag.ErrEmbeddingUnavailable) || errors.Is(err, ai.ErrEmbeddingDeploymentMissing) {
		t.Errorf("no pin: err = %v, want ErrEmbeddingUnavailable without consulting Azure", err)
	}
}
