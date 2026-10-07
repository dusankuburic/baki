# 7. Creates honour an Idempotency-Key inside the request transaction

Date: 2026-10-07. Status: accepted.

## Context

A client that loses the response to a create cannot tell whether it happened, and retrying it created a second
flow, organisation, API token or invite. The writes of a request already run in one RLS transaction that commits
before the first response byte (ADR 0004), so a request's outcome is decided exactly when its response is.

## Decision

1. **Declared on the route.** `route.Idempotent()` marks a create route. The router refuses to start when such a
   route is not authenticated, runs without the request transaction (`NoTx`) or is a read.
2. **Opt-in per request.** Only a request that carries `Idempotency-Key` uses it. The key is 1–255 printable ASCII
   characters; anything else is a 400 `IDEMPOTENCY_KEY_INVALID`. Keys are scoped per user. The record keeps a
   fingerprint: the method, the route pattern, and a SHA-256 of the path, query and body. The path counts because
   one pattern serves many flows and organisations. The same key with another fingerprint is a 422
   `IDEMPOTENCY_KEY_REUSED`.
3. **Inside the request transaction.** The idempotency layer runs right after the RLS middleware. It claims the key
   in `idempotency_keys` (migration v32), runs the route with its response held back, and stores the status, content
   type and body on the claim before it releases the response, which is what commits.
   - Below 400 the record commits with the route's writes.
   - 400 and above roll back, and the claim with them, so the retry runs again.
   - A response whose record cannot be stored becomes a 500 or 503, which rolls the create back too.
4. **Replay.** A retry gets the stored status, content type and body back, with `Idempotent-Replayed: true`. The
   route does not run, and the other headers it set the first time are not replayed.
5. **Duplicates in flight are refused, not queued.** The claim takes `pg_try_advisory_xact_lock` on the user and key
   first. A request whose key another transaction holds gets a 409 `IDEMPOTENCY_IN_PROGRESS` with `Retry-After: 1`.
   Waiting on the first transaction's uncommitted row would hold a pooled connection, and the waiting request's
   deadline, for as long as the first one runs, and a burst of retries could drain the pool the first request still
   needs. The lock ends with the first transaction, before its response leaves, so a retry that follows a response
   always finds the record.
6. **Bounds.**
   - A body over 256 KiB is not stored: the record keeps the status alone, and a replay has an empty body.
   - A record answers for 24 hours. After that the key is free again, and the retention sweep purges the record.
   - Bodies are sealed at rest like secret columns (ADR 0005), since a new API token or invite token is in them.
   - Account erasure removes a user's records (the foreign key cascades).
7. **No request transaction, no key.** Without accounts and row-level security (the desktop and local mode) the
   header is ignored.

## Consequences

Clients can retry the declared creates with one key per user action. Each such request costs three more statements
(the lock, the claim, the record), and its response reaches the client only when the route returns. A route that
streams or hijacks the connection must not be declared idempotent. A replay does not check authorization again:
within a day, a caller who has since lost access still gets the response they were already sent. Rows written
before field encryption was turned on are not backfilled; they expire within the day. The CORS layer allows the
`Idempotency-Key` header and exposes `Idempotent-Replayed` to the allowed origins, so a cross-origin client (a dev
server against a cloud backend) can send the key and see a replay.
