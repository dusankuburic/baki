package rag

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/cache"
)

// The knowledge search's and the knowledge list's memos (ADR 0017; R-observability-5, R-knowledge-1, R-knowledge-15).
//
// rag.embedders holds the caller's resolved embedder: resolving one reads the key store, the provider policy and
// the model registry, and every chat turn on an organisation's flow and every knowledge list did it again. The
// memo holds a credentialed provider chain, so a TTL alone would let a revoked key or a newly disallowed provider
// keep embedding until it ran out: it is dropped by the same changes that drop providers.model-listings — a
// credential saved or removed (CredentialChanged, which ProviderService also publishes for its own sign-ins and
// configurations), an erased account — and by what the listing does not depend on but the embedder does: the
// provider policy and the embedding settings (SettingsChanged) and the caller's membership of the org, whose
// shared credential an org-first provider embeds with (OrgMemberChanged). A change of the registry's "Use for
// knowledge base" designation announces nothing; the TTL bounds it.
//
// rag.identity-census and rag.stranded-docs hold an org's chunk census and stranded-by-document map relative to one
// embedding identity; both are keyed by the org's corpus generation, which the corpus invalidation moves.
var (
	embedderMemos = cache.Declare("rag.embedders",
		cache.Keys(cache.KindTenant, cache.KindVersion),
		cache.Shared(cache.MechanismInvalidation, "a credential, provider-policy, settings or membership change on any replica drops the caller's or the organisation's memo on every replica once committed; the 60 s TTL bounds a lost message and a registry designation change"))
	identityCensus = cache.Declare("rag.identity-census",
		cache.Keys(cache.KindTenant, cache.KindParam, cache.KindVersion),
		cache.ReplicaLocal("keyed by the org's corpus generation, which an upload, delete or re-index on any replica moves through rag.corpus-counts; the 30 s TTL bounds a lost message"))
	strandedDocs = cache.Declare("rag.stranded-docs",
		cache.Keys(cache.KindTenant, cache.KindParam, cache.KindVersion),
		cache.ReplicaLocal("keyed by the org's corpus generation, which an upload, delete or re-index on any replica moves through rag.corpus-counts; the 30 s TTL bounds a lost message"))
)

// embedderMemoTTL bounds how long a resolved embedder (or the answer that the caller has none) is reused. A var so
// tests can shorten it.
var embedderMemoTTL = time.Minute

const (
	embedderMemoCacheSize = 1024
	censusCacheSize       = 1024
	strandedDocsCacheSize = 256
)

// embedderMemo is a resolution's outcome: the embedder, or why the caller has none (err).
type embedderMemo struct {
	provider ai.Provider
	err      error
}

// memoisableEmbedderError reports whether a resolution's failure is an answer about the caller's credentials and
// the deployment's choices, which only the changes that drop the memo can alter: no embedding provider configured,
// none the caller holds a key for, or one the policy refuses. A failure to read (the key store, the policy, the
// registry) is not remembered.
func memoisableEmbedderError(err error) bool {
	return errors.Is(err, ErrEmbeddingNotConfigured) || errors.Is(err, ErrEmbeddingUnavailable) || errors.Is(err, ai.ErrProviderNotAllowed)
}

// embedderTenant is the memo's tenant part: the caller inside the org, the org alone for the deployment's scope,
// the caller alone outside an org.
func embedderTenant(scope, orgID string) cache.Part {
	switch {
	case scope != "" && orgID != "":
		return cache.OrgUser(orgID, scope)
	case orgID != "":
		return cache.Org(orgID)
	case scope != "":
		return cache.User(scope)
	}
	return cache.Local()
}

// embedderKey keys (scope, org)'s memo at the current generations of both.
func (c *searchCaches) embedderKey(scope, orgID string) cache.Key {
	v := strconv.FormatUint(c.scopeGen.Load(scope), 10) + "." + strconv.FormatUint(c.orgGen.Load(orgID), 10)
	k, _ := embedderMemos.Key(embedderTenant(scope, orgID), cache.Version(v))
	return k
}

// resolvedEmbedder is embedderFor through the memo, for the reads that resolve the caller's embedder on every
// request: the knowledge search and the knowledge list. An upload and a re-index resolve afresh — they write the
// corpus with what they resolve. Concurrent resolutions of one (scope, org) share one fill.
func (s *KnowledgeService) resolvedEmbedder(ctx context.Context, scope, orgID string) (ai.Provider, error) {
	if s.embedOverride != nil {
		return s.embedOverride, nil
	}
	c := s.caches()
	m, err := c.embedders.GetOrFill(ctx, c.embedderKey(scope, orgID), func(ctx context.Context) (embedderMemo, int64, error) {
		p, err := s.embedderFor(ctx, scope, orgID)
		if err != nil && !memoisableEmbedderError(err) {
			return embedderMemo{}, 0, err
		}
		return embedderMemo{provider: p, err: err}, 1, nil
	}, nil, "")
	if err != nil {
		return nil, err
	}
	return m.provider, m.err
}

// InvalidateEmbedderScope drops the memoised embedders of one key scope (a user's own keys) in every org.
func (s *KnowledgeService) InvalidateEmbedderScope(scope string) { s.caches().scopeGen.Bump(scope) }

// InvalidateEmbedderOrg drops the memoised embedders of every caller in orgID.
func (s *KnowledgeService) InvalidateEmbedderOrg(orgID string) { s.caches().orgGen.Bump(orgID) }

// InvalidateAllEmbedders drops every memoised embedder: a deployment-wide change, or a resync after a lost message.
func (s *KnowledgeService) InvalidateAllEmbedders() { s.caches().scopeGen.BumpAll() }

// embedderDrop is what one change drops: a key scope's memos, an org's, or (all) every one.
type embedderDrop struct {
	scope, org string
	all        bool
}

// credentialDrop is the drop of a credential change of key scope: an organisation's shared scope drops the org's
// memos, the deployment's ("") every one, a user's their own.
func credentialDrop(scope string) embedderDrop {
	if orgID, ok := strings.CutPrefix(scope, ai.OrgScope("")); ok {
		if orgID == "" {
			return embedderDrop{all: true}
		}
		return embedderDrop{org: orgID}
	}
	if scope == "" {
		return embedderDrop{all: true}
	}
	return embedderDrop{scope: scope}
}

// settingsDrop is the drop of a settings change: the deployment's settings carry the embedding provider and the
// policy's Enabled flags (every memo), an org's its allowed providers, a user's nothing the embedder reads today but
// their own memos are cheap to rebuild.
func settingsDrop(e events.SettingsChanged) embedderDrop {
	switch {
	case e.Scope == events.OrgSettings && e.ID != "":
		return embedderDrop{org: e.ID}
	case e.Scope == events.UserSettings && e.ID != "":
		return embedderDrop{scope: e.ID}
	}
	return embedderDrop{all: true}
}

func (s *KnowledgeService) applyEmbedderDrop(d embedderDrop) {
	if s == nil {
		return
	}
	switch {
	case d.all:
		s.InvalidateAllEmbedders()
	case d.org != "":
		s.InvalidateEmbedderOrg(d.org)
	case d.scope != "":
		s.InvalidateEmbedderScope(d.scope)
	}
}

// embedderInvalidation is what the other replicas drop for d: the scope or the org it names, every memo for an
// empty scope.
func embedderInvalidation(d embedderDrop) []eventbus.Invalidation {
	switch {
	case d.all:
		return []eventbus.Invalidation{{Key: "rag.embedders", Scope: eventbus.NewScope()}}
	case d.org != "":
		return []eventbus.Invalidation{{Key: "rag.embedders", Scope: eventbus.NewScope("org", d.org)}}
	case d.scope != "":
		return []eventbus.Invalidation{{Key: "rag.embedders", Scope: eventbus.NewScope("scope", d.scope)}}
	}
	return nil
}

// SubscribeEmbedderCache drops s's memoised embedders on the changes that can change what a caller resolves (see
// rag.embedders above), on this replica and, through rag.embedders, on the others.
func SubscribeEmbedderCache(bus *eventbus.Bus, s *KnowledgeService) {
	eventbus.Subscribe(bus, eventbus.Sync, "knowledge-embedder-cache", func(_ context.Context, e events.CredentialChanged) {
		s.applyEmbedderDrop(credentialDrop(e.Scope))
	}, eventbus.Replicated(func(e events.CredentialChanged) []eventbus.Invalidation {
		return embedderInvalidation(credentialDrop(e.Scope))
	}, "rag.embedders"))
	eventbus.Subscribe(bus, eventbus.Sync, "knowledge-embedder-cache", func(_ context.Context, e events.AccountErased) {
		s.applyEmbedderDrop(credentialDrop(e.KeyScope))
	}, eventbus.Replicated(func(e events.AccountErased) []eventbus.Invalidation {
		return embedderInvalidation(credentialDrop(e.KeyScope))
	}, "rag.embedders"))
	eventbus.Subscribe(bus, eventbus.Sync, "knowledge-embedder-cache", func(_ context.Context, e events.OrgMemberChanged) {
		if e.OrgID != "" {
			s.applyEmbedderDrop(embedderDrop{org: e.OrgID})
		}
	}, eventbus.Replicated(func(e events.OrgMemberChanged) []eventbus.Invalidation {
		if e.OrgID == "" {
			return nil
		}
		return embedderInvalidation(embedderDrop{org: e.OrgID})
	}, "rag.embedders"))
	eventbus.Subscribe(bus, eventbus.Sync, "knowledge-embedder-cache", func(_ context.Context, e events.SettingsChanged) {
		s.applyEmbedderDrop(settingsDrop(e))
	}, eventbus.Replicated(func(e events.SettingsChanged) []eventbus.Invalidation {
		return embedderInvalidation(settingsDrop(e))
	}, "rag.embedders"))
	// A peer's change: the scope or org it names, every memo for an empty scope (a resync).
	bus.OnRemote("rag.embedders", func(sc eventbus.Scope) {
		switch {
		case sc["org"] != "":
			s.applyEmbedderDrop(embedderDrop{org: sc["org"]})
		case sc["scope"] != "":
			s.applyEmbedderDrop(embedderDrop{scope: sc["scope"]})
		default:
			s.applyEmbedderDrop(embedderDrop{all: true})
		}
	})
}

// corpusKeyed keys an org's identity-relative memo (spec) at its current corpus generation; the zero Key (uncached)
// without an org or an identity.
func (c *searchCaches) corpusKeyed(spec *cache.Spec, orgID, identity string) cache.Key {
	if orgID == "" || identity == "" {
		return cache.Key{}
	}
	k, err := spec.Key(cache.Org(orgID), cache.Param("identity", identity), cache.Version(strconv.FormatUint(c.gen.Load(orgID), 10)))
	if err != nil {
		return cache.Key{}
	}
	return k
}

// corpusSettling reports whether orgID's corpus was invalidated within corpusSettleTTL: an upload's chunks may still
// sit in its uncommitted transaction, so what is read now is not stored.
func (s *KnowledgeService) corpusSettling(orgID string) bool {
	k, ok := corpusCountKey(orgID)
	if !ok {
		return false
	}
	n, hit := s.caches().counts.Get(k)
	return hit && n == corpusUnknown
}

// identityCounts is the org's chunk census relative to identity (CountKnowledgeChunksByIdentity), cached per corpus
// generation: the knowledge search reads it before every query embedding (ErrEmbedderMismatch), and the empty-result
// census reads it again. Not stored while the corpus settles or when an invalidation raced the read.
func (s *KnowledgeService) identityCounts(ctx context.Context, orgID, identity string) (interfaces.KnowledgeChunkCounts, error) {
	c := s.caches()
	read := func(ctx context.Context) (interfaces.KnowledgeChunkCounts, int64, error) {
		counts, err := s.store.CountKnowledgeChunksByIdentity(ctx, orgID, identity)
		return counts, 1, err
	}
	if s.corpusSettling(orgID) {
		counts, _, err := read(ctx)
		return counts, err
	}
	return c.census.GetOrFill(ctx, c.corpusKeyed(identityCensus, orgID, identity), read, &c.gen, orgID)
}

// strandedByDocument is CountStrandedKnowledgeChunksByDocument cached per corpus generation, as identityCounts.
// The map is shared by the callers: they only read it.
func (s *KnowledgeService) strandedByDocument(ctx context.Context, orgID, identity string) (map[string]int, error) {
	c := s.caches()
	read := func(ctx context.Context) (map[string]int, int64, error) {
		m, err := s.store.CountStrandedKnowledgeChunksByDocument(ctx, orgID, identity)
		return m, 1, err
	}
	if s.corpusSettling(orgID) {
		m, _, err := read(ctx)
		return m, err
	}
	return c.strandedByDoc.GetOrFill(ctx, c.corpusKeyed(strandedDocs, orgID, identity), read, &c.gen, orgID)
}
