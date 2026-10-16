# 2. Migration compatibility markers and schema-aware rollback

Date: 2026-10-06. Status: accepted.

## Context

The deploy workflow's automatic rollback put the previous image back after a failed smoke test; if the failed release had applied a migration, the old image then crash-looped against the newer schema, turning a failed deploy into an outage.

## Decision

1. Every new migration declares `-- baki:compat expand` (the previous release keeps working on the new schema) or `breaking`; the value is recorded in `schema_migrations.compat` when applied. A binary boots against a newer schema only when every newer step is expand-only; steps applied before the column existed count as breaking.
2. New steps (v32 on) are embedded `.sql` files in `internal/storage/database/migrations/`, text normalized (no BOM, LF) before hashing; `.gitattributes` keeps them LF. v1-v31 stay frozen Go literals.
3. Each step runs with `SET LOCAL lock_timeout` and is retried when a table lock is unavailable or on a deadlock (`40P01`).
4. The deploy workflow rolls the image back automatically only when the release changed no migration sources; otherwise it fails with the manual options.
5. Expand/contract is the default way to change shape: an expand step plus code that reads both shapes, then the contract step one release later.

## Consequences

Rolling the image back after an expand-only release needs no schema rollback; a rollback to a release older than this ADR still needs `bakicli migrate down`. A wrong `expand` on a breaking step would let an old binary run against a shape it does not understand, so reviews check the marker against the migrations README criteria.
