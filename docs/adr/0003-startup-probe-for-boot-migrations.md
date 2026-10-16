# 3. A startup probe covers boot-time migrations

Date: 2026-10-06. Status: accepted.

## Context

Migrations run before the HTTP listener binds, so the liveness probe (`/healthz`, 10 s initial delay, 3 × 30 s) restarted any container whose migrations took longer than about 100 s, mid-transaction, into the same step again.

## Decision

Add a Startup probe on `/healthz` (period 30 s, failure threshold 10, about 5 minutes; both parameters in `infra/main.bicep`). Liveness and readiness only begin after it passes. The deploy smoke test waits at least that long for `/readyz`. Boot order is unchanged.

## Consequences

A migration that needs more than the startup window still fails, intentionally: long work belongs in post-boot jobs or concurrent index builds, not a boot step. Readiness stays false until migrations finish (the listener binds after them). In Single revision mode on Azure Container Apps, a revision that fails the startup probe fails the Deploy step while the previous revision keeps serving; the failed revision retries at every restart.
