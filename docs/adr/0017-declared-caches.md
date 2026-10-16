# 17. Declared caches: a key cannot be built without its tenant

Date: 2026-10-08. Status: accepted.

## Context

Every cache built its own string key. The analyzer's report cache was process-global keyed by content but not tenant, so `analyze-raw` served one caller's report (block ids, line numbers) to another; the suppression inventory served another flow's lines; the org rule profile was filled under whichever caller's RLS scope came first. Ten caches were built raw, ~twenty more were hand-rolled maps, and thirteen `singleflight.Group`s deduplicated fills on keys of their own choosing.

## Decision

- A cache kind is declared once at package level: `cache.Declare(name, cache.Keys(kinds...), ...)`. The name is dot-separated lowercase (`analyzer.reports`). Keys must carry a `KindTenant` part unless the cache says why it is tenant-less with `cache.Global(reason)`; every declaration states locality: `cache.ReplicaLocal(reason)` or `cache.Shared(mechanism)`. A malformed, duplicate or incomplete declaration panics at init.
- Keys are built from typed parts (tenant: `Org`, `User`, `OrgUser`, `Tenant(org, user)`, `Local`; plus `RuleProfile`, `Content`, `Resource`, `Version`, `Param`), encoded tagged and length-prefixed so distinct tuples never collide. A key error never fails a request: a missing, extra or zero part returns an error wrapping `cache.ErrInvalidKey` (counted as `result="rejected"`); in a test binary `Key` panics instead.
- `cache.New[V](spec, budget, opts...)` builds a `*cache.Typed[V]` over `SizedLRU` (byte budget, entry cap, TTL); `GetOrFill(ctx, key, fill, guard, fence)` runs one fill per full key and stores only if `guard.FillOK` agrees for the fence captured before the fill, asked under the lock in the same step as the store (invalidators bump the fence before deleting). Caches with their own storage build keys through their spec and record via `RecordHit`/`RecordMiss`/`RecordStore`/`RecordEviction`/`cache.Track`.
- Metrics on the spec: `pad_cache_requests_total{cache,result}`, `pad_cache_stores_total`, `pad_cache_evictions_total{cache,reason}`, `pad_cache_entries`, `pad_cache_cost_bytes`. The registry is reviewed via `internal/di/testdata/caches.golden`; archtest ratchets limit raw cache constructions, `singleflight.Group`s, and `cache.Declare` to package-level vars.
- **Analyzer caches:** `analyzer.reports` (tenant, resource, version, rule profile, content) and `analyzer.suppression-inventory` in one injected `*analyzer.CachedAnalyzer` per process. `Input.Tenant` is `cache.Local()` without accounts, else the stored flow's organisation or owner, else the caller; analyze-raw with accounts runs uncached (zero tenant). The content part is `FlowHash` plus a digest of the parse identity (document id/name, subflow ids, block ids, line numbers), so a report is served only for the parse it was computed on. `analyzer.ProfileDigest(settings, rules)` replaces the settings/rule-set digests, folding each rule's id, digest, `RuleEnabled` state, stage, revision, confidence and fix type (so a metadata flip misses the cache).
- **Service caches** follow the same rules; load-bearing key decisions: rule profiles are per reader (B4 — an org's profile read under a non-member's scope is `Narrowed`, recorded in none of the flow's shared state, and `POST /api/analysis/baseline/set` refuses a narrowed caller 403); file names are in the parsed-document key (B6 — `b.txt` with `a.txt`'s bytes must not open as flow "a"); the admin flag is in the analytics key (B7). Flights are always keyed exactly like their cache.

## Consequences

A new cache names its tenant or argues why it has none, visible in the golden diff; a key built without its tenant fails tests loudly and ships only as an uncached computation (`result="rejected"`), never a shared entry. The declared parts are the contract for cross-replica sharing (each cache already states whether it tolerates per-replica staleness).
