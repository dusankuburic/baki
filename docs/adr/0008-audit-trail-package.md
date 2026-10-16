# 8. The audit trail is its own package and records the organisation and the actor

Date: 2026-10-07. Status: accepted.

## Context

The audit trail lived in `internal/api` behind a package-global pool and a `logAudit(...)` needing an `*http.Request`; services and background jobs could not record, and a row said neither which organisation it belonged to nor whether a session, an API token or a job acted.

## Decision

- `internal/audit` owns the trail (action constants, batched inserts, one retry, spill queue, redacted fallback) and does not import `internal/api` (`internal/archtest`). `audit.Provide` builds the recorder; its pool drains within the 5 s shutdown budget. Desktop gets a recorder that records nothing.
- `Record(ctx, audit.Entry{Action, ResourceType, ResourceID, OrgID, Meta})`: the actor comes from `auth.ActorFromContext` (a job records with `auth.WithActor(ctx, auth.SystemActor(name))`); the caller passes the organisation; user id/email come from claims alone.
- Columns (v33, expand): `org_id`, `actor_kind`, `token_id`, `actor_job`, `user_agent`, all nullable. `audit_events` stays without RLS. Erasure clears `user_agent` and `token_id` with the user id, email and IP. A once-per-boot backfill under a cross-replica advisory lock fills `org_id` for rows without one (idempotent, resumable, off the boot path).
- No package-global recorder: every caller holds an injected `*audit.Recorder`. Tests use `audit.NewCapture(ch)` and `audit.NewWithSink(store, sink)` instead of a global.

## Consequences

Services and jobs can record; the login-failure audit stays an explicit call (no actor to read). In local mode with a database, rows keep the empty user they always had; their actor kind says `user`.
