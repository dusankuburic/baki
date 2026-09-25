Deep-dive findings delivered (report above). If you want implementation, proceed in this order:

**Batch 1 — bug-risk fixes (highest value-per-effort)**
1. Bound the dashboard cache and wire InvalidateDashboardCache into flow-save/settings paths (internal/service/dashboard.go — currently zero callers, map never evicts).
2. Stop pinning RLS transactions across blob downloads: prefix-exempt /api/library content routes or move the blob fetch post-commit (rls_middleware.go + handlers_library.go).
3. Rewrite the health-trend query to resolve owner flow IDs before joining flow_analysis_history (postgres_dashboard.go:301) — kills the cross-tenant scan.

**Batch 2 — analyzer performance (benchmarked, incremental)**
4. Memoize FlowHash on the immutable FlowDocument (cache.go:48, computed twice per analysis today).
5. Parallelize the sequential 45-rule dispatch: precompute sigCache in collectBlocks, shard the block walk (engine.go:646-700) — gate on BenchmarkRunAnalysisLarge + adversarial benchmarks.
6. Cache SuppressionInventory (full uncached re-analysis today).
7. Trim per-Check allocations (patternFlagged/seen maps, ToLower in CustomRule, taint DFS memoization, dedup finding copies).

**Batch 3 — architecture refactors (no behavior change)**
8. Extract the fix engine (~700 lines) from service/flow.go into its own type.
9. Service-ify triage/comments/sharing (business logic out of handlers).
10. Adopt narrow role interfaces in the 8 services taking fat StorageBackend; split postgres_flows.go per domain.

Each batch ships with its tests; existing benchmarks and contract suites act as regression gates. Leave all documented trade-offs (RLS design, rate limiting, provider embedding) untouched.