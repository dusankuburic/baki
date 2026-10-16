# Schema migrations (v32 and later)

Versions 1–31 are frozen Go string literals in `../postgres_migrations.go` — never edit them; the boot-time checksum check refuses a database whose recorded checksum no longer matches. Every new step is a file in this directory, embedded into the binary and appended after v31.

- `NNNN_name.up.sql` — the step (e.g. `0032_audit_org_id.up.sql`); the 4-digit version continues the sequence without gaps; the name is lowercase letters, digits, `_`.
- `NNNN_name.down.sql` (optional) — the reverse, used only by `bakicli migrate down`, with the app stopped. No down file means the step is irreversible: `migrate down` refuses to cross it.

Each step runs in one transaction with its `schema_migrations` row; the checksum covers the up file's text after stripping a UTF-8 BOM and converting CRLF to LF (`.gitattributes` keeps `*.sql` LF).

## Compatibility directive (required)

Every up file carries exactly one line: `-- baki:compat expand` or `-- baki:compat breaking`.

- **expand** — the release before keeps working on the new schema (new tables, nullable columns, columns with a default, new indexes/functions). An older binary boots on a schema ahead of it by expand-only steps, so an image rollback is safe without a schema rollback.
- **breaking** — the previous release cannot run against it (dropping/renaming a column or table, `NOT NULL` without a default, a type change, RLS changes that hide rows); an older binary refuses to boot until the schema is rolled back.

Prefer expand/contract: ship the expand step with code that works with both shapes; ship the drop one release later, after no deployed code reads the old shape. Pending contract steps: v35 `settings_sections` (ADR 0012) and `CHECK (id <> '')` on `policies` after v48 (rekey `''` rows first, then add `NOT VALID` and validate after boot via `SchemaUpkeep`, its item's statement re-keying what the CHECK refuses).

Steps and downs run under `SET LOCAL lock_timeout = 5s`, retried on lock timeouts and deadlocks (max 6 attempts). Row work first, only on orphans of row-per-entity tables; growing tables are SchemaUpkeep's. Table locks in all-or-none groups (the 0048/0049 DO blocks): wait for a busy table alone (200 ms slices, extended to `deadlock_timeout` + 200 ms once if an autovacuum holds it), holding none of the group, so no concurrent multi-table writer deadlocks (an erasure writes `org_invites` both before and after `share_tokens`). Lock `users` and `refresh_tokens` in the last group with only their own statements after; never hold a write-blocking lock on `flow_versions` or `audit_events`. Foreign keys and CHECKs `NOT VALID`; build/drop indexes of busy or growing tables `CONCURRENTLY` after boot via `SchemaUpkeep` (v48/v49); never reuse a retired index name. Tests: `TestMigration0048_BlocksWritesOnlyInItsTail` checks reads and locks statement by statement; `TestMigration0048_NoDeadlockWithThePreviousRelease` and `_SignInsNeverWaitOnItsLockWaits` check lock waits against concurrent writes. No `CREATE INDEX CONCURRENTLY` or long backfills inside a step; run backfills as resumable jobs.

## Checks

`go test ./internal/storage/database/` checks: the sequence has no gaps; every up file has its directive; shipped steps match `testdata/migration_checksums.golden`. Refresh the golden when adding a step: `go test ./internal/storage/database/ -run TestMigrationChecksums_Golden -update-migration-checksums`. CI also boots the previous release on the migrated schema (`previous-release-boot`): a `breaking` step fails it, and so does an `expand` step the previous release cannot boot on. Operator tooling: `bakicli migrate preflight` checks for integrity violations (case-duplicate emails, duplicate invite tokens) and checksum drift before deploy; `bakicli migrate adopt-checksum <version>` reconciles drifted pre-release migration texts whose schema shape matches.
