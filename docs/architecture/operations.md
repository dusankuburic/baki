# Operating a deployment

Database connections, pooling, the stop budget, running more than one replica and the alerts of a cloud deployment (`infra/main.bicep` on Azure Container Apps, or compose). Decisions: [ADR 0023](../adr/0023-singleton-jobs-and-leases.md) (jobs and locks), [ADR 0024](../adr/0024-graceful-shutdown-and-server-restarting.md) (the stop).

## Database connections

One pool per replica (`storagedb.New`), capped by `PAD_DB_MAX_OPEN_CONNS` (default 25, per replica — the server sees `pool × replicas`, plus the rolling deploy's surge). With `dbServerMaxConnections` set and `dbMaxOpenConns` empty, Bicep derives each replica's pool:

```
min(25, max(1, (dbServerMaxConnections - dbReservedConnections) / (maxReplicas + max(minReplicas, 1))))
```

`dbReservedConnections` (default 20) is left to Azure's reserved connections and an operator's `psql`; the divisor's second term is the deploy surge; at least 1 in the derivation (the app reads 0 as unset and opens 25; in practice at least 3: a smaller pool skips the post-boot upkeep and HNSW build); at most 25; an explicit `dbMaxOpenConns` always wins; `dbServerMaxConnections = 0` leaves the app default. CI variables: `PAD_DB_MAX_OPEN_CONNS`, `PAD_DB_SERVER_MAX_CONNS`, `DB_RESERVED_CONNECTIONS`; `deploy_pool_test.go` evaluates the expression.

### Pinned lock connections

Each running Singleton job pins one pooled connection for its whole run (its session advisory lock, heartbeat 15 s); the vector index build and `schema_upkeep` (after every boot of a database deployment) pin two, so both skip a pool under 3, a schema migration one — out of the pool, not on top. Several jobs run in the first minutes after a boot, so a pool in single digits starves requests: keep it well above that (the default 25 is). Connections are named `baki-backend/<PAD_INSTANCE_ID>`. Lock holders:

```sql
SELECT a.application_name, a.pid, to_hex((l.classid::bigint << 32) | l.objid::bigint) AS key
FROM pg_locks l JOIN pg_stat_activity a USING (pid)
WHERE l.locktype = 'advisory' AND l.granted;
```

Keys are in `jobs.golden`, hex (`62616b697363616e` is "bakiscan"). `knowledge_reembed` runs only on demand (an org admin's re-index with `{"background": true}`, billed, one org at a time — a second start answers 409 `KNOWLEDGE_BUSY` — never started by the server, no period so never overdue).

The knowledge-base org lock is cross-replica: `lockOrg` takes the in-process mutex, then, with a `lock.Leaser`, a lease on `rag.KnowledgeLockKey(orgID)` for the check → embed → write window — key `0x6b6e6f77` ("know") high, FNV-1a-32 of `knowledge_org:<org id>` low; the high word must never be 0 or -1 (the chunk writes take `pg_advisory_xact_lock(hashtext(org_id))`; a colliding lease self-deadlocks). A lost lease cancels and answers busy; a lease that cannot be asked for (database down) answers 503 `KNOWLEDGE_LOCK_UNAVAILABLE` and starts nothing. `PAD_KNOWLEDGE_LEASE_CAP` (default 4) bounds a replica's leases — keep it well below `PAD_DB_MAX_OPEN_CONNS`.

### Idle transactions

Every pooled session carries `idle_in_transaction_session_timeout`: `PAD_DB_IDLE_TX_TIMEOUT`; unset = `PAD_REQUEST_TIMEOUT` + 1 min (`config.DBIdleTxTimeoutMargin`, 90 s by default); a set value must exceed the request timeout (below 45 s can cut a background flow copy waiting on a slow blob upload); `0s` disables. It ends sessions idle between statements (not inside one), freeing row locks. Every route in the request transaction has the default deadline (`TestRoutePolicy_RequestTransactionsHaveTheDefaultDeadline`), so a cut means a handler already past its deadline. It does not count the job locks and leases (idle outside a transaction); migrations and the vector build lift it (`liftSessionTimeouts`); background jobs are short back-to-back transactions. A cut session logs `terminating connection due to idle-in-transaction timeout` and returns SQLSTATE `25P03` on its next statement; the pool replaces the connection — repeated cuts point at a handler ignoring its deadline.

### The connection role

A superuser or `BYPASSRLS` role ignores every RLS policy: in the cloud profile `storagedb.New` refuses to start on such a role (before the migrations would make it the owner) and on a role it cannot read. `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user;` — both must be false; connect as the `baki_app` role of `infra/postgres.bicep` or `scripts/db-init`. `PAD_DB_ALLOW_BYPASSRLS=true` starts anyway for a migration window; local mode only warns.

### The boot checks

`storagedb.New` also refuses a DSN without TLS when TLS is required. These warn and never fail the boot:

| `msg` | When |
|---|---|
| `postgres: connection pools can exceed max_connections` | `PAD_DB_MAX_OPEN_CONNS × max(PAD_MAX_REPLICAS, 1)` exceeds `max_connections − reserved`; counts configured replicas, not the surge (the Bicep derivation leaves that room) |
| `PgBouncer transaction mode is unsupported: session advisory locks` | the `PAD_DATABASE_URL` host uses port 6432 |
| `postgres: connection role bypasses Row-Level Security — the RLS policies in this schema are INERT` | superuser or `BYPASSRLS` outside the cloud profile or with `PAD_DB_ALLOW_BYPASSRLS=true` |
| `schema upkeep skipped: the pool cannot spare the two connections it holds; raise PAD_DB_MAX_OPEN_CONNS` | `PAD_DB_MAX_OPEN_CONNS < 3`: upkeep pins two connections and skips smaller pools; foreign keys stay unvalidated |
| `knowledge vector index: build skipped: the pool cannot spare the two connections it holds; raise PAD_DB_MAX_OPEN_CONNS` | `PAD_DB_MAX_OPEN_CONNS < 3`: index build pins two connections and skips smaller pools; HNSW index is not built |

Server limits are read from the server in one query; when that fails, `PAD_DB_SERVER_MAX_CONNS` stands in (the line's `limits_from`), else the check is skipped.

## Pooling: session mode only

Point the app at the server (5432) or a pooler in **session** mode. Transaction pooling (Azure's built-in PgBouncer on 6432) breaks the session advisory locks (every lease lost at its first heartbeat), the migrations' session-level `SET`, pgx's prepared statements and the startup timeouts. A session-mode PgBouncer on 6432 also trips the boot warning — move it to another port.

## TLS to the database

Use `sslmode=verify-full` (chain and host name); `verify-ca` checks the chain only, `require` accepts any certificate. `PAD_DB_REQUIRE_SSL` (on by default in the cloud profile) refuses `disable`, `allow`, `prefer` and a DSN without `sslmode`; the other modes boot. Azure Flexible Server chains to public roots (DigiCert Global Root G2, Microsoft RSA Root CA 2017; check the Flexible Server TLS page — Azure rotates them); pgx verifies against the system bundle (the image ships Debian's `ca-certificates`), so `postgres://…@<server>.postgres.database.azure.com:5432/baki?sslmode=verify-full` works — keep the server's FQDN in the DSN, the certificate names it (with private networking the same name resolves privately). To pin the roots: concatenate the page's certificates into a PEM readable in the container and add `&sslrootcert=/path/to/azure-roots.pem` (update it before Azure moves to a root it does not hold). psql/pg_dump/libpq tools do not fall back to the system bundle: add `sslrootcert=system` (libpq 16+) or the downloaded bundle; `bakicli` uses pgx. Switching from `require` is a `PAD_DATABASE_URL` change alone. The compose stacks connect `sslmode=disable` with `PAD_DB_REQUIRE_SSL=false` (the bundled `db` serves no TLS).

## Private networking

Recommended production setup: `privateAccess = true` in `infra/postgres.bicep` (vnet with an `aca` subnet for the environment and a `private-endpoints` subnet, private DNS zone, private endpoint; public access off) plus the Container Apps environment in that network (`infrastructureSubnetId`, CI `AZURE_ACA_SUBNET_ID`); the ingress stays public. A private endpoint is added to the running server in place; VNet integration would mean a new server and a restore. An environment cannot join a network once created: to move an existing deployment (a maintenance window), deploy `postgres.bicep` private (the running app loses the database from here), then `main.bicep` under a new `appName` (its smoke test gates on `/readyz`), move the custom domain, delete the old app and environment (and the leftover `AllowAllAzureServices` firewall rule). Afterwards `psql`/`bakicli` reach the server only from inside the network. Run `az deployment group what-if` against a scratch group first: this path is not deployed in these templates.

## Stopping

`fx.StopTimeout` is 28 s; its first pause is `PAD_SHUTDOWN_DRAIN_DELAY` (3 s in the cloud, 0 local/desktop, at most 5 s — a longer value refuses to boot). The platform must not kill the process before the budget: ACA `terminationGracePeriodSeconds` = `stopGraceSeconds` (35 s), compose `stop_grace_period: 35s` on the app service (`deploy_pool_test.go` keeps both above the stop timeout; change them together). A stopping replica answers `/readyz` 503 `{"status":"draining"}` from the start of its drain.

## The audit spill with several replicas

A full audit queue — or a database outage (network error, the 5 s write deadline, SQLSTATE class 08/28/53 or 57P01-57P03) — spills rows whole to `PAD_AUDIT_SPILL_DIR/audit-spill-<instance>.jsonl` (`<instance>` = `PAD_INSTANCE_ID`), replayed once there is room; a row the database refuses for itself goes to the redacted log line (`pad_audit_dropped_total{reason="write_failed"}`). Each file is capped at 10 MB (`SpillMaxBytes`; past it rows go to the log, `pad_audit_spill_dropped_total`). A torn last line from a crash cannot swallow the next event and is skipped on replay (`pad_audit_spill_undecodable_total`). One file per replica; a replica adopts a file nothing has touched for 2 minutes (`.claiming` marker, rename, replay) — only meaningful on a shared volume; inserts are `ON CONFLICT (id) DO NOTHING`, so replays are idempotent. The default `/tmp/baki-audit-spill` is a per-replica `EmptyDir`: nothing shared, a replaced replica's file is lost with it.

## Migration status and the schema version

The **schema** migration runs at boot, serialised across replicas on a blocking `pg_advisory_lock` (the startup probe allows for the wait). The **legacy data migration** (`PAD_DATA_DIR` into the database, `POST /api/admin/migration/start`) is an admin action, one replica at a time.

- `GET /api/admin/migration/status` answers for the deployment from `job_runs` (`legacy_migration`; result in `job_runs.last_detail`, migration 0046): "running" = started after it last ended and no more than an hour ago; a new run clears the previous result; a second start anywhere is a 409. `GET /api/admin/system/health` answers `binaryVersion`, `schemaVersion` and `instanceId` of the replica that served it — during a rolling deploy, call until the answers agree.
- **Migration checksum drift**: `storagedb.New` refuses a database whose applied checksums differ from `testdata/migration_checksums.golden`. Check applied migrations:
  ```sql
  SELECT version, name, checksum, compat FROM schema_migrations WHERE version >= 47 ORDER BY version;
  ```
  | Recorded state | Remedy |
  |---|---|
  | Version with shipped schema shape but drifted text (e.g. pre-release 0048/0049) | `bakicli migrate adopt-checksum <version> [--dsn ...]` |
  | Unknown version or incompatible schema shape | Run `bakicli migrate down <target>` using the release/build that applied it, or apply down SQL files manually and delete `schema_migrations` rows |
  Down migrations must be run with the release that contains the steps (using the release artifact or `go run ./cmd/bakicli` from that checkout; the container image ships no `bakicli`).
- **Migration 0048 pre-deploy checks and refusal**: 0048 refuses to boot (changing nothing) if accounts share an email up to case or invitations share a token. Run preflight before deploying: `bakicli migrate preflight [--dsn ...]` or inspect `GROUP BY lower(email) HAVING count(*) > 1` and `GROUP BY token_hash HAVING count(*) > 1`. Resolve duplicate accounts using the recipe in [deployment.md](../deployment.md#before-deploying-v48). Run the recipe transaction while the previous release is idle or rerun it a minute later (idempotent); otherwise a sign-in straddling the commit can leave a rolling session.
- **`schema_upkeep` finishes 0048 and 0049 after boot**: runs once per boot, serialized by lease. Upkeep budget is 1 h; the subsequent HNSW vector index build gets 2 h. If the connection pool is busy, retries every 5 s doubling up to 1 min within budget. Validates 0048 foreign keys in separate transactions (5 s `lock_timeout`) after deleting orphan rows, and builds/drops indexes concurrently. Unfinished items WARN and retry at next boot; unvalidated keys still refuse new orphans. Check completion:
  ```sql
  SELECT conname, conrelid::regclass FROM pg_constraint WHERE NOT convalidated;
  SELECT indexrelid::regclass FROM pg_index WHERE NOT indisvalid;
  ```
- **Lock timeouts and holders**: steps retry up to 6 times (`WARN schema migration could not get a table lock…`) before failing boot. 0048 and 0049 take locks in all-or-none groups, `users` and `refresh_tokens` last: a sign-in queued behind them waits ≤ deadlock_timeout + 200 ms (~1.2 s) while an autovacuum worker holds the table (displaced after `deadlock_timeout`), and ≤ 200 ms otherwise. Lock holders beside `pg_dump`: autovacuum worker, manual VACUUM/ANALYZE, anti-wraparound vacuum, and `CREATE/REINDEX INDEX CONCURRENTLY`. Diagnose blocking locks:
  ```sql
  SELECT l.pid, l.mode, l.granted, c.relname,
         coalesce(pv.phase, pi.phase, 'waiting') AS phase
  FROM pg_locks l
  JOIN pg_class c ON c.oid = l.relation
  LEFT JOIN pg_stat_progress_vacuum pv ON pv.pid = l.pid
  LEFT JOIN pg_stat_progress_create_index pi ON pi.pid = l.pid
  WHERE l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
    AND l.pid <> pg_backend_pid();
  ```
  Remedy: wait for the operation to complete, or as superuser cancel the blocking backend (`SELECT pg_cancel_backend(pid)`). Note `pg_stat_activity.backend_type` is hidden from the `baki_app` role.

## Redis

Redis is optional (`PAD_REDIS_URL`); `PAD_MAX_REPLICAS` above 1 refuses to boot without it. Down is degraded, not unready: the boot proceeds with a WARN, `/readyz` stays 200 and adds `"degraded":["redis"]`. `pad_redis_up` (0 after a failed ping or three failed operations) is the signal; `pad_redis_errors_total{mechanism}` says which part.

- **What an outage costs:** rate limits and the per-user caps become per replica (N times; `pad_redis_caps_total{outcome="open"}`), collaborators on other replicas stop seeing each other, a chat stream cannot resume elsewhere, a stop or approval for another replica's stream answers 503 `REPLICA_UNREACHABLE`, and — the one refusal — with `AI.DailyBudget > 0` and more than one replica the in-flight ledger fails closed (`AI_BUDGET_UNAVAILABLE`; 503 `BUDGET_UNAVAILABLE` on the knowledge routes; a streaming answer stops at its next re-check) — see [ai-providers.md](ai-providers.md#the-in-flight-budget-ledger-across-replicas-addendum-to-adr-0028-phase-5b). Cache invalidations are not sent: peers serve old settings, rule profiles, corpus counts and dashboards for their TTL (30 s; model listings 10 min) and drop every replicated cache when their subscription is back. A single replica keeps its in-process ledger even with `PAD_REDIS_URL` set.
- **Every key has a TTL** (the registry golden; `TestDeployRedis_VolatileLRUNeedsEveryKeyToExpire`), so the Redis runs `volatile-lru`. The invalidation channel's signals: `pad_inval_gap_total` (message loss), `pad_inval_dropped_total{reason}`, `pad_inval_resync_total{reason}` and `pad_redis_msgs_total{result="unknown"}` (expected during a rolling deploy; `bad` is a bug).
- **SSE and stream commands:** durable events carry an `id:` from a per-user Redis Stream (~200 frames, 5 min) replayed on `Last-Event-ID`; each replica records which users it holds streams for (`baki:sse:subs`), which stops the chat watchdog cancelling a stream watched from another replica. Commands ride `baki:chat:cmd:<instance>`; each replica keeps `baki:replica:live:<origin>` (rewritten every 5 s, 15 s TTL, deleted on a clean stop): a replica that cannot write it for 10 s stops the chat streams the others know of (`STREAM_INTERRUPTED`), and a dead replica's stops answer as ended once the key lapses (≤ 15 s; `owner_gone`). A release without these mechanisms beside one with them runs as separate islands — keep `maxReplicas` at 1 until the whole fleet runs the new release.

## Running more than one replica

The repository ships one replica and no Redis (`minReplicas = maxReplicas = 1`, `provisionRedis = false`); raising it is a deliberate commit once the `two-replica` CI job has been green (`deploy_replicas_test.go` pins the defaults). Behind a load balancer without stickiness every request can land anywhere — nothing needs sticky sessions: Postgres carries the data, locks and versions; Redis carries events, commands, expiring leases and cache drops.

Checklist before `maxReplicas` goes above 1:

1. **Redis.** `PAD_PROVISION_REDIS=true`; with `maxReplicas > 1` the SKU defaults to Standard C1 (Basic has no replica node or SLA — a restart is an outage of every cross-replica mechanism). Harden it: private network, AUTH and `rediss://`, no RDB/AOF, `maxmemory` headroom, `volatile-lru`. Alert on `pad_redis_up == 0`, any `pad_redis_evicted_keys_total` increase (an evicted claim or ledger key is the failure ADR 0029 lists per key), and `pad_redis_bus_rejected_total{reason!="stale"}`. Commands are signed from `PAD_ENCRYPTION_KEY`/`PAD_AUTH_SECRET`: every replica needs the same secrets and clocks within 30 s.
2. **The guard.** `PAD_MAX_REPLICAS > 1` without `PAD_REDIS_URL` is refused at app boot, by the deploy workflow and by `deploy_replicas_test.go`.
3. **A name per replica.** ACA sets `CONTAINER_APP_REPLICA_NAME`; compose names its replicas; elsewhere the host name. Duplicate names still work (a boot suffix keeps the Redis identities apart) but collide in logs, `application_name` and the audit spill file names.
4. **A scale rule.** `scaleConcurrentRequests` (default 100): open event streams and WebSockets count as concurrent requests while open (~50 users per replica; idle tabs scale out). Size it above the open streams a replica should carry.
5. **Database pools.** Set `dbServerMaxConnections` (derivation above; the knowledge leases pin connections).
6. **Alerts.** `alertEmails`: Redis adds `redis-degraded` and `invalidation-channel`; more than one replica adds `replicas-at-ceiling` and `replicas-below-min`.
7. **One release.** Never run a release without these mechanisms beside one with them (an island: events, invalidations and the budget's in-flight view do not cross; a stop for its streams is 503). Deploy at one replica first, rehearse Redis at one replica, raise `PAD_MAX_REPLICAS` (and `PAD_MIN_REPLICAS` for a floor of two) once `two-replica` is green on the release; lowering them again changes nothing in the schema.

What is shared: every per-process state is declared in `stateRegistry` (`internal/archtest/state_test.go`, ADR 0032) — sessions, jobs, conversations (`conversations.version`), the undo ring (`flow_snapshots`) and the migration status in Postgres; rate limits, collaboration rooms and presence, SSE fan-out/replay/holder records, chat stream commands and resume, the AI budget ledger, the per-user caps (10 event streams, 10 sockets, 3 uploads, 3 chat streams per user), cache invalidation (`baki:inval`) and the scan/analysis claims in Redis (every key in `redis_keys.golden` with its `on-down` mode, ADR 0029). Per replica on purpose: its sockets and streams (die with it), provider circuit breakers, the scanner's alert pre-check. Postgres down leaves rotation after three failed probes. A Redis that comes back empty loses the resume snapshots (rewritten by their owner within a second), the pending approvals (not decidable from another replica until their 60 s timeout) and the replay logs; an ended stream's budget reservation lapses with its 30 s lease.

Local rehearsal: `docker compose -f docker-compose.yml -f docker-compose.replicas.yml up -d --build --wait`, then `scripts/ci/two-replica-smoke.sh`.

## Alerts

`infra/main.bicep` provisions them when `alertEmails` is set (log alerts match `Log_s contains '"msg":"…"'`). `restarts` (>3 in 5 min), `error-burst` (>20 ERROR lines in 15 min), `readiness-failing` (per replica, above what a drain can log — a deploy does not fire it), `job-overdue` (`job overdue`, 2 h window: the line repeats at most hourly), `job-lease-lost`/`advisory-lock-lost` (once after a failover is expected; repeated means transaction-mode pooling), `chat-shutdown-workers`, `redis-degraded`, `invalidation-channel`, `replicas-at-ceiling`, `replicas-below-min`. Normal, not alerted: `job skipped: lock unavailable`, `server draining`. The template does not scrape `/metrics`; a deployment that does alerts on `pad_redis_up == 0`, `rate(pad_inval_gap_total[15m]) > 0`, sustained `pad_inval_dropped_total`, `pad_chat_commands_total{side="origin",outcome="unreachable"}`, `pad_redis_caps_total{outcome="open"}`, `pad_ai_ledger_errors_total`, `pad_chat_conversation_conflicts_total{source="retry_exhausted"}`, `pad_audit_spilled_total{reason="db_unavailable"}` and `count(up{job="baki"})` below `minReplicas`. `pad_usage_dropped_total{reason}`: the shared runner's refusal is `runner`, not `shutdown` — key alerts and dashboards on `reason="runner"`.
