// Package cache holds the process-local caches of the analyzer and the backend.
//
// A cache kind is declared once, at package level, with the parts every one of its keys carries:
//
//	var reportSpec = cache.Declare("analyzer.reports",
//		cache.Keys(cache.KindTenant, cache.KindRuleProfile, cache.KindContent),
//		cache.ReplicaLocal("derived from content and rule profile; recomputed on a miss"))
//
// A key is built from typed parts (Org, User, Local, RuleProfile, Content, ...) through the spec, so a key without
// its tenant cannot be built: (*Spec).Key refuses it, and the caller computes without the cache. A cache that is
// deliberately tenant-less says why with Global(reason). A spec also says where its entries live: ReplicaLocal(reason),
// each replica holds its own and the reason says why that is acceptable, or Shared(MechanismInvalidation, reason), each
// replica holds its own but a change on any replica drops the entry on every replica (the invalidation fan-out, ADR 0032).
// New builds the default storage for a spec, a typed LRU bounded by cost, with GetOrFill for fills deduplicated on the
// full key. Every spec keeps hit, miss, store and
// eviction counts, which internal/metrics exports, and Specs lists the declarations for the registry golden
// (docs/adr/0017-declared-caches.md).
//
// Cache, NewLRUCache and NewSizedLRU are the untyped building blocks; production code outside this package declares
// a spec instead (ratcheted in internal/archtest).
package cache
