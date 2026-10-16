# 7. Creates honour an Idempotency-Key inside the request transaction

Date: 2026-10-07. Status: accepted.

## Context

A client that lost the response to a create could not tell whether it happened, and retrying created a second flow, organisation, API token or invite.

## Decision

1. `route.Idempotent()` marks a create route; the router refuses to start if it is unauthenticated, `NoTx`, or a read.
2. Opt-in per request: `Idempotency-Key` of 1-255 printable ASCII characters (else 400 `IDEMPOTENCY_KEY_INVALID`), scoped per user. The record keeps a fingerprint (method, route pattern, SHA-256 of path/query/body); the same key with another fingerprint is 422 `IDEMPOTENCY_KEY_REUSED`.
3. The layer runs right after the RLS middleware: claims the key in `idempotency_keys` (v32), runs the route with the response held back, and stores status, content type and body on the claim before releasing the response. Below 400 the record commits with the route's writes; ≥400 rolls back (the retry runs again); a record that cannot be stored becomes 500/503, rolling the create back too.
4. A replay returns the stored response with `Idempotent-Replayed: true`; the route does not run.
5. In-flight duplicates are refused, not queued: `pg_try_advisory_xact_lock` on user and key; a held lock answers 409 `IDEMPOTENCY_IN_PROGRESS` with `Retry-After: 1` (waiting would hold pooled connections for the first request's whole run).
6. Bounds: bodies over 256 KiB are not stored (replay has an empty body); records answer for 24 hours; bodies are sealed at rest (ADR 0005); account erasure cascades. Without row-level security (desktop, local mode) the header is ignored.

## Consequences

A replay does not re-check authorization: within 24 h a caller who since lost access still gets the stored response. A route that streams or hijacks the connection must not be idempotent. CORS allows `Idempotency-Key` and exposes `Idempotent-Replayed` to allowed origins.
