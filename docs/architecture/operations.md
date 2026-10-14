# Operating a deployment

Database connections, pooling, the stop budget, running more than one replica and the alerts of a cloud deployment (`infra/main.bicep` on Azure
Container Apps, or the compose stack). The decisions behind them are in
[ADR 0023](../adr/0023-singleton-jobs-and-leases.md) (jobs and their locks) and
[ADR 0024](../adr/0024-graceful-shutdown-and-server-restarting.md) (the stop).

## Database connections

Each replica opens one connection pool (`storagedb.New`), capped by `PAD_DB_MAX_OPEN_CONNS`, 25 by default. The cap
is per replica, so the server sees up to `pool × replicas` connections, and more during a rolling deploy: in Single
revision mode the new revision starts its `minReplicas` (at least one) beside the old revision's replicas until
traffic moves.

### Sizing the pool from the server's cap

Give `infra/main.bicep` the server's `max_connections` as `dbServerMaxConnections` (`infra/postgres.bicep` outputs it
as `maxConnections`; Burstable B1ms has 50, General Purpose D2ds_v5 859). With `dbMaxOpenConns` empty it then sets
each replica's pool to

```
min(25, max(1, (dbServerMaxConnections - dbReservedConnections) / (maxReplicas + max(minReplicas, 1))))
```

- `dbReservedConnections` (default 20) is left to others: the 15 connections Azure Flexible Server reserves for
  replication and monitoring, and 5 for an operator's `psql` or `bakicli`.
- `max(minReplicas, 1)` is the rolling deploy's surge, so every pool full at once still fits mid-deploy. With the
  template's `minReplicas = 1` it is `maxReplicas + 1`.
- At least 1, because the app reads 0 as "not set" and opens its default of 25.
- At most 25, the app's default: a large server is no reason to open more. An explicit `dbMaxOpenConns` can, and it
  always wins over the derivation.
- `dbServerMaxConnections = 0` (the default) leaves the pool at the app's default, as before.

| Server | `max_connections` | `maxReplicas` | Pool per replica |
|---|---|---|---|
| B1ms | 50 | 1 | (50 − 20) / 2 = 15 |
| B1ms | 50 | 3 | (50 − 20) / 4 = 7 |
| B2s | 429 | 1 | 204, capped at 25 |
| D2ds_v5 | 859 | 1 | 419, capped at 25 |

(`minReplicas` 1 throughout; with `minReplicas = maxReplicas = 3` on a 100-connection server it is 80 / 6 = 13.)

`internal/config/deploy_pool_test.go` evaluates the Bicep expression for these cases. The CI deploy reads the knobs
from repository variables: `PAD_DB_MAX_OPEN_CONNS`, `PAD_DB_SERVER_MAX_CONNS` and `DB_RESERVED_CONNECTIONS`. The
template passes the app `PAD_MAX_REPLICAS` (always) and `PAD_DB_SERVER_MAX_CONNS` (when set).

### Pinned lock connections

Each running Singleton job holds one pooled connection for the whole run: its session advisory lock lives on that
session, and a heartbeat checks it every 15 s (ADR 0023). The work itself takes further connections from the same
pool, the vector index build pins two, and the schema migration pins one while it runs. They come out of the pool,
not on top of it. In the first minutes after a boot several jobs run at once (the backfills, the re-seal, the vector
index build, the first retention, scanner and blacklist-cleanup runs; `internal/lifecycle/testdata/jobs.golden` lists
the declared ones), so a pool in single digits leaves requests waiting for connections while they run. Keep it well
above that; the default 25 is.

To see which replica holds which job lock (connections are named `baki-backend/<instance>`, the replica's
`PAD_INSTANCE_ID`, by default the replica name Azure Container Apps gives it, unless the DSN sets
`application_name`):

```sql
SELECT a.application_name, a.pid, to_hex((l.classid::bigint << 32) | l.objid::bigint) AS key
FROM pg_locks l JOIN pg_stat_activity a USING (pid)
WHERE l.locktype = 'advisory' AND l.granted;
```

The keys are in `jobs.golden`, in hex (`62616b697363616e` is "bakiscan", the scanner).

One job runs only on demand and spends money: `knowledge_reembed`, an org admin's knowledge-base re-index with
`{"background": true}` (ADR 0027). It re-embeds the org's corpus page by page under the organisation's shared
embedding credential, one org at a time across the deployment (a second start answers 409 `KNOWLEDGE_BUSY`), and
is never started by the server itself; a model switch only shows up as `pad_rag_stranded_search_total` and the
document list's `embeddingStranded` counts until an admin re-indexes. Its run state is the `job_runs` row and
`pad_job_*` like every other job; it has no period, so it is never overdue.

#### The knowledge-base org lock

An upload and a synchronous re-index of one org are mutually exclusive across replicas, not only inside a process.
`rag.KnowledgeService.lockOrg` takes two levels, both without waiting (the second admin gets 409 `KNOWLEDGE_BUSY`):
the in-process mutex (a cheap answer that uses no connection), then, when the deployment has a `lock.Leaser`
(`ProvideKnowledgeService` receives it as it does for `knowledge_reembed`; nil on the desktop and without a database,
which keep the in-process lock alone), a lease on `rag.KnowledgeLockKey(orgID)` held for the upload's check -> embed ->
write window and for the whole re-index page loop. Without it replica A could pass the model check, replica B's
re-index convert the corpus to another identity, and A insert old-identity chunks: a mixed corpus (ADR 0027), and two
replicas could both pay to embed the same upload. A lost lease (its session died; another replica may hold the org)
cancels the operation and answers busy; a lease that cannot be asked for at all (the database is down) answers 503
`KNOWLEDGE_LOCK_UNAVAILABLE` and starts nothing.

- **Key space.** `0x6b6e6f77` ("know") in the high 32 bits, an FNV-1a-32 of `knowledge_org:<org id>` in the low
  32. The high word must never be 0 or -1: the chunk-write transactions take `pg_advisory_xact_lock(hashtext(org_id))`,
  an int4 promoted to bigint, and a lease on such a key would be held on one session while the operation's own write
  waits for it on another (a self-deadlock until the statement timeout). `TestKnowledgeLockKey_*` and
  `TestKnowledgeLease_*` prove the spaces disjoint and reproduce the deadlock with the naive key. The jobs' keys
  (`jobs.golden`) have other high words. The short transaction lock stays: it protects one write if a lease is lost
  mid-way.
- **Pool cost.** A lease pins one pooled connection for the whole operation (seconds to minutes, billed embedding
  calls in the middle), like a job. `PAD_KNOWLEDGE_LEASE_CAP` (default 4) bounds how many a replica holds at once; the
  operation beyond it answers 409 `KNOWLEDGE_BUSY` instead of waiting for the pool. Keep it well below
  `PAD_DB_MAX_OPEN_CONNS`; with the default pool of 25 the worst case is 4 pinned connections beside the jobs'.
- **Seeing it.** The lease shows in the query above with a key whose high word is `6b6e6f77`. The background
  re-embed takes the org lease per request (each `ReindexOrg` page batch), so an upload may slip in between two of
  its requests exactly as it can between two in one process; the model guard of the next request still applies.

### The boot checks

`storagedb.New` warns and never fails the boot:

| Level | `msg` | When |
|---|---|---|
| WARN | `postgres: connection pools can exceed max_connections` | `PAD_DB_MAX_OPEN_CONNS × max(PAD_MAX_REPLICAS, 1)` is more than `max_connections − superuser_reserved_connections − reserved_connections`. The line carries the numbers and a `suggested_max_open_conns` that leaves room for the surge. |
| WARN | `PgBouncer transaction mode is unsupported: session advisory locks` | A host of `PAD_DATABASE_URL` uses port 6432 (read from the parsed DSN, before the first connection). |

The pool check counts the replicas as configured, not the surge: a deployment that fits only in steady state does
not warn, yet can run out of connections during a deploy. The Bicep derivation leaves that room.

The server's limits come from the server itself, in one query. When that fails (a pooler in front, a role that may
not read the settings), `PAD_DB_SERVER_MAX_CONNS` stands in when it is set, with no reserved connections known, and
the line's `limits_from` says `PAD_DB_SERVER_MAX_CONNS` instead of `server`. Unset, the check is skipped.

## Pooling: session mode only

Point the app at the server itself (port 5432), or at a pooler in **session** mode. Transaction pooling, which is
what Azure Flexible Server's built-in PgBouncer does on port 6432, breaks:

- the session advisory locks: the lock and its unlock land on different server sessions. Every lease is lost at its
  first heartbeat (`advisory lease lost; discarding its session`, `job lease lost`), and an unlock answers "not held"
  (`advisory unlock reported not held; discarding connection`);
- the migrations' session-level `SET` (it would reach other clients);
- pgx's cached prepared statements, unless PgBouncer 1.21+ has `max_prepared_statements`;
- the startup parameters `statement_timeout` and `lock_timeout`, which PgBouncer refuses unless it is configured to
  track or ignore them.

These PgBouncer details are not tested here.

Session pooling works but saves nothing here: each pooled connection keeps its server session. A session-mode
PgBouncer on port 6432 also trips the boot warning; move it to another port.

## Stopping

`fx.StopTimeout` (28 s) bounds the app's stop. What runs in it, in order, with each step's budget, is the table in
[ADR 0024](../adr/0024-graceful-shutdown-and-server-restarting.md). Its first pause is `PAD_SHUTDOWN_DRAIN_DELAY`
(3 s in the cloud, 0 for local and desktop, at most 10 s). The platform must not kill the process before:

- ACA: `terminationGracePeriodSeconds` = `stopGraceSeconds` in `infra/main.bicep`, 35 s;
- compose: `stop_grace_period: 35s` on the app service of both files (Docker's default is 10 s).

`internal/config/deploy_pool_test.go` keeps both above the `StopTimeout` it reads from the wiring. Change the table
and the timeout together, and the grace periods with them.

While it stops, a replica answers `/readyz` with 503 (`{"status":"draining"}`) from the start of its drain, then stops
listening. The readiness alert below allows for that.

## The audit spill with several replicas

Audit rows are written through a bounded queue. When it is full a row goes to an on-disk spill file, and a reaper
replays the file into the queue once there is room (`pad_audit_spilled_total`, `pad_audit_spill_replayed_total`,
`pad_audit_spill_depth`). A row that the database refuses twice does not spill: it goes to the redacted log line
(`pad_audit_dropped_total{reason="write_failed"}`).

- **One file per replica.** `PAD_AUDIT_SPILL_DIR/audit-spill-<instance>.jsonl`, `<instance>` being `PAD_INSTANCE_ID`
  (the replica name on Azure Container Apps, else the host name) limited to `[A-Za-z0-9._-]`. The file the release
  before wrote, `audit-spill.jsonl`, is adopted like any other leftover. Two replicas on one volume never rewrite
  each other's file.
- **Depth from the start.** A file left by the previous boot counts in `pad_audit_spill_depth` from the first scrape,
  not from the first drain.
- **Adopting the files of replicas that are gone.** At start, and once a minute, a replica looks in the directory for
  spill files that are not its own and have not been modified for 2 minutes, and takes one over by creating a
  `<file>.claiming` marker (`O_EXCL`) and renaming the file to `<file>.adopt-<its instance>-<unix nanos>`. Only the
  replica that wins the marker replays it; the claim is deleted when it is empty. A replica that dies while
  replaying leaves its claim, which another replica takes over after the same 2 minutes, and which the same instance
  continues at once. There is no lock file and no heartbeat: a live replica rewrites its file on every spill and
  drain, and drains it every 500 ms, so a non-empty file that nothing has touched for 2 minutes is abandoned. The
  one live replica that can lose its file this way has had a reaper stalled for 2 minutes (its queue stayed over
  half full), and then the claimant replays its rows, which is harmless because of the next point. An empty file is
  no work and is left alone (every idle replica has one) until it is a day old.
- **Replays are idempotent.** Both audit INSERTs end in `ON CONFLICT (id) DO NOTHING`, so a row written twice (the
  file replayed after a crash between the insert and the file rewrite, or a claim taken over from a replica that was
  only slow) is stored once.

The default needs no change for several replicas: `/tmp/baki-audit-spill` is an `EmptyDir` of each replica, shared
with no one, so adoption has nothing to find and a replaced replica's file is lost with it, as before. A volume
shared by all replicas (Azure Files) is the optional upgrade that lets a replacement replica replay what the
previous one left; it needs a file system with exclusive create and atomic rename, and is the only setup where the
adoption rules above matter. Leave the default unless audit rows must survive replacing a replica.

## Migration status and the schema version

Two different things are called migration. The **schema** migration runs at boot and serialises across replicas on a
blocking `pg_advisory_lock`; a replica booting behind a longer step waits for it (the startup probe allows for that).
The **legacy data migration** (`PAD_DATA_DIR` into the database, `POST /api/admin/migration/start`) is an admin
action that runs on one replica at a time.

- **`GET /api/admin/migration/status` answers for the deployment.** The run is recorded in `job_runs` under
  `legacy_migration` like every job (start, end, holder, error; `pad_job_*` metrics), and its result (the counts,
  `NotMigrated`, the first failures, capped at 64 KiB) in `job_runs.last_detail` (migration 0046). Any replica reads
  the row, so the answer no longer depends on which replica the request reached or on a restart since. The shape is
  unchanged: `configured`, `running`, `startedAt`, `result`, `error`. A run is "running" when it started after it
  last ended and no more than an hour ago (the run's own timeout), so a replica that died mid-run reads as not
  running after the hour; a start this replica recorded while it runs nothing counts as its own finished run. A new
  run clears the previous result when it starts. Without a database (desktop, local) the status is the process's own,
  as before. A second start while a run goes anywhere is still a 409.
- **`GET /api/admin/system/health` says which build answered.** It adds `binaryVersion` (the `X-Baki-Version` build
  stamp), `schemaVersion` (the highest applied schema migration, left out when the database cannot be asked) and
  `instanceId` to the per-subsystem breakdown. During a rolling deploy the replicas differ: call the page until the
  answers agree, or read `schemaVersion` against the release's newest migration. `/readyz` does not carry them.
- **Rollback.** Migration 0046 only adds a nullable column and is `expand`: the release before neither reads nor
  writes it and runs on the new schema; `bakicli migrate down` drops it. A replica of the release before answers the
  status from its own memory, as it did.

## Redis

Redis is optional (`PAD_REDIS_URL`); a single replica needs none, and `PAD_MAX_REPLICAS` above 1 refuses to boot without it.
[Running more than one replica](#running-more-than-one-replica) below is the checklist, the failure modes of every mechanism and how to raise the count.
[ADR 0029](../adr/0029-redis-envelope-key-registry-and-health.md) holds the payload envelope, the registry of every key
(`internal/di/testdata/redis_keys.golden`) and the failure-mode table. What an operator needs:

- **Down is degraded, not unready.** An unreachable Redis does not stop the boot (a WARN, `redis: backplane unreachable at boot`)
  and does not fail `/readyz`: it stays 200 and the body adds `"degraded":["redis"]`. A URL that does not parse
  refuses to boot. The admin health page (`/api/admin/system/health`) pings live and reports `overall: degraded`.
- **The signal is a metric.** `pad_redis_up` is 1 while Redis answers (a ping every 5 s and every operation's outcome)
  and 0 while it is degraded: the last ping failed, or three operations in a row failed. It is absent without
  `PAD_REDIS_URL`. Alert on `pad_redis_up == 0` for a few minutes; `pad_redis_errors_total{mechanism}` says which part
  is failing (a declared key family, `ping`, or `other`). `pad_redis_msgs_total{kind,result}` counts decoded payloads:
  `unknown` rising during a rolling deploy is the other release's newer payloads and is expected, `bad` is a bug.
- **What an outage costs, per mechanism:** rate limits become per replica (N times the rate), collaborators on other
  replicas stop seeing each other's edits and presence, a chat stream cannot be resumed on another replica, and a
  revocation reaches peers at their 30 s reload; nothing is refused, except that AI calls under a configured daily
  budget are (next bullet) and that a stop or an approval decision for a chat stream another replica runs answers 503
  `REPLICA_UNREACHABLE` (a retry; the stream is not stopped by it, see
  [ADR 0033](../adr/0033-stream-commands-across-replicas.md)). The deploy smoke test (`/readyz`) cannot see
  it, so check `pad_redis_up` after a deploy that touches Redis. Cache invalidations and access rechecks (`baki:inval`,
  [ADR 0032](../adr/0032-cache-invalidation-fanout-and-state-registry.md)) are not sent: the other replicas serve the old
  settings, rule profiles, corpus counts and dashboard views for their TTL (30 s; model listings 10 min), and keep a
  revoked socket until its 2-minute pass; each replica drops all of it when its subscription is back.
- **The AI budget is the one mechanism that refuses.** With a daily AI budget set (`AI.DailyBudget > 0`) and more than one
  replica (`PAD_MAX_REPLICAS > 1` with Redis; a single replica keeps its in-process ledger even when `PAD_REDIS_URL` is
  set, so a Redis maintenance window cannot stop its chat) the in-flight
  ledger lives in Redis (`baki:ai:inflight:*`) and fails closed: while Redis is down new chat turns, knowledge-base
  embedding runs and the provider connection tests answer `AI_BUDGET_UNAVAILABLE` (503 `BUDGET_UNAVAILABLE` on the
  knowledge routes), and a chat response already streaming stops at its next budget re-check with "response stopped:
  couldn't verify today's AI budget". It recovers on its own when Redis does. With no daily budget configured nothing
  touches Redis for it. Watch `pad_ai_ledger_errors_total{op}`; see
  [ai-providers.md](ai-providers.md#the-in-flight-budget-ledger-across-replicas-addendum-to-adr-0028-phase-5b).
- **The invalidation channel's signals.** `pad_inval_gap_total` rising means the channel loses messages (each gap forces a
  resync); `pad_inval_dropped_total{reason}` counts messages this replica did not send (`redis_down`, `publish_error`,
  `queue_full`); `pad_inval_resync_total{reason}` says why caches were dropped wholesale (`subscribe` is normal at boot and
  after an outage); `pad_inval_ignored_total{reason="unknown_key"}` during a rolling deploy is the other release's newer
  caches. The per-user caps (10 event streams and 10 sockets per user, 3 concurrent uploads and 3 chat streams) are
  per replica, so N replicas allow N times as many.
- **Every key has a TTL**, so the managed Redis runs `volatile-lru` (`infra/main.bicep`; a channel is not a key). The registry golden
  is the list and `TestDeployRedis_VolatileLRUNeedsEveryKeyToExpire` fails a key declared without one.
- **Event streams across replicas** ([ADR 0030](../adr/0030-cross-replica-commands-and-sse-replay.md)). With Redis, an SSE
  event emitted on one replica reaches a client connected to another (`baki:sse:ev`), the durable ones carry an `id:`
  taken from a per-user Redis Stream (`baki:sse:log:<user>`, about 200 frames, 5 minutes) and a reconnect with
  `Last-Event-ID` is replayed what followed it, and each replica records which users it holds a stream for
  (`baki:sse:subs:<user>`), which is what stops the chat watchdog cancelling a stream whose owner's tab sits on
  another replica. An outage costs the cross-replica part only: events reach local clients, frames carry no id,
  and the watchdog assumes someone is listening. `pad_sse_fanout_total{direction,outcome}` (`failed`, `dropped`,
  `skipped`), `pad_sse_log_total{outcome}`, `pad_sse_replay_total{outcome}` and `pad_sse_presence_reads_total{outcome}`
  say which part. A replica of the release before does not take part, so a rolling deploy from it runs the two
  releases as separate islands for its duration: keep `maxReplicas` at 1 until the whole fleet runs this release.
- **Stream commands.** Each replica listens on `baki:chat:cmd:<its instance id>` and `baki:chat:reply:<its instance id>`
  (`PAD_INSTANCE_ID`; the default is the platform's replica name or the host name; the name Redis sees carries a random
  suffix chosen at boot, so two replicas that share a name still stay apart).
  `pad_chat_commands_total{op,side,outcome}` counts the commands both ways; a sustained `outcome="unreachable"` at
  `side="origin"` is a replica that died or a Redis outage (compare `pad_redis_up`), and users see the retry toast.

## Running more than one replica

The repository ships **one replica and no Redis** (`minReplicas = maxReplicas = 1`, `provisionRedis = false`, in
`infra/main.bicep`, `infra/main.parameters.json` and `deploy.yml`). The first deploy of the release that has the
cross-replica mechanisms stays that way; this section is how a deployment is then taken to more than one, and what it
has to know to run that way. Raising the repository defaults is a deliberate commit, made once the `two-replica` CI job
(below) has been green for a while; `internal/config/deploy_replicas_test.go` pins the defaults and says so.

### What changed, and what did not

Behind a load balancer with no stickiness, every request can land on any replica. What a request needs from another
replica now travels through two shared places: **Postgres** (the data, plus locks and versions) and **Redis** (events,
commands, leases that expire, caches told to drop). Nothing needs a sticky session: leave `stickySessions` unset.

### Checklist before `maxReplicas` goes above 1

1. **Redis.** `PAD_PROVISION_REDIS=true` (`provisionRedis`). With `maxReplicas > 1` the SKU defaults to Standard C1 (a
   replica node and an SLA; Basic has neither, and a restart of Basic is a full outage of every cross-replica
   mechanism); with one replica it stays Basic, so turning Redis on first, to rehearse, is not repriced. The template
   runs it with `maxmemory-policy volatile-lru`, which is safe because every key the app writes has a TTL: the
   registry (`internal/di/testdata/redis_keys.golden`) declares one for each, `redisx.Declare` refuses a key without,
   and `TestDeployRedis_VolatileLRUNeedsEveryKeyToExpire` reads the golden. A Redis of your own: `rediss://`, the same
   policy, one logical database, no persistence needed (everything in it is a lease, a few minutes of log or a counter).
2. **The guard.** `PAD_MAX_REPLICAS` above 1 without `PAD_REDIS_URL` is refused by the app at boot, by the deploy
   workflow before it deploys (`Refuse several replicas without Redis`), and by `deploy_replicas_test.go` for the
   files in the repository. The template passes `PAD_MAX_REPLICAS` itself.
2a. **Harden Redis.** Event bodies, stream commands and the replay log cross Redis (ADR 0030, ADR 0033): put it on a
   private network, require AUTH and use `rediss://`, turn off RDB and AOF (everything in it is a lease, a few minutes of
   log or a counter), keep `maxmemory` with headroom and the policy `volatile-lru`. Alert on `pad_redis_up == 0`, on any
   increase of `pad_redis_evicted_keys_total` (an evicted claim or ledger key is the failure ADR 0029 lists per key) and
   on `pad_redis_bus_rejected_total{reason!="stale"}` above zero (somebody who is not a replica publishing on the command
   channels, or replicas with different `PAD_ENCRYPTION_KEY`/`PAD_AUTH_SECRET`). Commands are signed with a key derived from
   that secret, so every replica needs the same one and clocks within 30 s; a rolling change of the secret answers 503 for
   the overlap (retry). The app warns at boot when `PAD_MAX_REPLICAS > 1` and the URL is `redis://` or has no password.
3. **A name per replica.** Stream commands are routed by the replica's instance id plus a boot suffix
   (`baki:chat:cmd:<id>#<suffix>`). On Azure Container Apps each replica takes `CONTAINER_APP_REPLICA_NAME`, which the platform
   sets; the template sets no `PAD_INSTANCE_ID` on purpose (one value would be every replica's name). Elsewhere the host
   name is the fallback, and compose names its replicas (`docker-compose.replicas.yml`). Two replicas with one name
   still work (the suffix keeps their Redis identities apart), but logs, `application_name` and the audit spill file
   names then collide, so keep them distinct; the smoke script and the admin health page (`instanceId`) show the names.
4. **A scale rule.** `maxReplicas` above `minReplicas` does nothing by itself; the template adds an `http` rule,
   `scaleConcurrentRequests` (default 100 concurrent requests per replica). **Open event streams and WebSockets count
   as concurrent requests for as long as they are open**: a signed-in tab holds an event stream and, in an open flow, a
   socket, so 100 is roughly 50 users per replica, and the rule scales out on idle tabs. Size it above the open
   streams a replica should carry (and watch the `replicas-at-ceiling` alert).
5. **Database pools.** Set `dbServerMaxConnections`; each replica's pool is then derived from the replica count and the
   rolling deploy's surge (see above). More replicas mean more connections at once, and the knowledge-base leases
   pin some of them (`PAD_KNOWLEDGE_LEASE_CAP` per replica).
6. **Alerts.** Set `alertEmails`. A deployment with Redis gets `redis-degraded` and `invalidation-channel`; one with
   `maxReplicas > 1` gets `replicas-at-ceiling` and `replicas-below-min` (the Alerts table below).
7. **Per-replica limits are multiplied.** These are per replica, so N replicas allow N times as many: 10 event streams
   and 10 sockets per user, 3 concurrent uploads and 3 chat streams per user. The daily AI budget is not multiplied
   (its in-flight ledger is shared); rate limits are not multiplied while Redis is up.
8. **A fleet on one release.** Do not run a release without these mechanisms beside one that has them (see "A rolling
   deploy" below).

### What is shared, and how

Every piece of per-process state is declared in `internal/archtest/state_test.go` (`stateRegistry`, ADR 0032) with its
locality and, for a shared one, the test that proves it. The shared ones:

| What | Where it lives | Key, channel or table | Proven by |
|---|---|---|---|
| Sessions, token revocation | Postgres; a revocation also fans out | `baki:auth:revoked`, a 30 s reload | `TestCachedBlacklist_*` |
| Rate limits (per address and per user) | Redis | `ratelimit:{group}:{addr}` | `ratelimit_groups.golden` |
| Collaboration rooms, presence, ticket single-use | Redis | `ws:room:{flow}`, `ws:presence:{flow}`, `baki:ws:ticket:{jti}` | `TestWSTicket_*`, `TestPresenceSync_AcrossReplicas` |
| Server-sent events between replicas, their replay and who holds a stream | Redis | `baki:sse:ev`, `baki:sse:log:{user}`, `baki:sse:subs:{user}` | `TestSSEFanout_*`, `TestSSEReplay_*` |
| Chat stream state for a reload | Redis | `chat:resume:{stream}`, `baki:chat:active:{conversation}` | `TestCommands_*` |
| Stop and approval decision for a stream another replica runs | Redis | `baki:chat:cmd:{instance}`, `baki:chat:reply:{instance}`, `baki:chat:approval:{proposal}`, `baki:chat:decision:{proposal}` | `TestCommands_*`, `TestTwoReplicas_EndToEnd` |
| The AI budget's in-flight spend | Redis | `baki:ai:inflight:user:{scope}`, `baki:ai:inflight:org:{org}` | `TestRedisLedger_*` |
| Caches told to drop (settings, rule profiles, model listings, corpus counts, dashboards, socket access) | Redis | `baki:inval` | `TestTwoReplicas_EndToEnd`, `internal/invalidation` |
| An identical flow re-uploaded through another replica | Redis | `baki:flow:upload-id:{user}:{digest}` | `TestUploadIDs_*` |
| Conversations (one writer wins, the others reload) | Postgres | `conversations.version`, compare-and-swap | conversation-version contract scenarios |
| The undo ring of a stored flow | Postgres | `flow_snapshots` | `TestSnapshotStore_TwoReplicasShareTheRing` |
| Knowledge-base upload and re-index exclusion | Postgres | an advisory lease per org | `TestKnowledgeLock_*` |
| Legacy data migration status | Postgres | `job_runs.last_detail` | `TestMigrationStatus_*` |
| Background jobs (scanner, retention, re-seal, backfills) | Postgres | session advisory locks (ADR 0023) | `lifecycle` tests |
| Audit rows that overflowed the queue | The replica's own spill file | `audit-spill-<instance>.jsonl`, adopted when abandoned | `internal/audit` tests |

Still per replica, on purpose or as known gaps (the registry says which): the SSE connections and WebSockets a replica
holds, the streams it runs, its provider circuit breakers, learned context windows and token exchanges, and the
scanner's alert-dedup memory (a hand-over of the scan job to another replica re-analyzes every flow once and may repeat
an alert). A replica that dies takes its own streams with it.

### What fails, and how

Redis is not a readiness dependency: a replica with Redis down stays ready, reports `degraded` and runs each mechanism in
its declared on-down mode (the `on-down` column of `redis_keys.golden`; [ADR 0029](../adr/0029-redis-envelope-key-registry-and-health.md)).
Postgres is: a replica that cannot reach it leaves rotation after three failed probes.

| Mechanism | Redis down | A replica dies or restarts |
|---|---|---|
| Rate limits | Each replica limits on its own: N times the rate, never a refusal. | Its buckets are in Redis; nothing is lost. |
| Token revocation | Not fanned out; the other replicas learn at their 30 s reload from Postgres, which stays the source of truth. | Reloaded from Postgres. |
| Collaboration rooms and presence | People on other replicas stop seeing each other's edits and cursors; each replica's own sockets work. Records resume when Redis is back. | The replica's sockets drop and reconnect elsewhere; its presence records expire (a few minutes at most). |
| WebSocket ticket single-use | Falls back to each replica's own memory: a ticket is single-use per replica for its 2-minute life. The Postgres revocation list still applies. | The in-memory part is lost with the replica; the shared part expires. |
| Server-sent events between replicas | Events reach the clients of the replica that emitted them only; frames carry no id; the chat watchdog assumes someone is listening and never cancels a stream for it. | Clients reconnect (any replica) and send `Last-Event-ID`: up to 200 durable frames from the last 5 minutes are replayed. |
| Stop, approval decision, reload for a stream another replica runs | 503 `REPLICA_UNREACHABLE` with `Retry-After`, never an ok that did nothing; the client retries a stop twice, then shows a toast. A stop or decision for a stream on the replica that got the request needs no Redis. | The stream is gone with its replica; a stop or decision for it is 503 until its snapshot expires (at most 12 minutes), and the answer in progress is not stored: the user asks again. |
| AI budget (when `AI.DailyBudget > 0`) | **Fails closed:** new chat turns, knowledge-base embedding runs and provider tests answer `AI_BUDGET_UNAVAILABLE`, and a response already streaming stops at its next budget re-check. With no daily budget nothing touches Redis for it. | Its ledger entries stop counting after their 30 s lease. |
| Cache invalidation and socket rechecks | Not sent: the other replicas serve settings, rule profiles, dashboards and corpus counts until their TTL (30 s; model listings 10 min) and keep a revoked socket until its 2-minute pass. Every replica drops all replicated caches when its subscription is back. | Nothing to recover: caches rebuild from Postgres. |
| Re-upload of an identical flow | Each replica remembers its own uploads' ids: the same file uploaded through two replicas can make two flows. | The memo of the replica is lost. |
| Conversation saves | Not affected: the compare-and-swap is Postgres's. | Not affected. |
| Undo ring | Not affected (Postgres). | Not affected. |
| Knowledge-base lease | Not affected (Postgres advisory lease). | The lease ends with the session; its holder is noticed within about 20 s. |
| Background jobs | Not affected (Postgres advisory locks). | Another replica takes the job at its next attempt. |
| Audit spill | Not affected. | A replica's spill file is adopted by another after 2 minutes if the directory is shared; on the default per-replica `EmptyDir` it is lost with the replica. |
| Migration status | Not affected. | Read from `job_runs` by any replica. |

When Redis comes back, the subscriptions re-establish on their own backoff (a second or two, up to ten) and the first
thing each does is drop its replicated caches; commands and events cross again without a restart. A Redis that came
back **empty** (no persistence, the usual restart) has lost the resume snapshots, the pending approvals and the replay
logs: a running stream's snapshot is written again by its owner within a second, but an approval that was waiting
cannot be decided from another replica (the card stays pending until its 60-second timeout) and the replay a
reconnect would have had is gone. A stream that ended while Redis was down may leave its budget reservation in the
ledger until the 30 s lease runs out: its user's next turn can be refused with "daily AI budget exceeded" for that long.

### A rolling deploy with mixed versions

Container Apps in Single revision mode starts the new revision's replicas (`minReplicas`, at least one) beside the old
ones and moves traffic across, so every deploy at more than one replica has a window with both releases on one Redis
and one database.

- **Releases that both have these mechanisms.** The wire formats are additive: a payload with a version or kind the
  receiver does not know is ignored and counted (`pad_redis_msgs_total{result="unknown"}`, `pad_inval_ignored_total{reason="unknown_key"}`),
  never deleted and never a failure; a rising `unknown` rate during the deploy is the other release's newer payloads and is
  expected. Migrations are `expand` only, so either release runs on the schema.
- **A release before these mechanisms beside one that has them** is an island: it neither publishes nor subscribes, so
  events, invalidations and the budget's in-flight view do not cross between the two, and a stop for a stream that
  release runs is 503 (its snapshot names no replica). That is why the first deploy of this release stays at
  `maxReplicas = 1`: with one replica the window is the old revision's one replica beside the new one's, short and
  without a user on both.
- **Drain.** A stopping replica answers `/readyz` with 503 for its drain, ends its event streams (clients reconnect
  to another replica) and runs its chat streams to their end within the stop budget (ADR 0024); the
  `terminationGracePeriodSeconds` and the readiness alert allow for it.
- **Checking a deploy.** `GET /api/admin/system/health` answers `binaryVersion`, `schemaVersion` and `instanceId` of the
  replica that served it: call it until the answers agree. The deploy's smoke test prints a warning when `/readyz` reports
  `degraded`.

### Raising `maxReplicas` safely

1. **Deploy the release at one replica first** (the defaults) and let it run: the schema is migrated, and the fleet is
   one release. Nothing in this section is needed yet.
2. **Rehearse Redis at one replica.** Set the repository variable `PAD_PROVISION_REDIS=true` (and, if you want the
   production cache already, `redisSku=Standard` in the parameters), deploy, and check that `/readyz` is ok without
   `degraded`, `pad_redis_up` is 1 and the `redis-degraded` alert exists. With Redis and one replica every mechanism
   runs through it already: events carry ids, stop and decisions go through the same path, the budget ledger is shared.
3. **Check the two-replica job.** `two-replica` in `ci.yml` (`docker-compose.replicas.yml` and
   `scripts/ci/two-replica-smoke.sh`) must be green on the release you are deploying; it gates the deploy.
4. **Raise the count.** Set `PAD_MAX_REPLICAS` (and `PAD_MIN_REPLICAS` for a floor of two: a replica lost then does
   not take the app down) and, if the default does not fit, `PAD_SCALE_CONCURRENT_REQUESTS`; set `PAD_DB_SERVER_MAX_CONNS`
   if it is not already. The deploy refuses the combination without Redis. Redeploy.
5. **Watch.** The admin health page on a few requests (`instanceId` differs), `pad_redis_up`, the alerts above, the
   pools (`postgres: connection pools can exceed max_connections` at boot says the numbers). Run the smoke script
   against the deployment's replicas if they can be addressed one by one.
6. **Going back** is the same variable lowered and a redeploy; nothing in the schema depends on the count, and Redis
   can stay provisioned.

### Trying it locally

```
docker compose -f docker-compose.yml -f docker-compose.replicas.yml up -d --build --wait
scripts/ci/two-replica-smoke.sh
docker compose -f docker-compose.yml -f docker-compose.replicas.yml down -v
```

The overlay runs two replicas (`app-a` on :8081 and `app-b` on :8082, `PAD_INSTANCE_ID` set, `PAD_MAX_REPLICAS=2`), a
Redis with `volatile-lru` and a round-robin balancer on :8080 that flushes event streams. It is a development stack
like the base file (plaintext, a TLS-less database); its project name differs from the base file's, so it never touches
the development volume. The smoke script drives sign-in across replicas, a change on one showing on the other, an
event on one reaching a stream on the other and replaying after a reconnect, six rounds of two racing conversation
saves, an undo through the other replica, the migration status, and Redis stopped and started. It cannot cover the
stop and approval of a chat stream (that needs an AI provider); `internal/di/two_replicas_test.go` does, in process on
miniredis and one database (the in-memory store always, Postgres when `DATABASE_URL` is set).

The production compose overlay (`docker-compose.prod.yml`) has no Redis; its `Caddyfile` resolves `app` again every
two seconds and round-robins, so `--scale app=2` reaches both replicas once the stack also passes `PAD_REDIS_URL` and
`PAD_MAX_REPLICAS` to the app, which that overlay does not yet.

### Rules for a deployment that scrapes `/metrics`

The template's alerts read log lines and platform metrics because nothing scrapes the Prometheus metrics (see Alerts).
A deployment that does can alert on the exact signals:

| Signal | Rule | Means |
|---|---|---|
| `pad_redis_up` | `pad_redis_up == 0` for 5 minutes | Redis is degraded (the `redis-degraded` alert) |
| `pad_inval_gap_total` | `rate(pad_inval_gap_total[15m]) > 0` for 15 minutes | the invalidation channel loses messages (the `invalidation-channel` alert approximates it from the warnings) |
| `pad_inval_dropped_total{reason}` | any sustained rate | this replica could not send its invalidations |
| `pad_chat_commands_total{side="origin",outcome="unreachable"}` | sustained | stops and decisions cannot reach the owner (a dead replica, or Redis) |
| `pad_ai_ledger_errors_total{op}` | sustained | the budget ledger fails; with a daily budget set, chat is refused |
| `pad_chat_blind_saves_total` | `rate(...[1h]) > 0` | clients from before conversation versions still save without `baseVersion`. This is not a conflict counter: there is none, a lost race is the 409 `CONVERSATION_CONFLICT` in the access log |
| replicas | `count(up{job="baki"})` below `minReplicas` | the `replicas-below-min` alert, from the platform's `Replicas` metric |

## Metrics renamed

`pad_usage_dropped_total{reason}` counts the usage rows the process could not record. Phase 4b renamed the value
for the shared runner's refusal from `shutdown` to `runner` (the drain's own refusals were never the only cause;
`pad_background_dropped_total{task="ai_usage_record"}` says whether it was the cap or the drain). An alert or
dashboard keyed on `reason="shutdown"` goes silent without a series change: key it on `reason="runner"`.

## Alerts

`infra/main.bicep` provisions them when `alertEmails` is set, all on one action group. The log alerts query the Log
Analytics workspace; the app's own lines match on the slog JSON field, `Log_s contains '"msg":"<message>"'`.

| Alert (`<app>-…`) | Fires on | Window, severity | Look at |
|---|---|---|---|
| `restarts` | the `restarts` metric above 3 | 5 min, 1 | crash loop, OOM kill |
| `error-burst` | more than 20 `"level":"ERROR"` lines | 15 min, 2 | the ERROR lines and their `request_id` |
| `readiness-failing` | one replica logging at least `max(readinessFailureThreshold, stopGraceSeconds / probePeriodSeconds + 2)` readiness failures (6 by default) | 10 min, 1 | `/readyz`: Postgres, blob (Redis is reported, not gated; see `pad_redis_up`) |
| `job-overdue` | `job overdue` | 2 h, 2 | the line's `job`, `pad_job_overdue`, the job's `job run failed` lines |
| `job-lease-lost` | `job lease lost` | 15 min, 2 | a database failover; repeated, the pooler |
| `advisory-lock-lost` | `advisory unlock reported not held; discarding connection` or `advisory lease lost; discarding its session` | 15 min, 2 | PgBouncer in transaction mode (above); once after a failover is expected |
| `chat-shutdown-workers` | `chat workers still running at shutdown` | 1 day, 3 | a provider call that ignores cancellation |
| `redis-degraded` (with `provisionRedis`) | a replica that logged `redis degraded: …` and not `redis recovered` for more than 5 minutes and is still logging | 1 day lookback, 2 | the Redis resource, `pad_redis_errors_total{mechanism}`; what degraded means is the table in "Running more than one replica" |
| `invalidation-channel` (with `provisionRedis`) | more than 4 of `invalidation publish failed…` and `invalidation subscription lost…` | 15 min, 3 | the other replicas serve stale caches until their TTL; `pad_inval_gap_total`, `pad_inval_dropped_total` |
| `replicas-at-ceiling` (with `maxReplicas` above `minReplicas`) | the platform's `Replicas` metric at `maxReplicas` for the whole window | 15 min, 3 | raise `maxReplicas` or `scaleConcurrentRequests`; idle tabs count as load |
| `replicas-below-min` (with `maxReplicas` above 1) | `Replicas` below `minReplicas` | 15 min, 2 | a replica that does not start or is replaced in a loop: the readiness and restart alerts |

The overdue line repeats at most hourly per replica, at the job's next attempt after the hour, so its window is two
hours: with one hour the alert would resolve and fire again every hour while the job stays overdue.

The readiness alert counts per replica and needs more failures than a stopping replica can log in its grace period
(one per probe period), so a deploy does not fire it, while a replica that stays unready long enough to leave
rotation does. A brief blip of fewer failures no longer alerts on its own; outside a drain, `/readyz` answers 503
only after three failed checks in a row anyway.

Not alerted, because they are normal operation: `job skipped: lock unavailable` (a database blip skips one attempt; a
job that keeps failing turns overdue) and `server draining` (every stop). One lost lease fires both lock alerts.

Not verified against a live workspace: the `ReplicaName_s` column of `ContainerAppSystemLogs_CL` (the query falls
back to one group, the old total count, through `column_ifexists`); whether ACA logs one line per failed probe or
folds repeats; whether it probes readiness on a terminating replica at all. The same holds for the replica column of
`ContainerAppConsoleLogs_CL` that `redis-degraded` groups by (`ContainerGroupName_s`; without it the alert still
fires, over all replicas as one) and for the `Replicas` platform metric the two replica alerts read.

The Prometheus metrics are not scraped, so `redis-degraded` and `invalidation-channel` read the app's own warnings (each
is logged once per transition or at most every 30 s per replica); the metrics they stand for are in "Rules for a
deployment that scrapes `/metrics`" above.
