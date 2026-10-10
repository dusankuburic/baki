# 17. Declared caches: a key cannot be built without its tenant

Date: 2026-10-08. Status: accepted.

## Context

Every cache built its own string key. The analyzer's report cache was a process global keyed by content and rule
digests but not by tenant, so `POST /api/analysis/analyze-raw` served one caller's report (root name, document id,
block ids) to another who posted the same files, and every repeat of identical content returned block ids the
caller's document does not have. The suppression inventory was keyed by content only and served another flow's line
numbers; the org rule profile was filled under whichever caller's row-level-security scope came first. Ten caches
were built directly with `cache.NewLRUCache`, `cache.NewSizedLRU` or golang-lru, about twenty more were hand-rolled
maps, and thirteen `singleflight.Group`s deduplicated fills on keys of their own choosing, which is only safe when
the flight key is as fine as the cache key. Nothing listed the caches or what separates their entries, none had
metrics, and `pad-core` built only through `go.work` (it imported `golang.org/x/sync` without requiring it).

## Decision

- **A cache kind is declared once, at package level:** `cache.Declare(name, cache.Keys(kinds...), ...)`. The name is
  dot-separated lowercase segments (`analyzer.reports`, `flow.search-index`). The keys must carry a `KindTenant` part
  unless the cache says why it is deliberately tenant-less with `cache.Global(reason)`; both at once is an error.
  Every declaration also says where its entries live: `cache.ReplicaLocal(reason)` (why per-replica staleness is
  acceptable) or `cache.Shared(mechanism)` (reserved for Phase 5b). A malformed, duplicate or incomplete declaration
  panics, so the boot and every test of the package fail at init.
- **Keys are built from typed parts:** tenant (`Org`, `User`, `OrgUser`, `Tenant(org, user)`, `Local`), `RuleProfile`,
  `Content`, `Resource`, `Version` and `Param(name, value)`. A constructor given empty input returns the zero part.
  `(*Spec).Key(parts...)` accepts exactly the declared parts in any order (same-kind parts keep their argument order)
  and encodes them tagged and length-prefixed in the declared order, so distinct tuples never collide (fuzzed by
  decoding every key back into its parts).
- **A key error never fails a request.** A missing, extra or zero part returns an error wrapping
  `cache.ErrInvalidKey`; the caller computes without the cache and the rejection is counted. In a test binary
  (`testing.Testing()`) `Key` panics instead, so the test that hit a missing tenant fails;
  `cache.SetKeyErrorPanicForTesting(false)` lets a test assert the production path.
- **Storage:** `cache.New[V](spec, budget, opts...)` builds a `*cache.Typed[V]` over `SizedLRU` (byte budget, entry
  cap, default TTL). `GetOrFill(ctx, key, fill, guard, fence)` runs one fill per full key, so a flight never joins
  callers the key separates. It stores the result only if `guard.FillOK` agrees for the fence captured before the
  fill, asked under the cache's lock in one step with the store, so an invalidator that bumps the fence before it
  deletes the key cannot slip between them (invalidators keep that order). `cache.FillGuard` is the method set of
  `eventbus.Gen[string]`, which `pad-core` cannot import. A fill whose
  leader's context ended is neither stored nor handed out, and a live joiner fills again. Caches whose storage is a
  domain structure (the analyzer's per-flow report store, TTL maps with sweeps) keep it, build their keys through
  their spec and record through `RecordHit`, `RecordMiss`, `RecordStore`, `RecordEviction` and `cache.Track`.
- **Statistics live on the spec** as atomics summed over every instance (storage is tracked weakly, so per-test
  instances drop out). `internal/metrics` reads `cache.Specs()` at scrape time: `pad_cache_requests_total{cache,
  result=hit|miss|rejected}`, `pad_cache_stores_total{cache}`, `pad_cache_evictions_total{cache,
  reason=capacity|expired|replaced|deleted}`, `pad_cache_entries{cache}` and `pad_cache_cost_bytes{cache}`. The
  `cache` label is the declared name, a closed set checked by the cardinality test. `pad-core` has no Prometheus
  dependency.
- **The registry is reviewed like the route policy.** `internal/di/testdata/caches.golden` lists every declaration
  (name, parts, tenant or global reason, locality); refresh with `go test ./internal/di/ -run TestCacheRegistry_Golden
  -update-caches`. A companion test fails when a `Declare` in the source is not linked into that test binary.
- **Ratchets** in `internal/archtest/caches_test.go`, scanning `core/` as well: raw cache constructions per file
  (`cache.NewLRUCache`, `cache.NewSizedLRU`, `golang-lru` constructors; 10 before Phase 4a, target: the undo ring
  only), `singleflight.Group` references per file (13 before Phase 4a), and `cache.Declare` only as a package-level
  var with a literal name unique across both modules. `core/cache` itself is not counted.
- **Why `Declare` plus `New(spec)`** rather than registering in `New(name, parts...)`: services are constructed many
  times in tests, so registration at construction cannot tell a duplicate name with a different meaning from a
  second instance, and a golden could list only the caches some test happened to construct. Package-level
  declarations all exist once their packages are imported. `New` taking the declared spec is the plan's
  `cache.New(name, keyParts...)` with the name and parts written once.

## Consequences

A new cache names its tenant or argues in one line why it has none, and that line shows up in a golden diff. A key
built without its tenant fails tests loudly and, if it ships, degrades to an uncached computation visible as
`result="rejected"`, never to a shared entry. The declared parts are the contract for Phase 5b: each cache already
says whether it can tolerate per-replica staleness. `pad-core` builds standalone (`GOWORK=off go build ./...` in
`core`). `core/cache` imports `testing` for `testing.Testing()`. For count-bounded caches `pad_cache_cost_bytes`
counts entries. The existing caches move onto declared specs in later steps of Phase 4a: the analyzer's report and
suppression-inventory caches become an injected `*analyzer.CachedAnalyzer` keyed by tenant, resource, version, rule
profile and content plus parse identity; the service caches follow, and the ratchets are lowered as they go.

## Analyzer cache

The first caches on declared specs are the analyzer's, `analyzer.reports` (tenant, resource, version, rule
profile, content) and `analyzer.suppression-inventory` (tenant, version, rule profile, content). They live in an
injected `*analyzer.CachedAnalyzer`; `DefaultCache` and the package-level `CachedAnalysis*`, `LookupCachedReport`,
`SuppressionInventoryCached` and `RunBatchAnalysis` are gone.

- **One analyzer per process.** `internal/di.ProvideAnalyzer` builds it (50 reports within 96 MiB, 64
  inventories) and hands it to the analysis service, which serves `Analyze`, `Lookup`, `SuppressionInventory` and
  `Batch` from it, and to the flow service (`service.WithAnalyzer`), which drops reports on a reimport, on
  `FlowDeleted` (`InvalidateFlow`, every tenant's entries of the flow) and when the desktop opens a folder
  (`ResetTenant(cache.Local())`). A nil or option-less analyzer analyzes without caching.
- **Tenant.** `analyzer.Input.Tenant` is chosen by the service: `cache.Local()` without accounts (the desktop, local
  mode), else the stored flow's organisation or owner (`cache.Tenant`), so every reader of a flow shares its entry,
  else the caller (another path-less input). With none of them the part is zero and the analysis runs uncached,
  never joining another caller's walk. The desktop session analytics are `Reports(cache.Local())`: another tenant's
  reports cannot reach them by construction. Analyze-raw (`AnalysisService.AnalyzeRaw`) no longer records into the
  per-flow diff pair or the history store, but still counts in `pad_analysis_runs_total`; with accounts it runs
  uncached (zero tenant), because each post is a fresh parse that no later request hits and its entry would only
  evict stored flows' reports. Without accounts it keeps feeding the session analytics.
- **Content and parse identity.** The content part is `FlowHash` (unchanged, still persisted as `ContentHash`) plus
  a digest of what the report carries from the parse: the document id and name and every subflow id, block id and
  line number, computed in the same walk and memoized with `FlowHash`. A report is served only for the parse it was
  computed on (the same document, a copy, or a stored flow resolved again with its stored ids). A re-parse of
  unchanged text, and a whitespace-only edit, analyze again instead of receiving another parse's block ids and line
  numbers: analyze-raw SARIF keeps its line numbers on every repeat, and an inventory never shows another flow's
  lines. The report store keeps its domain rules: one entry per (tenant, flow), and a folder and the files beneath
  it replace each other. The second digest costs about 0.5 ms per 8,000 blocks on a document's first hash
  (`BenchmarkComputeFlowHash`, about +45% over the content hash alone; a block's subflow id is hashed only where it
  differs from its subflow's). A desktop folder reload re-parses with new ids, so it analyzes again; the diff pair
  keeps its previous distinct run across such a reload (same content and findings).
- **Rule profile.** `analyzer.ProfileDigest(settings, rules)` replaces the settings and rule-set digests: each rule's
  ID, `RuleDigest`, effective enabled state (`RuleEnabled`, so `DisabledByDefault` counts for an absent settings
  entry) and the stage, revision, confidence and fix type registered under its ID (the engine stamps them on
  findings), the parse-error fallback's, and the rule settings with every option value tagged by its type (`"8"` and
  `8` differ, as they do for the rules). Order-insensitive, full SHA-256, in memory only. A metadata flip
  (`SetRuleMetaForTesting`) therefore misses the cache; nothing needs clearing. `scanner.DigestRuleSet` delegates
  to it.
- **Flights and statistics.** One walk per full report key at a time (a flight never joins callers the key
  separates); a cancelled leader's walk is neither stored nor handed out, as before. The report store records hits
  (`Lookup` and `Analyze`), misses (`Analyze` only, so a probe followed by an analysis counts once), stores and
  evictions on its spec and is tracked for the size gauges; the inventory is a `cache.Typed`.

Hit rate: analyze-raw posts and re-parsed desktop files no longer hit an entry computed for another parse;
stored flows, desktop reloads served from the parsed-document cache and repeated views keep hitting.

## Service caches

The backend's own caches follow on declared specs. Each builds its keys through its spec; where the storage is
`cache.Typed` the spec also counts and sizes it, and the caches that keep their own storage (an invalidation by
something no key part names, a TTL per entry, a test clock, a value served after a failed refresh) call the
`Record*` methods, and `cache.Track` where the storage has a size.

| Cache | Key parts | Storage | Notes |
|---|---|---|---|
| `flow.ast` | local user (or the uploading caller), source (`file`, `folder-member`, `upload`), digest of name and bytes | `Typed`, 96 MiB, 24 h | desktop and local mode only; the file name is in the key (B6) |
| `flow.upload-ids` | caller, digest of file names and contents | `Typed`, 1024, 24 h | cloud: an identical re-upload updates the flow it minted |
| `flow.search-index` | flow id; Global | `Typed`, 48 MiB / 64 | served only for the document object it was built from |
| `flow.documents` | flow id; Global | `Typed`, 160 MiB, 48 MiB per entry | served only at the stored version the caller's header read names |
| `chat.context`, `chat.scrubbed-docs` | caller, flow, edit generation (+ the texts' inputs) | `Typed`, 32 and 4 | |
| `chat.history-scrubs` | caller, SHA-256 of the text | own LRU (entries + bytes) | per caller: a shared hit would time another user's text |
| `chat.token-counts` | provider, SHA-256 of the text; Global | own LRU | content-addressed on purpose: a count reveals a text only by timing, and system prompts repeat across users |
| `rag.corpus-counts` | org | `Typed` with `GetOrFill` (the corpus generation as guard), 30 s | an invalidation stores a 10 s "not empty" marker |
| `rag.query-embeddings` | caller in the org, embedder, SHA-256 of the query | `Typed`, 256, 10 min | |
| `dashboard.home` | user, epoch | `Typed` with `GetOrFill` (the home generation as guard), 1024, 30 s | the epoch retires every entry at once |
| `dashboard.analytics` | user, admin flag, query | own map | dropped by the owner and by every org a view spans; the admin flag is in the key (B7) |
| `rules.profiles` | org (reads without a request scope) or the caller in the org | own map, 1024, 30 s | per reader (B4); a member's entry goes when their membership changes |
| `rules.deployment-profile` | none; Global | own map | the profile without an org |
| `settings.deployment` | none; Global | the provider's own | serves the last value read when a refresh fails |
| `providers.model-listings` | credential scope (user, org, local user), provider | own map | per-key and per-scope invalidation marks, a TTL per listing |
| `azure.owner-memos` | registry owner, memo | own maps | the import marker, the import deferral, the orphan-sweep rate limit; the service's test clock |

- **B4, rule profiles per reader.** An org's custom rules are visible to its members only (row-level security), so
  a resolve under a non-member collaborator's request scope reads none. That resolve used to fill the org's shared
  profile and hide the rules from every member for the TTL, and a member's resolve showed them to the non-member.
  A profile is now cached for the view its read showed: in a request's scope, its caller's (`OrgUser`); without
  one, on the pool, every row, whoever asks, so the org's own (`Org`), which background jobs share. The resolver
  asks the context (`reqtx.ReadsFiltered`, which the Postgres scope answers; a fail-open scope that could not start
  counts as the pool, and a resolve that fell through to it is not stored as the caller's). Each reader gets exactly
  what its read shows; members no longer share one resolve (one per member per org per 30 s). A scoped resolve
  also reads the caller's membership (`OrgMemberRole`), and a non-member's profile is `Narrowed`: an analysis under
  it is the reader's alone, cached under `OrgUser` and recorded in none of the flow's shared state (the diff pair
  that exports, the diff and the chat read, the history, the dashboard claim), which it used to replace with a
  report lacking the org's rules (final review, caches-1). The one scoped route that persists an analysis,
  `POST /api/analysis/baseline/set`, refuses a narrowed caller (403, `AnalysisService.ReadsOrgView`): its keys
  and rule set would file the org rules' findings as new rules, not drift.
  An org's invalidation drops every reader's entry, a membership change (added, removed) the member's own and fences
  the org's resolves in flight, and flights are keyed like the cache. A request on a route without row-level
  security (`/api/analysis/analyze` and the other heavy routes) reads on the pool and so still sees the org's
  rules there, and only there: such a read fills the org's entry, never the caller's, so it is not served on a
  scoped route (baseline, policy). Whether a non-member collaborator should run the rules on the exempt routes is
  not decided here (it needs a membership check on the request path, ADR 0013).
- **B6, file names in the parsed-document key.** The parse depends on the name: the flow's name, the implicit
  subflow's, every subflow's `SourceFile`. The single-file key was the bytes alone, so `b.txt` with the bytes of an
  earlier `a.txt` opened as flow "a"; an upload's key ignored its root name. The digest now covers the name (and the
  root name), and the source is a key part, so a folder member and a single file never share an entry. A desktop
  "fix all" parses the file under its file name, as a load does, so the document it seeds is the one a reload of the
  written bytes would parse (it used to be parsed under the flow's name: `my.flow.txt` reloaded as flow "my").
- **B7, the admin flag in the analytics key.** The payload's instance-wide locked-account count depends on whether
  the caller is an instance admin; sessions of one user with and without the role no longer share a view or a
  flight. A rule-catalog change still waits out the 30 s TTL.
- **Flights.** `rule_profile.go`, `document_provider.go`, `flow.go` (search indexes) and `dashboard.go`
  (analytics) keep a `singleflight.Group`, each keyed exactly like its cache: their entries are served only after a
  check `GetOrFill` cannot make (an org's invalidation across readers, the stored version, the exact document
  object, the orgs a view spans). The rule-profile and analytics flights used to be coarser than the key they now
  carry (the org without the reader, the user without the admin flag); the home flight moved into `GetOrFill`, and
  the corpus count gained one there.
- **Ratchets.** Raw cache constructions are down to the undo ring (`flow_snapshots.go`, state rather than a memo);
  `singleflight.Group`s to 11. The memos left off the registry are listed with their reason in
  `internal/archtest/caches_test.go` (`undeclaredMemos`, checked to still exist): memos of configuration or of
  immutable artifacts (blob health, the IdP's discovery document, egress proxy verdicts, the SPA shell, time zones,
  trusted-proxy sets, struct reflection) and the token revocation set. The `internal/ai` memos stay for Phase 4b.
