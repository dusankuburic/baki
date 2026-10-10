# Operating a deployment

Database connections, pooling, the stop budget and the alerts of a cloud deployment (`infra/main.bicep` on Azure
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
| `readiness-failing` | one replica logging at least `max(readinessFailureThreshold, stopGraceSeconds / probePeriodSeconds + 2)` readiness failures (6 by default) | 10 min, 1 | `/readyz`: Postgres, blob, Redis |
| `job-overdue` | `job overdue` | 2 h, 2 | the line's `job`, `pad_job_overdue`, the job's `job run failed` lines |
| `job-lease-lost` | `job lease lost` | 15 min, 2 | a database failover; repeated, the pooler |
| `advisory-lock-lost` | `advisory unlock reported not held; discarding connection` or `advisory lease lost; discarding its session` | 15 min, 2 | PgBouncer in transaction mode (above); once after a failover is expected |
| `chat-shutdown-workers` | `chat workers still running at shutdown` | 1 day, 3 | a provider call that ignores cancellation |

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
folds repeats; whether it probes readiness on a terminating replica at all.
