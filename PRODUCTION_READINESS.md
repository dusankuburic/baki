# Whole-App Production-Readiness Review — Not-Ready Features

## Context

A deep-dive review across the whole application (auth/tenancy, core product, infra/ops) to
list what is **not production-ready**. Three review passes were run and then **verified
against the code** — several "blocker" claims turned out false (see "Corrected" at the end),
so this list is the calibrated result. The dominant real theme is **per-instance in-memory
state** for a few features that breaks (or degrades) when more than one backend replica runs
(the deploy target is Azure Container Apps, which can scale > 1). Severity assumes a
multi-replica cloud deployment; single-replica makes the scaling items moot.

Most security-critical cross-replica state is **already** Postgres-backed (token revocation,
WS single-use tickets, daily AI usage, dashboards) — the in-memory variants are the
local/desktop path. The gaps below are the genuine remainder.

---

## HIGH — fix before multi-replica production

1. **Rate limiting is per-instance, in-memory** (`internal/api/middleware/ratelimit.go`). No
   shared/edge limiter, so the effective limit multiplies by replica count and brute-force/
   DoS protection weakens as you scale. (Per-IP only; no per-account limit either — partly
   mitigated by DB-backed account lockout.) → shared limiter (Redis) or enforce at the ACA
   ingress.
   ✅ **Partially addressed:** rate limiting now has an optional Redis backplane. When
   `PAD_REDIS_URL` is set, every limiter uses an atomic Lua token bucket shared across
   replicas (the effective limit no longer scales with replica count); unset keeps today's
   in-memory behavior unchanged. The fail-open-on-Redis-error trade-off is documented. Items
   #2 and #3 will reuse the same backplane; per-account limiting is still TODO.
2. **WebSocket presence / live collaboration is an in-memory hub**
   (`internal/websocket/hub.go`). Users on different replicas never see each other's
   presence; the collaboration-indicator feature silently only works within one replica.
   Needs sticky sessions or a pub/sub backplane (Redis/Azure Web PubSub) — otherwise the
   feature is effectively single-instance.
   ✅ **Addressed:** the hub now takes an optional Redis backplane
   (`internal/websocket/backplane.go`, wired via `NewHubWithRedis` when `PAD_REDIS_URL`
   is set). Room broadcasts fan out over Redis pub/sub (with origin-dedup so a replica
   ignores its own echo) and presence lives in a shared per-room hash with
   heartbeat-refreshed TTL, so users on any replica see each other. Nil client keeps the
   in-memory single-replica path unchanged; fail-open on Redis errors. Cross-replica
   propagation is covered by miniredis tests (`backplane_redis_test.go`).
3. **Live chat-stream resume is in-memory** (`internal/service/chat.go` `activeStreams`/
   `finishedStreams`). Resume only works if the client reconnects to the same replica; on
   any other replica it silently can't resume. Conversation *history* is Postgres-backed so
   no data loss, but the resume UX is broken across replicas without sticky routing.
   ✅ **Addressed:** stream state is now mirrored to Redis (`internal/service/chat_resume.go`,
   enabled via `SetResumeBackplane` when `PAD_REDIS_URL` is set). `ResumeStream`/`OwnerOf`
   fall back to the shared store so a client reconnecting to a different replica resumes
   the buffer and passes the owner authz check. Nil client keeps the single-replica
   in-memory behavior; mirroring fails open.
4. **No account deletion / data retention / DSAR** (privacy & compliance — applies
   **   regardless of replica count). There is no user-erasure endpoint (`DeleteUser` doesn't
   exist), no admin/self-service deletion, and no data-export endpoint for data-subject
   access. No retention policy or auto-purge on `audit_events` (keeps email + IP
   indefinitely), `refresh_tokens`, `token_blacklist`, or expired `org_invites`. Flow
   cascades cover dependent flow rows but not a user's `audit_events`,
   `flow_versions.created_by`, or `finding_status.updated_by`. → blocker if GDPR/CCPA scope
   applies; add erasure + DSAR export + retention/purge.

## MEDIUM — reliability / cost / completeness

5. **Audit log is best-effort and drops events when the queue is full**
   (`internal/api/audit.go:129-132`, 256-buffer `default:` drop). Not usable as a compliance/
   forensic system of record under load → bounded backpressure or a durable sink.
6. **AI budget enforcement fails open** (`internal/service/chat.go`): if `GetDailyUsage`
   errors it's treated as 0 → unlimited spend during a DB hiccup. Cost-control risk.
7. **Unbounded search-index cache** (`internal/service/flow.go` `idxCache`): a per-flow
   search-index map with no size bound and no eviction → gradual memory growth / OOM on
   large libraries over long uptime. Add LRU + size cap. (The AST cache is bounded — see
   "Corrected".)
8. **API tokens can be created with no expiry** (`internal/api/handlers_apitoken.go` —
   `ExpiresAt` set only when `ExpiresInDays > 0`). Indefinite PATs are a credential-hygiene
   risk → enforce a max lifetime / sensible default.
9. **Graceful-shutdown gaps:** the governance scanner uses a per-*tick* timeout, not
   per-flow, so a slow flow can delay shutdown toward the fx 25s `StopTimeout`
   (`internal/scanner/scanner.go`, `main.go`); and the Tauri sidecar is `kill()`ed without a
   SIGTERM grace period (`src-tauri/src/lib.rs`), risking an interrupted write on app close.
10. **Blob readiness is a hard 2s fail with no tolerance** (`internal/api/handlers_system.go`
    readiness): a transient Azure latency spike flaps the pod out of rotation. Allow N
    consecutive failures before reporting not-ready.
11. **DB migrations are forward-only with no versioning or rollback**
    (`internal/storage/database/postgres_migrations.go`): the schema is one embedded `const
    schema` string applied at boot under a session advisory lock; there is no `migrations/`
    directory, no version table, and no `down` path. A bad DDL change can't be rolled back
    without manual SQL, and there is no CI gate on migration reversibility. → keep
    forward-compatible DDL discipline, or adopt a versioned tool (golang-migrate/goose) with
    tested down-migrations.
12. **No Postgres backup automation / verified DR.** Blob soft-delete + versioning is
    tracked separately below, but database backup/restore is operator-only — no in-code
    backup job, no PITR wiring/verification, no restore drill. → confirm managed-DB backups
    + retention/point-in-time and run a restore test; add a DR runbook.
    ✅ **Addressed:** the backup/restore policy is now documented end-to-end in
    [`docs/DR_RUNBOOK.md`](docs/DR_RUNBOOK.md) (RPO/RTO, PITR + geo-restore + deleted-server
    restore, quarterly restore-drill, cutover, break-glass), and the backup config is
    reproducible-as-code in [`infra/postgres.bicep`](infra/postgres.bicep) (geo-redundant
    backups + pinned retention window). Operators must still run the first restore drill and
    confirm the managed-DB settings match the runbook targets.
13. **Azure Container Apps infra is not defined as code.** Deployment uses
    `azure/container-apps-deploy-action@v2` against ACA resources referenced via GitHub
    secrets; there is no Terraform/Bicep/ARM checked in for the ACA environment/app, so
    provisioning is manual/external. → add IaC for reproducibility and disaster-recreate
    (and to pin the `securityContext` for item 19).

## LOW — hardening

14. **Missing pagination on a few list endpoints** — knowledge documents
    (`handlers_org.go handleKnowledgeList`) and collaborators (`handlers_sharing.go`) return
    unbounded lists. (Note: flow *version* list **is** clamped — not a gap.)
15. **RAG has no max-chunk cap** (`internal/rag/service.go`) → a large upload can fan out into
    many embedding calls (cost/rate-limit spike). Bound chunk count.
16. **No per-user concurrent-upload throttle** on flow upload (`handlers_flow.go`) — relies on
    the global body limit + the 50 MiB blob cap. Add a per-user concurrency guard.
17. **Dockerfile base not digest-pinned** (`Dockerfile` `alpine:3.x`) while the prod DB image
    is pinned — pin the app base too.
18. **No aggregated error reporting.** Tracing (OpenTelemetry), metrics (Prometheus), and
    structured logging are all in place, but there is no exception-aggregation sink
    (Sentry / App Insights exception SDK) — errors live only in logs/traces. → add a sink for
    crash/error triage.
    ✅ **Addressed:** a concrete OTel exception sink (`internal/telemetry/errsink.go`) is now
    registered into the `errreport` funnel at startup (when an OTLP exporter is configured).
    Recovered panics and reported errors are emitted as OpenTelemetry exception events — on
    the live request span when present, else a standalone span — so Azure Application
    Insights (or any OTLP backend) aggregates them. No new external SDK; metrics-only default
    when no exporter is set.
19. **Container hardening beyond the base digest:** the final image is non-root with a
    healthcheck, but it is Alpine (not distroless), pulls `wget` only for the healthcheck,
    and sets no `read-only` fs / `tmpfs`, `no-new-privileges`, or `cap_drop ALL`. The latter
    three are ACA `securityContext`-level — verify at deploy time.
20. **Minor hardening notes:** (a) Postgres `sslmode` is recommended in `.env.example` but
    **not enforced** by the app (operator responsibility); (b) CSP is applied to SPA HTML
    only and allows `'unsafe-inline'` styles; (c) `RuntimeConfig` retry/circuit-breaker
    fields are unused — the AI client uses package-level constants instead.
21. **First-user-becomes-admin includes SSO JIT provisioning**
    (`postgres_users.go tryCreateUser`): on a fresh deployment with an empty `users` table,
    the first identity to authenticate — including via SSO — is silently promoted to admin.
    Operational note: register/provision the intended admin **before** exposing the
    deployment or enabling SSO for end users.
22. **`share_tokens` has no RLS backstop** (deliberate — the public `/api/shared` viewer
    resolves tokens with no authenticated user). All authenticated `share_tokens` queries
    rely solely on handler-level flow authz; a guard comment now sits on the table DDL in
    `postgres_migrations.go`.

## Azure Blob (tracked separately — already reviewed/hardened)

Code is hardened + CI-validated (DB↔blob E2E against Azurite), but still **not deployed/
validated**: changes are uncommitted; the Managed-Identity DB connection is unverified
against real Azure; the storage account needs provisioning (container, RBAC, soft-delete +
versioning, lifecycle policy). See prior tracks — this remains open.

---

## Corrected — agent claims that are NOT real gaps (verified false)

- ❌ "Token revocation / logout not distributed" — **false**: `internal/storage/database/
  postgres_blacklist.go` is the cloud blacklist across replicas; in-memory is local mode.
- ❌ "WS ticket replayable across replicas" — **false**: `ConsumeWSTicket` marks single-use via
  the shared blacklist (`AddIfAbsent`) — single-use across replicas (`router.go:227`).
- ❌ "ListFlowVersions unbounded → DoS" — **false**: clamped to ≤200 (else 50) in
  `postgres_flows.go`.
- ❌ "SSO email-linking enables account takeover" — **false**: links only on an IdP-*verified*
  email and strips the password of an unverified local account (`handlers_sso.go
  resolveSSOUser`) — it explicitly defends the pre-registration takeover vector.
- ❌ "RLS middleware commits a partial write when rollback panics" — **overstated**: commit
  happens only on a normal `<400` return; the panic path never commits.
- ❌ "No admin-promotion endpoint → multi-admin needs DB access" — **false**:
  `PUT /api/admin/users/{id}/role` (`handlers_admin.go handleAdminUserRole`) lets an existing
  admin promote any user to admin (validated role, last-admin demotion guard, audit-logged).
  Only the zero-admin break-glass recovery (no admins left at all) needs direct DB access.
- ❌ "AST cache is unbounded → OOM" — **false**: `ProvideASTCache` builds an LRU bounded to
  100 entries (`NewLRUCache(100)`, `internal/di/services.go:34`) with a 24h per-entry TTL.
  Only the `idxCache` search-index map in `service/flow.go` is unbounded.

## Since-implemented (verified in source — no longer open)

A later working-tree pass closed most of the list. Verified present in code:
- **Item 4** — self-service erasure (`DELETE /account`) + DSAR export (`GET /account/export`,
  `handlers_auth.go`) + retention purge job (`initRetentionPurge` in `main.go`,
  `postgres_retention.go`).
- **Item 6** — AI budget now **fails closed** on a usage-read error (`chat.go enforceBudget`).
- **Item 8** — API tokens capped: default 90 / max 365 days (`handlers_apitoken.go`).
- **Item 13** — ACA is defined as code (`infra/main.bicep`: managed env + container app).
- **Item 14** — pagination on knowledge docs + collaborators (`clampListLimit`).
- **Item 15** — RAG chunk cap (`maxKnowledgeChunks = 500`, `rag/service.go`).
- **Item 16** — per-user upload concurrency guard (`uploadLimiter`, `handlers_flow.go`).
- **Item 17** — Dockerfile stages digest-pinned (`@sha256`).
- **Item 20a** — `sslmode` enforced (`RequireSSL` refuses insecure DSN, `postgres_storage.go`).

Plus items **1, 2, 3, 18** above (see the ✅ notes). Genuinely still open: **item 5** (audit
queue still drops on a full buffer, though now logged + metered) and the lower-priority
hardening items (9–11, 19, 20b–c). The **autonomous migration service**
(`handlers_admin.go handleMigrationStart`) remains a 501 stub.

## Scope / how to use this

This is a review deliverable — a prioritized list, not an implementation plan. The
single biggest decision that sets severity: **will production run more than one replica?**
- If **yes** → items 1–3 are real blockers (need shared rate-limit + a pub/sub backplane for
  presence/stream-resume, or sticky sessions as a stopgap).
- If **single replica** (ACA min=max=1) → items 1–3 drop to low; focus on 5–13.
- Item **4** (privacy/retention) applies **regardless of replica count** — it is driven by
  regulatory scope (GDPR/CCPA), not topology.
