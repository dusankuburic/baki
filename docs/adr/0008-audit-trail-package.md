# 8. The audit trail is its own package and records the organisation and the actor

Date: 2026-10-07. Status: accepted.

## Context

The audit trail lived in `internal/api`: a package-global worker pool (`InitAuditPool`, `ShutdownAuditPool`, the
on-disk spill queue) and `logAudit(ctx, backend, r, trustedProxies, …)`, which needed an `*http.Request` for the
client address. Services and background jobs could not record, and a row said neither which organisation it
belonged to nor whether a session, an API token or a job acted. Listing an organisation's trail was impossible.

## Decision

- **`internal/audit`** owns the trail and does not import `internal/api` (`internal/archtest`). It holds the action
  constants (`audit.Action*`, `audit.AllActions`), and a `*audit.Recorder` holds the pool: batched inserts, one
  retry, the spill queue, salvage after a panic and the redacted log fallback.
- **fx owns the lifecycle.** `audit.Provide` builds the recorder; its pool starts with the app and drains within
  the 5 s shutdown budget when the app stops, after the HTTP server's drain of the background runners. Desktop
  (no server store) gets a recorder that records nothing.
- **`Record(ctx, audit.Entry{Action, ResourceType, ResourceID, OrgID, Meta})`.** Who acted comes from
  `auth.ActorFromContext`; a background job records with `auth.WithActor(ctx, auth.SystemActor(name))`. The user
  id and email name an account, so they come from the claims alone. The client address and user agent come from
  the request's `reqinfo.Info`. The caller passes the organisation.
- **Columns (v33, expand):** `org_id`, `actor_kind`, `token_id`, `actor_job`, `user_agent`, all nullable, plus a
  partial index on `(org_id, created_at DESC, id)`. `audit_events` stays without RLS. Erasure clears `user_agent`
  and `token_id` with the user id, email and IP, and the user agent of the user's pre-auth rows (login, register,
  password reset), which name the user only as their resource. The admin audit response and the data-subject export keep their
  shape.
- **Backfill:** once per boot, under a cross-replica advisory lock and off the boot path, `org_id` is filled for
  rows without one: an org resource's row gets its resource id, a flow's row the flow's organisation, everything
  else stays NULL. Batches commit on their own and only NULL rows are written, so the job is idempotent and an
  interrupted run continues on the next boot. It walks the flows, not the log, so rows that never resolve are not
  re-read, and every statement writes at most a fixed number of rows, so a page of busy flows still finishes within
  the pool's statement timeout.
- **No package-global recorder.** Every caller holds an injected `*audit.Recorder`. The transitional path of
  Phase 2a's first wave (`api.logAudit` writing through a default recorder, `audit.SetDefault`/`audit.Submit`, and
  the `api.AuditAction*` re-exports) is gone; a recorder that never started writes each row detached to its store.
- **Tests** assert on rows without a global: `audit.NewCapture(ch)` is a running recorder whose queue is `ch`, and
  `audit.NewWithSink(store, sink)` is a never-started recorder that asks `sink` on each row where to put it (a
  channel takes it, in order and without blocking; none: a detached write to `store`). The `internal/api` tests
  build their handlers' recorders with `NewWithSink` over a package variable that `captureAuditEvents` sets, so a
  handler built before the capture is captured too.

## Consequences

Services and jobs can record, and an organisation's trail can be listed once the call sites pass the
organisation. Rows written before the call sites passed it get their organisation from the next boot's backfill
where it can be derived. The login-failure audit stays an explicit call: there is no actor to read. In local mode
with a database, rows keep the empty user they always had; their actor kind says `user`.
