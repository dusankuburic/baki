# 4. Commit the request's transaction before the first response byte

Date: 2026-10-07. Status: accepted.

## Context

The RLS middleware committed only after the handler returned, so a failed commit (connection lost at `COMMIT`, an aborted transaction) was merely logged after the client had already received a 2xx for a write that did not persist.

## Decision

1. The RLS middleware wraps the response in `txResponseWriter`. The transaction is decided at the first final `WriteHeader`, the first `Write` or `Flush`, or when the handler returns having written nothing. Below 400 it commits first — a failed commit becomes a 503 with headers restored to pre-handler state. 400 and above roll back. A read whose transaction could not start (`ErrRLSUnavailable`) gets a 503 with `Retry-After: 1` whatever the handler answered.
2. Post-commit work goes through `internal/platform/txhook`, a registry that is pending, committed or aborted. Hooks run after the commit; hooks registered after a rollback are dropped and never run.
3. Each hook runs through `storagedb.RLSHookRunner`: a fresh lazy RLS scope for the same user, committed when the hook returns — never the pool, where the policies allow every row.
4. `withTx` hands its callback the transaction's context, which carries a registry committed after the transaction commits and aborted when it rolls back.

## Consequences

A client is never told a write succeeded when it did not. Handlers must not touch the database after they start writing the response (the scope has ended). A panic after the first byte leaves the write committed and its post-commit work still runs. Work that should not delay the response (SMTP) goes to a `lifecycle.Bounded` runner from inside the hook.
