package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-core/cache"
)

// Search cache tuning (see searchCaches). Vars so tests can shorten them.
var (
	// corpusCountTTL is how long an org's chunk count is trusted; in-process changes invalidate at
	// once (KnowledgeDocChanged), another replica's upload is seen within this window.
	corpusCountTTL = 30 * time.Second
	// corpusSettleTTL is how long an org reads as non-empty after an invalidation, without a count:
	// an upload's chunks may still sit in its uncommitted transaction when AddDocument returns.
	corpusSettleTTL = 10 * time.Second
	// queryEmbeddingTTL bounds how long a query's embedding is reused.
	queryEmbeddingTTL = 10 * time.Minute
)

const (
	corpusCountCacheSize    = 4096
	queryEmbeddingCacheSize = 256
	// corpusUnknown is the count an invalidated org reads as during corpusSettleTTL: not empty, so
	// Search embeds and searches as it always did.
	corpusUnknown = -1
)

// searchCaches are Search's per-service caches: per-org chunk counts (an org with nothing indexed skips
// the billed query embedding) and recent query embeddings (a regenerated answer embeds once). Bounded LRUs.
type searchCaches struct {
	// mu orders a count fill against InvalidateCorpus: a fill stores its count only when no
	// invalidation of its org ran since it read gen, so a count read before an upload never overwrites it.
	mu     sync.Mutex
	gen    eventbus.Gen[string]
	counts cache.Cache // orgID → total chunks (int)
	embeds cache.Cache // queryEmbeddingKey → []float32
}

func (s *KnowledgeService) caches() *searchCaches {
	s.searchOnce.Do(func() {
		counts, _ := cache.NewLRUCache(corpusCountCacheSize)
		embeds, _ := cache.NewLRUCache(queryEmbeddingCacheSize)
		s.search = &searchCaches{counts: counts, embeds: embeds}
	})
	return s.search
}

// InvalidateCorpus drops the org's cached chunk count. Until re-read the org counts as non-empty.
func (s *KnowledgeService) InvalidateCorpus(orgID string) {
	c := s.caches()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen.Bump(orgID)
	c.counts.Set(context.Background(), orgID, corpusUnknown, corpusSettleTTL)
}

// SubscribeCorpusCache drops s's cached chunk count of an org whenever its corpus changes, and when the
// organisation is deleted.
func SubscribeCorpusCache(bus *eventbus.Bus, s *KnowledgeService) {
	eventbus.Subscribe(bus, eventbus.Sync, "knowledge-corpus-count", func(_ context.Context, e events.KnowledgeDocChanged) {
		s.InvalidateCorpus(e.OrgID)
	})
	eventbus.Subscribe(bus, eventbus.Sync, "knowledge-corpus-count", func(_ context.Context, e events.OrgMemberChanged) {
		if e.Kind == events.OrgDeleted && e.OrgID != "" {
			s.InvalidateCorpus(e.OrgID)
		}
	})
}

// corpusEmpty reports whether the org has no knowledge chunks at all, from
// the cached count when fresh. A count that cannot be read is "not empty":
// Search then embeds and searches exactly as before.
func (s *KnowledgeService) corpusEmpty(ctx context.Context, orgID string) bool {
	c := s.caches()
	if v, ok := c.counts.Get(ctx, orgID); ok {
		return v.(int) == 0
	}
	captured := c.gen.Load(orgID)
	total, _, err := s.store.CountKnowledgeChunks(ctx, orgID)
	if err != nil {
		return false
	}
	c.mu.Lock()
	if c.gen.FillOK(ctx, orgID, captured) {
		c.counts.Set(ctx, orgID, total, corpusCountTTL)
	}
	c.mu.Unlock()
	return total == 0
}

// queryEmbeddingKey keys a query embedding by what produced it (the embedding identity: endpoint, model,
// width), the caller and the org, and a hash of the scrubbed query — never the text itself. The caller is
// part of the key on purpose: a shared entry would let one user time whether another had asked the same question.
func queryEmbeddingKey(identity, scope, orgID, query string) string {
	h := sha256.New()
	for _, part := range []string{identity, scope, orgID, query} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// embedQuery embeds the (scrubbed, truncated) query, reusing a recent embedding of the same query by the
// same embedder. ok is false when the provider answered no vector; a provider with no embedding identity is never cached.
func (s *KnowledgeService) embedQuery(ctx context.Context, provider ai.Provider, scope, orgID, query string) (vec []float32, ok bool, err error) {
	identity := ai.EmbeddingIdentity(provider)
	key := ""
	if identity != "" {
		key = queryEmbeddingKey(identity, scope, orgID, query)
		if v, hit := s.caches().embeds.Get(ctx, key); hit {
			return append([]float32(nil), v.([]float32)...), true, nil
		}
	}
	emb, err := provider.Embed(ctx, []string{query})
	if err != nil || len(emb) == 0 {
		return nil, false, err
	}
	if key != "" && len(emb[0]) > 0 {
		s.caches().embeds.Set(ctx, key, append([]float32(nil), emb[0]...), queryEmbeddingTTL)
	}
	return emb[0], true, nil
}
