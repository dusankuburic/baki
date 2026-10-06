# 4. Commit the request's transaction before the first response byte

Date: 2026-10-07. Status: accepted.

## Context

Requests that write run inside one row-level-security transaction that the RLS middleware owns. The middleware
passed the handler's response straight through, and committed only after the handler returned. A commit that
failed then (connection lost at `COMMIT`, an aborted transaction) was only logged: the client had already
received a 2xx for a write that did not persist.

Post-commit work (blob purges, emails, cache invalidation) ran from a registry with no notion of a rollback:
late registrations were silently lost, and hooks ran with a context whose RLS scope had already ended.

## Decision

1. The RLS middleware wraps the response in `txResponseWriter`. The transaction is decided at the first final
   `WriteHeader`, the first `Write` or a `Flush`, or when the handler returns having written nothing.
   - Below 400 it commits first. A failed commit becomes a 503 (`SERVICE_UNAVAILABLE`), with the headers restored
     to what they were before the handler ran.
   - 400 and above roll back.
2. Post-commit work goes through `internal/platform/txhook`, a registry that is pending, committed or aborted.
   Hooks run after the commit. Hooks registered after a rollback are dropped, so they never run.
3. The RLS middleware runs each hook through `storagedb.RLSHookRunner`: a fresh lazy RLS scope for the same
   user, committed when the hook returns. A hook's database access stays scoped, instead of reaching the pool,
   where the policies allow every row.
4. `withTx` hands its callback the transaction's context. When it opens its own transaction, that context
   carries a registry that is committed after the transaction commits, or aborted when it rolls back.

## Consequences

A client is never told that a write succeeded when it did not. Handlers must not touch the database after they
start writing the response: the scope has ended, and the statement fails. A scan of every handler found none
that do. A panic after the first byte leaves the write committed, and its post-commit work still runs. Work
that should not delay the response, such as SMTP, goes to a `lifecycle.Bounded` runner from inside the hook.
