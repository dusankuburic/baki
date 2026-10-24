package rag

import (
	"context"
	"crypto/sha256"
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

// Search's caches (ADR 0017): per-org chunk counts (an org with nothing indexed skips the billed query
// embedding) and recent query embeddings (a regenerated answer embeds once).
var (
	corpusCounts = cache.Declare("rag.corpus-counts",
		cache.Keys(cache.KindTenant),
		cache.Shared(cache.MechanismInvalidation, "an upload, delete or re-index on any replica drops the count on every replica once committed; the 30 s TTL bounds a lost message"))
	// The caller is part of an embedding's key on purpose: a shared entry would let one user time whether another
	// had asked the same question. The query is in the key only as its SHA-256.
	queryEmbeddings = cache.Declare("rag.query-embeddings",
		cache.Keys(cache.KindTenant, cache.KindParam, cache.KindContent),
		cache.ReplicaLocal("10 min TTL; a function of the embedder and the query, recomputed on a miss"))
)

// searchCaches are Search's per-service caches. Bounded LRUs.
type searchCaches struct {
	// gen fences a count fill against InvalidateCorpus: a fill stores its count only when no invalidation of its
	// org ran since it began, so a count read before an upload never overwrites it.
	gen    eventbus.Gen[string]
	counts *cache.Typed[int]       // total chunks of an org
	embeds *cache.Typed[[]float32] // a query's embedding
}

func (s *KnowledgeService) caches() *searchCaches {
	s.searchOnce.Do(func() {
		s.search = &searchCaches{
			counts: cache.New[int](corpusCounts, corpusCountCacheSize, cache.WithTTL(corpusCountTTL)),
			embeds: cache.New[[]float32](queryEmbeddings, queryEmbeddingCacheSize, cache.WithTTL(queryEmbeddingTTL)),
		}
	})
	return s.search
}

// corpusCountKey keys orgID's chunk count; false without an org.
func corpusCountKey(orgID string) (cache.Key, bool) {
	if orgID == "" {
		return cache.Key{}, false
	}
	k, err := corpusCounts.Key(cache.Org(orgID))
	return k, err == nil
}

// InvalidateCorpus drops the org's cached chunk count. Until re-read the org counts as non-empty.
func (s *KnowledgeService) InvalidateCorpus(orgID string) {
	k, ok := corpusCountKey(orgID)
	if !ok {
		return
	}
	c := s.caches()
	// The fence first: a fill in flight is refused, then the count is replaced by the settle marker.
	c.gen.Bump(orgID)
	c.counts.Set(k, corpusUnknown, 1, corpusSettleTTL)
}

// InvalidateAllCorpora drops every org's cached count and refuses the fills in flight: what a replica does when it
// cannot tell which org a lost invalidation concerned.
func (s *KnowledgeService) InvalidateAllCorpora() {
	c := s.caches()
	c.gen.BumpAll()
	c.counts.Clear()
}

// SubscribeCorpusCache drops s's cached chunk count of an org whenever its corpus changes, and when the
// organisation is deleted.
func SubscribeCorpusCache(bus *eventbus.Bus, s *KnowledgeService) {
	eventbus.Subscribe(bus, eventbus.Sync, "knowledge-corpus-count", func(_ context.Context, e events.KnowledgeDocChanged) {
		s.InvalidateCorpus(e.OrgID)
	}, eventbus.Replicated(func(e events.KnowledgeDocChanged) []eventbus.Invalidation {
		return corpusInvalidation(e.OrgID)
	}, "rag.corpus-counts"))
	eventbus.Subscribe(bus, eventbus.Sync, "knowledge-corpus-count", func(_ context.Context, e events.OrgMemberChanged) {
		if e.Kind == events.OrgDeleted && e.OrgID != "" {
			s.InvalidateCorpus(e.OrgID)
		}
	}, eventbus.Replicated(func(e events.OrgMemberChanged) []eventbus.Invalidation {
		if e.Kind != events.OrgDeleted {
			return nil
		}
		return corpusInvalidation(e.OrgID)
	}, "rag.corpus-counts"))
	// A peer's corpus change: the same drop for the org it names, every org's for an empty scope (a resync).
	bus.OnRemote("rag.corpus-counts", func(sc eventbus.Scope) {
		if org := sc["org"]; org != "" {
			s.InvalidateCorpus(org)
			return
		}
		s.InvalidateAllCorpora()
	})
}

// corpusInvalidation is what the other replicas drop when orgID's corpus changes; nothing without an org (the
// local drop ignores it too, and an empty scope would mean every org's count).
func corpusInvalidation(orgID string) []eventbus.Invalidation {
	if orgID == "" {
		return nil
	}
	return []eventbus.Invalidation{{Key: "rag.corpus-counts", Scope: eventbus.NewScope("org", orgID)}}
}

// corpusEmpty reports whether the org has no knowledge chunks at all, from
// the cached count when fresh. A count that cannot be read is "not empty":
// Search then embeds and searches exactly as before.
func (s *KnowledgeService) corpusEmpty(ctx context.Context, orgID string) bool {
	count := func(ctx context.Context) (int, int64, error) {
		total, _, err := s.store.CountKnowledgeChunks(ctx, orgID)
		return total, 1, err
	}
	k, ok := corpusCountKey(orgID)
	if !ok {
		total, _, err := count(ctx)
		return err == nil && total == 0
	}
	c := s.caches()
	// One count read per org at a time; stored only if no invalidation of the org ran meanwhile.
	total, err := c.counts.GetOrFill(ctx, k, count, &c.gen, orgID)
	if err != nil {
		return false
	}
	return total == 0
}

// queryEmbeddingKey keys a query embedding by the caller in the org, what produced it (the embedding identity:
// endpoint, model, width) and the scrubbed query's SHA-256 — never the text itself. The zero Key (uncached)
// when the caller or the org is missing.
func queryEmbeddingKey(identity, scope, orgID, query string) cache.Key {
	tenant := cache.OrgUser(orgID, scope)
	if tenant.IsZero() {
		return cache.Key{}
	}
	sum := sha256.Sum256([]byte(query))
	k, _ := queryEmbeddings.Key(tenant, cache.Param("embedder", identity), cache.Content(string(sum[:])))
	return k
}

// embedQuery embeds the (scrubbed, truncated) query, reusing a recent embedding of the same query by the
// same embedder. ok is false when the provider answered no vector; a provider with no embedding identity is never cached.
func (s *KnowledgeService) embedQuery(ctx context.Context, provider ai.Provider, scope, orgID, query string) (vec []float32, ok bool, err error) {
	identity := ai.EmbeddingIdentity(provider)
	var key cache.Key
	if identity != "" {
		key = queryEmbeddingKey(identity, scope, orgID, query)
		if v, hit := s.caches().embeds.Get(key); hit {
			return append([]float32(nil), v...), true, nil
		}
	}
	// Charged to the caller in the org (the wrapper's hook and usage row read it from ctx), as Search set it;
	// marked as a query so a provider that embeds the two sides of a retrieval pair differently (Gemini) can.
	emb, err := provider.Embed(ai.WithQueryEmbedding(ai.WithUsageScope(ctx, scope, orgID)), []string{query})
	if err != nil || len(emb) == 0 {
		return nil, false, err
	}
	if len(emb[0]) > 0 {
		s.caches().embeds.Set(key, append([]float32(nil), emb[0]...), 1, 0)
	}
	return emb[0], true, nil
}
