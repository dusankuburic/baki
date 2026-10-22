package ai

import "strconv"

// EmbeddingIdentifier is implemented by providers that can name what their Embed produces: the endpoint, the
// embedding model and the requested vector width. Same identity ⇒ same text maps to the same vector, so an
// embedding cache may serve one's result for the other; different identities must never share. "" means the
// provider cannot say — nothing is shared.
type EmbeddingIdentifier interface {
	EmbeddingIdentity() string
}

// EmbeddingIdentity is p's embedding identity, or "" when p does not name one
// (see EmbeddingIdentifier).
func EmbeddingIdentity(p Provider) string {
	if id, ok := p.(EmbeddingIdentifier); ok {
		return id.EmbeddingIdentity()
	}
	return ""
}

// EmbeddingIdentity satisfies EmbeddingIdentifier for the OpenAI-compatible family: the /embeddings endpoint
// (which, for Azure AI Foundry, names the resource), the model or deployment, and the `dimensions` sent.
// "" when the provider has no embedding model (it cannot embed).
func (b *openaiBase) EmbeddingIdentity() string {
	if b.embeddingModel == "" || b.baseURL == nil {
		return ""
	}
	return b.providerLabel + "|" + *b.baseURL + "|" + b.embeddingModel + "|" + strconv.Itoa(b.embeddingDimensions())
}

// EmbeddingIdentity satisfies EmbeddingIdentifier for Gemini (one endpoint). The requested width is
// deliberately not part of it: the format is frozen (ADR 0027 §2) and a width change is detected through
// vector_dims, not the identity string.
func (g *GeminiProvider) EmbeddingIdentity() string {
	return "gemini|" + g.EmbeddingModel()
}

// ConcurrentEmbedder is implemented by providers whose Embed may be called from several goroutines at once
// (the knowledge base's upload overlaps its embedding batches); a provider that does not say so is called one
// batch at a time. Every provider chain the factory builds qualifies (the audited wrapper below).
type ConcurrentEmbedder interface {
	EmbedsConcurrently() bool
}

// EmbedsConcurrently reports whether p's Embed may run concurrently (see
// ConcurrentEmbedder).
func EmbedsConcurrently(p Provider) bool {
	c, ok := p.(ConcurrentEmbedder)
	return ok && c.EmbedsConcurrently()
}

// EmbedsConcurrently: the factory's chains are safe for concurrent use — the circuit breaker and retry wrappers
// keep state under locks, usage is recorded per call, and raw providers share only the HTTP client and their own
// lock-guarded memos.
func (p *auditedProvider) EmbedsConcurrently() bool { return true }

// EmbeddingIdentity forwards the RAW provider's identity, adopted before the
// resilience and tracing wrappers hid it (adoptRawHints).
func (p *auditedProvider) EmbeddingIdentity() string {
	if p.embedIdentifier != nil {
		return p.embedIdentifier.EmbeddingIdentity()
	}
	return EmbeddingIdentity(p.inner)
}
