# 23. Singleton jobs and leases

Date: 2026-10-10. Status: accepted; adopted by every advisory-lock user except the blocking schema-migration lock
(Phase 5a waves 1 and 2b).

## Context

Background work that must run on one replica at a time (the scanner's sweep, the retention purge, the at-rest,
audit and re-seal backfills, the legacy migration, the knowledge HNSW build, the token-blacklist cleanup) took a
Postgres session advisory lock through `lock.GlobalLocker`, each with its own code around it and three different
error policies:

- **Fail open.** The scanner and the retention purge swept *without* the lock when asking for it failed (a database
  blip), so every replica swept at once and dispatched each alert once per replica (R3).
- **No lease check.** The lock lives on one pinned, idle connection while the work runs on other pool connections.
  When that connection died (a network cut, a Flexible Server failover, `pg_terminate_backend`) Postgres dropped the
  lock and the job ran on; another replica, or the next revision of a rolling deploy, could take the lock and run it
  beside it (R5). `releaseAdvisoryLock` ignored `pg_advisory_unlock`'s boolean, and `false` means the session did not
  hold the lock: lost while held, or a transaction-mode pooler that ran the unlock on another server session, where
  the lock then leaks and stalls the job on every replica (R4).
- **Panics.** A panic in the scanner's sweep outside its per-flow recovery ended its ticker loop for good (R6).
- **Shutdown.** The admin "scan now" ran under `context.Background()` and the legacy migration's hour-long run was a
  bare goroutine; neither was cancelled at shutdown, which closed the pool under them (R16, R17). A lock *error* on
  the migration start was answered 409 "already in progress".
- **Liveness.** The only signal was `pad_background_loop_*`, per process and recorded on completion. With two
  replicas the one without the lock skips forever by design, so a per-process "consecutive skips" count is wrong,
  and a leaked lock (held forever) never looks like anything but "held".
- **Keys.** The ten fixed keys were distinct by hand; the only test compared the retention key with hand-copied
  constants (R18).

## Decision

1. **Leases** (`internal/platform/lock`). `Lease{Lost() <-chan struct{}; Release()}` and
   `Leaser{TryLease(ctx, key) (Lease, bool, error)}` extend the package without changing `GlobalLocker`, so its test
   fakes stay as they are; `AsLeaser(GlobalLocker)` adapts one (its leases never report `Lost`; nil stays nil).
2. **The Postgres lease** (`postgres_lock.go`). `TryLease` takes the same session advisory lock on a pinned
   connection and checks it every 15 s (5 s timeout) with `pg_locks` (`pid = pg_backend_pid()`, the key's halves in
   `classid`/`objid`, `objsubid = 1`). A session that does not answer, or answers that it does not hold the lock,
   loses the lease: `Lost` closes, the session is discarded (whatever it still holds goes with it),
   `pad_advisory_lease_lost_total` counts it and a WARN line names the key. The heartbeat and `Release` share a mutex
   on the connection. `Release`, and the release func of the plain `TryGlobalLock`, treat an unlock that answers
   `false` as a lost lease: counted (`pad_advisory_unlock_not_held_total`), logged
   (`advisory unlock reported not held; discarding connection`), session discarded. The blacklist's private copy of
   the lock code (`tryDBAdvisoryLock`) is gone.
3. **`lifecycle.Singleton`.** `NewSingleton(Job, Leaser, JobState)` runs `Job{Spec, Every, RunAtStart, Timeout,
   RetryOnLockError, OverdueAfter, Run}`:
   - `Start` schedules it (a run at once with `RunAtStart`, then every `Every`; a job without a period runs only at
     start), `Stop` ends the schedule, cancels the run in flight (whoever started it) and waits for it within the
     5 s stop budget; `RunOnce(ctx)` makes one synchronous attempt under the caller's context; `RunNow(ctx)` takes the
     lock under `ctx` and starts the run in the background (an operator's "run it now"), so the caller learns at
     once whether it started.
   - **No leaser** (no database, the desktop): the process runs the job alone. **Lock error:** the attempt is
     skipped, fail closed (`pad_job_skipped_total{reason="lock_error"}`, WARN `job skipped: lock unavailable`), and
     retried after `RetryOnLockError` when that is sooner than the period. **Held elsewhere:** skipped
     (`reason="held"`, DEBUG). One run per process at a time also without a database (a second attempt is "held").
   - The run's context ends on `Timeout` (scheduled and `RunNow` runs), on `Stop` (cause `ErrJobStopped`) and when the
     lease is lost (cause `ErrLeaseLost`; WARN `job lease lost`). A panic is recovered per run and reported through
     `errreport` (location `job.<name>`): the schedule goes on.
   - `pad_job_runs_total{job,outcome=ok|failed|lease_lost|panic}` and `pad_job_duration_seconds{job}` count every
     run. A run whose unlock found the lock gone did its work (its success is recorded) but ran unguarded for a while:
     `lease_lost`, logged. The label is `outcome`, not `result`: the cardinality test holds `result` to the cache's
     closed set.
   - Each Singleton joins the process-wide group, and `lifecycle.DrainAll` (the server's stop) halts every job its
     owner did not stop, cancels its run and waits for it before draining the runners. A job that only runs on
     demand (the migration) is cancelled at shutdown this way.
4. **Persisted job state.** Migration 0041 (`expand`) adds `job_runs(name PK, last_started_at, last_success_at,
   last_error_at, last_error, holder_instance)`: one row per job, written by the replica that runs it (start, then
   end: success, or failure with the error text cut at 1 KiB, a later success keeping the failure as history). No
   row-level security (listed in `tablesWithoutRLS`: written and read by the jobs on the pool, never in a request, no
   tenant's data); claimed in `lifecycle_jobs.go` (not personal, no organisation, not purged: one row per job). The
   store is `storage/interfaces.JobRunStore`, a role of `StorageBackend` (Postgres and the test fake, held together by
   the `JobRuns_*` contract scenarios), so a job that has the backend has its state with no new wiring. The
   Singleton takes the narrower `lifecycle.JobState` (the role's first three methods): `storage/interfaces` imports
   `internal/auth`, which uses `internal/lifecycle`, so neither interface package can import the other and the role
   satisfies the interface structurally (asserted in `postgres_jobs.go`). Writes are best effort on their own 5 s
   deadline; a failed write is logged (`job state not recorded`) and never fails the job. The holder is
   `lifecycle.SetInstanceID` (default host name and process id).
5. **Overdue.** After every attempt without a success (skipped, failed, lease lost, panicked) the Singleton reads the
   last success on any replica (the persisted state, else what this process saw) and calls the job overdue when it is
   older than `OverdueAfter` (default twice `Every`; a job without a period is never overdue). It counts from the
   later of that success and its own process start, so a replica booting after a long downtime does not report a job
   nobody had the chance to run. Overdue sets `pad_job_overdue{job}` to 1 and logs WARN `job overdue` (job,
   last_success, attempts_without_success, overdue_after) when the job turns overdue and then at most once an hour
   per replica while it stays overdue, so an alert over a window of an hour or more keeps firing; the next success
   sets 0 and logs INFO `job no longer overdue`. `pad_job_last_success_timestamp_seconds{job}` follows the persisted
   value on every replica. A leaked lock now shows as overdue, not as an endless "held".
6. **Fixed log messages** (`lifecycle.LogJob*`), for alerts on the console logs; never reworded:

   | Level | `msg` | When |
   |---|---|---|
   | WARN | `job overdue` | no success anywhere past the threshold (then hourly) |
   | WARN | `job lease lost` | a run's lock was lost while it ran |
   | WARN | `job skipped: lock unavailable` | an attempt failed closed on a lock error |
   | WARN | `advisory unlock reported not held; discarding connection` | an unlock answered `false` (any lock user) |
   | WARN | `advisory lease lost; discarding its session` | the heartbeat lost a lease |

7. **Registry and keys.** A job is declared once, at package level: `lifecycle.DeclareJob(JobDecl{Name, LegacyKey,
   Schedule})`. A job that shipped before the registry keeps its constant (`LegacyKey`): a new key during a rolling
   deploy would let the old and the new revision each hold "the" lock and run the job side by side. A new job derives
   its key, FNV-1a-64 of `"baki:job:"+name`. A malformed name, a missing schedule, a duplicate name or key panics at
   init. `internal/lifecycle/testdata/jobs.golden` lists every job (name, key with its ASCII spelling, legacy or
   derived, schedule) and is reviewed like the caches golden (`-update-jobs`); a companion test fails when a
   `DeclareJob` in the source is not linked into that test binary. `TestJobKeys_Unique` reads every `*LockKey`
   constant from the source (the jobs', `bakiboot`, `padanalz`, and those still in package `main`), so a changed
   constant is seen: all distinct, a legacy key is one of them, a derived key is its name's hash and none of them.
   The two-`int4` key form stays deferred (changing keys needs a release that takes both).
8. **Adopted in this step:** the scanner (its schedule, `ScanOnce` and therefore the admin scan-now; a lock error
   retries in 5 minutes; a sweep is bounded by the interval as before; the run state through its backend), the audit
   `org_id` backfill (once per boot, a lock error retries in 5 minutes instead of waiting for the next boot), the legacy
   migration (`RunNow`: 409 when it runs anywhere, 503 on a lock error; cancelled at shutdown; `MigrationRunner.Stop`),
   the token-blacklist cleanup (every 5 minutes, overdue after an hour) and the knowledge HNSW build. The schema
   migration lock stays a blocking lock by design.
9. **Adopted in the app (Phase 5a wave 2b), which completes the adoption:** the retention purge (`retention_purge`,
   `bakiretn`: at boot and every `PAD_RETENTION_PURGE_INTERVAL`, a lock error skips and retries in 5 minutes where it
   used to sweep without the lock (R3), overdue after twice the interval, so a GDPR purge that ran nowhere for two
   intervals logs `job overdue`; a partial purge is a failed run), the field-encryption backfill
   (`field_encryption_backfill`, `bakifenc`) and the secret re-seal (`secret_reseal`, `bakikrot`), both once per boot
   like the audit backfill (a lock error retries in 5 minutes instead of waiting for the next boot). All three keep
   their legacy keys, record their runs in `job_runs` and are cancelled at shutdown: the two backfills by their own
   stop hooks (registered after the server's), the purge by the server's drain. `StorageResult` provides the backend
   as `lock.Leaser`, so the scanner (`scanner.WithLeaser`), the legacy migration (`runner.WithLeaser`), the audit
   backfill (`BackfillDeps.Leaser`) and the three jobs above run under the heartbeat: every advisory-lock user but the
   schema migration is a Singleton with a lease. Without a database the leaser is nil and each job runs alone.

   | Job | Key | Leaser | State | Schedule |
   |---|---|---|---|---|
   | `scanner` | bakiscan | backend | backend | every `PAD_SCAN_INTERVAL`; `ScanOnce` for scan-now |
   | `retention_purge` | bakiretn | backend | backend | at boot, every `PAD_RETENTION_PURGE_INTERVAL`; overdue 2× |
   | `field_encryption_backfill` | bakifenc | backend | backend | once per boot, 5 m, lock-error retry 5 m |
   | `secret_reseal` | bakikrot | backend | backend | once per boot, 30 m, lock-error retry 5 m |
   | `audit_org_backfill` | bakiaudo | backend (fx) | backend | once per boot, 10 m, lock-error retry 5 m |
   | `legacy_migration` | bakimigr | backend | none | on demand (admin), 1 h |
   | `token_blacklist_cleanup` | bakiblck | backend | backend | every 5 m; overdue 1 h |
   | `vector_index_build` | bakihnsw | backend | backend | once per boot |

## Consequences

- A database blip skips a sweep instead of letting every replica sweep. A lost lock ends the job's run, so two
  replicas no longer run it side by side for the rest of the run. A panic costs one run. An admin's sweep and a
  migration end when the server stops. Behind a transaction-mode pooler (unsupported) every lease is lost at its first
  heartbeat: loud (`lease_lost`, WARN) where the lock used to leak silently.
- Every running job pins one pooled connection and sends one small statement every 15 s on it; the HNSW build pins
  two. Each run writes two rows' worth of upserts and each skip reads one value.
- Logs: the scanner's `sweep already running on another replica` (INFO) and `advisory lock unavailable; sweeping
  without it` are gone (DEBUG `job skipped: running elsewhere`, WARN `job skipped: lock unavailable`); its `list flows
  failed` and `flow page did not advance` are now the `err` of WARN `job run failed`, as are the blacklist cleanup's
  and the HNSW build's failures. The audit backfill's failure is WARN `job run failed` (was ERROR), its shutdown
  interruption INFO `job run interrupted by shutdown`. `pad_background_*{task="audit_org_backfill"}` is replaced by
  the `pad_job_*` series; `pad_background_loop_*{loop="scanner"}` stays for one release (dashboards and the alert
  recipe in its help name it).
- `api.MigrationRunner.Start`/`StartAs` return an error (`ErrMigrationRunning`, `ErrMigrationLockUnavailable`,
  `ErrMigrationDisabled`) instead of a bool. `audit.ScheduleOrgBackfill` takes `audit.BackfillDeps` (fx.In) with an
  optional `lock.Leaser` and the backend; `scanner.New` takes `...scanner.Option` (`WithLeaser`).
- **Rollback.** 0041 only adds a table: the previous release boots on the new schema and never reads it. The keys did
  not change, so during a rolling deploy or a rollback the two revisions exclude each other as before. The previous
  release ignores an unlock's `false`, as it always did.

- Wave 2b (the app's jobs): the retention purge no longer sweeps on a lock error (it skips, WARN `job skipped: lock
  unavailable`, and retries in 5 minutes); a partial purge (`retention purge incomplete`, kept with its counts) is
  also WARN `job run failed` and does not count as a success, so a table that never purges shows as overdue; its
  panics are the job's (`job.retention_purge`, `pad_panics_total`) instead of `retention purge panicked`. The
  field-encryption backfill's failure is WARN `job run failed` (was ERROR `field encryption backfill failed`), its
  rows sealed an INFO line of their own. The re-seal keeps its report lines (`secret re-seal complete` and the rest);
  `secret re-seal interrupted by shutdown` is now `secret re-seal interrupted` (the job's line beside it says whether
  shutdown or a lost lock ended it), and `secret re-seal skipped: lock unavailable` / `secret re-seal runs on another
  replica` are the job's DEBUG/WARN skip lines. `pad_background_task_*{task="field_encryption_backfill"|"secret_reseal"}`
  are replaced by the `pad_job_*` series; `pad_background_loop_*{loop="retention_purge"}` stays. `job_runs` gains
  the rows `retention_purge`, `field_encryption_backfill` and `secret_reseal`. The retention purge is halted by the
  server's drain (`lifecycle.DrainAll`), before its own stop hook, since it joins the process-wide group.
- Wave 2b (R20): the admin scan-now and the legacy migration are wired wherever the deployment has a server store
  (`capability.ServerStore`: cloud and local+db, with or without accounts), as the scheduled sweep and the purge
  already were; they used to be cloud-only, so local+db answered 503.

## Wiring left to the app

- `lifecycle.SetInstanceID(cfg.Runtime.InstanceID)` at boot: not wired (it needs an invoke in `app.go`, or a line in
  `provideStorageBackend`); the holder recorded in `job_runs` is the default host name and process id, which on
  Container Apps is the replica name.
- `internal/lifecycle/singleton_registry_test.go` must import `_ "pad-analyzer/internal/app"` beside the other job
  packages, so the golden lists the app's three jobs and `TestJobRegistry_CoversEveryDeclaration` passes (the golden
  already has their lines).

## Not done

- The legacy migration records no job state: the runner has no store handle, and its status stays per process.
- Replicas whose schedules are not aligned each run a periodic job once per interval when the lock is free at their
  tick (as before): the lock prevents overlap, not repetition. A "succeeded elsewhere within the period" skip on the
  persisted state would make it once per deployment; not done here.
- One pinned connection per running job; a shared lock session holding every job lock is a later optimisation.
- An admin view of `job_runs`.
