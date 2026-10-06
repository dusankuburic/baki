# 2. Migration compatibility markers and schema-aware rollback

Date: 2026-10-06. Status: accepted.

## Context

The binary refuses to boot against a schema newer than it knows. That guard is correct, but the deploy
workflow's automatic rollback put the previous image back after a failed smoke test. If the failed release had
applied a migration, the old image then crash-looped against the newer schema, turning a failed deploy into an
outage. Migrations were also Go string literals in one 2,148-line file, hashed byte for byte, so a CRLF checkout
could not hold `.sql` files safely.

## Decision

1. Every new migration declares `-- baki:compat expand` (the previous release keeps working on the new schema)
   or `breaking`. The value is recorded in `schema_migrations.compat` when the step is applied. A binary boots
   against a newer schema when every newer step is recorded as expand-only, and refuses otherwise. Steps
   applied before the column existed count as breaking.
2. New steps (v32 on) are embedded `.sql` files (`internal/storage/database/migrations/`). Their text is
   normalized (no BOM, LF) before hashing, and `.gitattributes` keeps them LF. v1–v31 stay frozen Go literals.
3. Each step runs with `SET LOCAL lock_timeout` and is retried when a table lock is not available, so a step
   queued behind the serving revision cannot stall that revision's queries indefinitely.
4. The deploy workflow rolls the image back automatically only when the release changed no migration
   sources, which it determines from the serving image's commit tag and `git diff`. Otherwise it fails with the
   manual options.
5. Expand/contract is the default way to change shape: ship an expand step plus code that reads both shapes,
   then the contract step one release later.

## Consequences

Rolling the image back after an expand-only release needs no schema rollback. The guard only helps binaries
that contain it, so a rollback to a release older than this ADR still needs `bakicli migrate down`. Authors must
classify each step. A wrong `expand` on a breaking step would let an old binary run against a shape it does not
understand, so reviews check the marker against the criteria in the migrations README.
