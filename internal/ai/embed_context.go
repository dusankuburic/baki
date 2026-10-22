package ai

import "context"

type queryEmbeddingKey struct{}

// WithQueryEmbedding marks the Embed calls under ctx as search queries rather than documents to be stored.
// Gemini embeds the two sides of a retrieval pair under different task types (RETRIEVAL_QUERY against
// RETRIEVAL_DOCUMENT); the other providers embed both alike. The knowledge base sets it on its query path, and
// every wrapper (audited, retry, breaker, tracing) forwards ctx unchanged.
func WithQueryEmbedding(ctx context.Context) context.Context {
	return context.WithValue(ctx, queryEmbeddingKey{}, true)
}

// IsQueryEmbedding reads WithQueryEmbedding.
func IsQueryEmbedding(ctx context.Context) bool {
	v, _ := ctx.Value(queryEmbeddingKey{}).(bool)
	return v
}
