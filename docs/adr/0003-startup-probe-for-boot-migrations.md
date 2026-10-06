# 3. A startup probe covers boot-time migrations

Date: 2026-10-06. Status: accepted.

## Context

Migrations run while the storage backend is constructed, before the HTTP listener binds. The liveness probe
(`/healthz`, 10 s initial delay, 3 × 30 s) therefore restarted any container whose migrations took longer than
about 100 s, mid-transaction, into the same step again. The refactoring proposal suggested binding the listener
before migrating and serving `/healthz` during the migration.

## Decision

Add a Startup probe on `/healthz` (period 30 s, failure threshold 10, so about 5 minutes by default, both
parameters in `infra/main.bicep`). Liveness and readiness only begin after it passes. The deploy smoke test
waits at least that long for `/readyz`. Boot order is unchanged.

## Consequences

Long migrations are no longer killed, with a configuration change instead of a restructured boot. Readiness
already stays false until migrations finish, because the listener binds after them. A migration that needs more
than the startup window still fails, and that is intended: long work belongs in post-boot jobs or concurrent
index builds (see the migrations README), not in a boot step. If boot ever needs to serve traffic while
migrating, the listener-first design remains available.
