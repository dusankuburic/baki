# Transactions in the database backend

How a storage write gets its transaction, and why there is no public `Tx(ctx, fn)`.

## The request transaction

An authenticated request in a deployment with row-level security runs in one transaction, which the RLS middleware
opens through `reqtx.Opener` and commits before the first response byte (ADR 0004). Every storage call of the
request joins it: reads through `b.query(ctx)`, writes through `withTx`. So a service's store calls within one
request are atomic together, and the row-level policies see the caller.

Routes declared `route.NoTx()` (streaming, CPU-heavy, the knowledge upload and re-index), the public routes and the
background jobs run without it. There each store method is its own transaction.

## `withTx` and its variants (`internal/storage/database/rls.go`)

| | In a request transaction | Without one |
|---|---|---|
| `withTx(ctx, fn)` | joins it | opens its own, with no user: the policies allow every row |
| `withTxAs(ctx, userID, fn)` | joins it | opens its own as `userID` (`app.current_user_id` set), so the `WITH CHECK` policies are evaluated; its post-commit hooks get a fresh scope for the same user |
| `withOwnTx(ctx, isolation, fn)` | never joins: its own connection and transaction | the same; at `sql.LevelSerializable` a serialization failure reruns `fn` in a new transaction, five attempts in all |

- `withTxAs` serves the writes whose policies check the writer even off the request transaction: the knowledge
  document, chunk and embedding writes, and the batch triage upsert.
- `withOwnTx` serves the writes that must be durable when the request rolls back, on tables without row-level
  security: `CreateUser` (serializable, so the first user alone becomes admin) and `SaveAuditEvents`.
- A fail-closed request scope that could not start makes `withTx` and `withTxAs` return `ErrRLSUnavailable` rather
  than open a transaction of their own.
- When they open their own transaction, `fn` gets a context carrying it, so nested store calls join it, and a
  post-commit registry (`txhook`).
- All three open their transactions through `openTx`, and so does `BeginRLS` (the request's scope).
  `TestArch_TransactionsOpenedOnlyWhereAllowed` counts the calls of `BeginTx`, `BeginRLS` and `openTx`: only
  `rls.go` and the migration runner have any.

## No public `Tx(ctx, fn)`

The Phase 3 proposal asked for a public transactional entry point. It is not added, because no service needs one:

- Inside a request, the request transaction already makes a service's store calls atomic together.
- A service that needs several store calls atomic outside a request can open the scope the middleware opens:
  `reqtx.Opener.Open(ctx, userID, false)`, the calls, then `Commit` (with `HookRunner` for the post-commit hooks).
  Every store call joins it as it joins the request's. It is a database-only fx value, nil without row-level
  security (ADR 0010). No service does this today.
- A single store method that must be atomic opens its transaction itself, with `withTx` or a variant.

Two multi-call writes run outside a request transaction today:

- **Knowledge upload** (`rag.KnowledgeService.AddDocument`, a `NoTx` route because embedding takes seconds) saves
  the new document, then its chunks, then deletes the versions it supersedes, each in a transaction of its own. The
  order makes a failure safe: a failed chunk save deletes the new document, and a failed delete leaves a duplicate,
  not a gap (`internal/rag/adddoc_atomicity_test.go`). A crash between the first two writes leaves a document
  without chunks; opening a scope through `reqtx.Opener` after the embedding would close that window.
- **SSO sign-up** (`service.IdentityService.ResolveSSOUser`, a public route) creates the user, then the identity
  link. If the link fails, the account stays without it: the next sign-in through the provider is refused, because
  the email belongs to a verified account, and the account has no password. A transaction around both calls would
  not help, because `CreateUser` commits on its own by design; this needs one store method that writes both, or a
  compensating delete.
