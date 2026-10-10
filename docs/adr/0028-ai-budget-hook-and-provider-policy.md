# 28. AI budget as a pre-call hook, and a provider policy at the factory

Date: 2026-10-10. Status: accepted, implemented (Phase 4b, budget wave).

## Context

The daily AI budget (`AI.DailyBudget`, `AI.PerUserDailyBudget`) was enforced by two callers of the provider
chains: the chat service (a reservation and a check at the start of a turn, a re-check before every tool-loop
iteration) and the knowledge base (one check before an upload or a re-index). Every other billed call was
ungated: the provider connection tests (`POST /api/providers/test` and the org variant, one 16-token `Chat` per
candidate model, recorded and billed to the org for the shared-credential form) and the per-turn query embed of
the knowledge search, which was covered only because the chat turn's check ran first. Nothing pinned that a new
caller would be gated, and a new factory builder could not carry the budget at all: the chains had no hook.

The only provider policy was the deployment-wide `AI.Providers[id].Enabled` flag, read in `prepareTurn` after
the provider was built (so a disabled provider's key had been read) and nowhere else: not for embeddings, not
for the connection tests, not for listings. Organisations had no way to say which providers may see their
flows and corpus. The disable's error reached the client without a code.

The in-flight ledger (what running chat streams have reserved and spent) was a `sync.Map` inside `AIBudget`,
with no seam for the shared ledger Phase 5b needs across replicas.

## Decision

1. **A pre-call hook in the audited wrapper.** `ai.BudgetChecker` (`Check(ctx, BudgetCall{Scope, OrgID,
   ProviderID, Model, Kind})`) is consulted first in `Chat`, `Stream` and `Embed` of every chain the factory
   builds — above the circuit breaker and the retry policy, so a refusal is not a provider failure and is never
   retried. The checker's error is returned unwrapped: `errors.Is(err, service.ErrAIBudgetExceeded)` keeps
   classifying it, and the wire codes are unchanged (`AI_BUDGET_EXCEEDED` / `AI_BUDGET_UNAVAILABLE` on streams
   and on `ProviderTestResult.code`, 429 `BUDGET_EXCEEDED` / 503 `BUDGET_UNAVAILABLE` on the knowledge routes).
   Alias-model providers (Azure AI Foundry) are exempt in the hook as they were in both callers' gates: baki
   neither prices nor counts their usage. The factory takes the checker at construction
   (`ai.WithBudgetChecker`, a `FactoryOption` of `NewProviderFactory`); `service.AIBudget` implements it.
2. **Who is charged is resolved once, for the gate and the row.** `ai.WithBudgetScope(ctx, scope, orgID)`
   names the spender for calls that carry no request of their own (`Embed`); without it the wrapper uses its
   own scope, the request's org, else the shared credential's org — the resolution the usage row already made.
   The knowledge base seeds it per run, so an org corpus embedded through a member's own key is gated on the
   org AND its usage row names the org (it recorded no org before: the org budget never saw that spend).
3. **One usage read per request.** `ai.WithBudgetMemo(ctx)` attaches a request-scoped memo of daily-usage
   reads keyed by (scope, org); the chat worker seeds it on the stream context together with the stream's own
   ledger entry (a service-private key, `withStreamSpend`), so the turn's start check, the loop re-checks, the
   hook before every call and the RAG query embed inside the turn share one `GetDailyUsage`. The knowledge base
   seeds one per run (an upload's N batches cost one read). The memo replaces the loop-private `turnBudgetUsage`;
   it is request-scoped, never shared across requests, so it needs no declared cache spec. The in-flight ledger
   covers the memo's staleness, as before.
4. **Chat keeps its two checks.** The start check refuses before RAG starts and before the user turn is
   persisted ("no spend on refusal"); the loop re-check updates the reservation and reports mid-loop stops with
   their prefix. The hook is the net under them, and the only gate for everything else.
5. **The ledger is a seam.** `AIBudget` holds a `spendLedger` (`hold`, `release`, `others`); `memLedger` is
   today's `sync.Map` and never errs. A ledger that cannot answer makes the check fail closed with
   `ErrAIBudgetUnavailable` (client-safe copy, the cause logged) — exactly the "Redis down → budget fails
   closed" rule Phase 5b wants for the shared ledger, pinned now by `TestAIBudget_LedgerErrorFailsClosed`.
6. **A provider policy, asked before any key is read.** `ai.ProviderPolicy` (`Allowed(ctx, scope, orgID,
   providerID)`) is consulted by every factory builder — `For`, `ForEmbedding`, `ForOrg`, `ForOrgCredential`,
   `ForEmbeddingOrg`, `ForAzureConnection`, `ForAzureConnectionEmbedding` — before the credential is resolved,
   so a refused provider never sees a key. Every builder gained a `context.Context` for the policy's reads and
   the budget scope. `service.ProviderPolicy` implements it: the deployment's `Enabled` flag (the one
   unambiguous disable, as the chat gate read it) for every build, and the organisation's allowed-provider list
   for a build that names an org. A personal builder on an org flow (a member's own key) still names the flow's
   organisation through the context's budget scope: an organisation's content goes only to providers the
   organisation admitted, whichever credential the chain runs on. An unreadable org settings document is an
   error of its own (the chat path words it as the generic setup failure), never "allowed".
7. **The org allowlist is a settings field.** `AI.AllowedProviders []string` (`allowedProviders`, omitempty) on
   the org settings document, additive, no schema change, no version step (ADR 0012: prefer adding fields).
   Empty — unset, or saved empty, which the wire cannot tell apart — allows every provider the deployment
   offers. Unread in the deployment's and a user's settings. Org settings are saved through the existing org
   settings route, which replaces the list only when the body names `ai.allowedProviders` (an explicit `[]`
   clears it) and keeps the stored list otherwise: the document is written whole, and a partial write — the
   Rules panel posts `{analysis: {rules}}` — must not wipe the org's policy. Every listed id must be a registered
   provider (400 `PROVIDER_UNKNOWN`), since a typo would lock the org out of every provider; the audit row of a
   write that names the list records it. A settings panel for the list is not part of this change.
8. **Codes.** `PROVIDER_NOT_ALLOWED` (reserved by ADR 0025) on the stream's error event (with the policy's
   copy, which names the provider and what to do), as `ProviderTestResult.code`, 400 on the knowledge
   routes and on the context preview; the deployment disable, which reached the client codeless, folds into it.
   A policy that could not be read (the org's settings document failed to load) is `ai.ErrProviderPolicyUnavailable`
   with fixed copy, the cause logged: `PROVIDER_POLICY_UNAVAILABLE`, 503 on the knowledge routes and the code on
   a connection test's result — never "not configured", and never the store's text. The connection tests also
   carry `AI_BUDGET_EXCEEDED` / `AI_BUDGET_UNAVAILABLE` when the hook refused them; `ok:false` and `error` as
   before. The frontend's error bubble treats the new code like `PROVIDER_UNKNOWN`: a title, a relist, "Choose
   another model".
9. **Metrics.** `pad_ai_budget_refusals_total{provider, call, reason}` counts the refusals: `call` in chat /
   stream / embed, or `build` for the policy's refusal at the factory; `reason` in exceeded / unavailable /
   not_allowed; every label a closed set. `pad_usage_dropped_total{reason}` names the runner's refusal `runner`
   (it counted the shared runner's cap as `shutdown`); `pad_background_dropped_total{task="ai_usage_record"}`
   says whether it was the cap or the drain.

## Consequences

- Every billed provider call in the process is gated: the connection tests (`TestProviderService_TestConnection_
  RefusedOverBudget`, the org form on the org's budget), the knowledge search's query embed
  (`TestKnowledge_SearchQueryEmbedIsRefusedOverBudget`), and any caller a later phase adds, as long as it builds
  through the factory. A chain built around the factory (`NewAuditedProvider` directly, the Azure probes) carries
  no checker: the probes make no billed model call by design (ADR 0025).
- Cost: zero extra usage reads for a chat turn and one per knowledge-base run (the memo); the policy costs one
  `LoadOrgSettings` per org build (one per chat turn, per knowledge run, per uncached org listing). No cache
  yet; if `ListProviders` shows it, a declared cache keyed by org invalidated on `events.SettingsChanged`.
- The hook refuses with the same sentinels the callers' gates use, so the stream classification, the knowledge
  routes' rendering and the frontend's budget notice are unchanged. The only new client-visible class is
  `PROVIDER_NOT_ALLOWED`, which older clients render as a generic error bubble with the server's message.
- The desktop profile has no usage store: `GetDailyUsage` reads $0 there, so only the in-flight ledger can trip
  the budget Settings shows (`modes.md`). Unchanged by this change; now stated.
- `ai.NewProviderFactory` keeps its four positional parameters (about 76 test call sites) and takes the checker
  and the policy as options; `ProvideAI` passes the `ProvideAIBudget` and `ProvideProviderPolicy` instances,
  both built from settings and the backend alone — no cycle through the services that take the factory.
- The builders' `ctx` parameter touched every call site (10 in production, about 75 in tests), mechanically.

## Rollback

The previous release runs on the same schema (no migration) and the same settings documents: it ignores
`allowedProviders` and keeps it — `settingsdoc` merges a write onto the stored section and carries the keys the
writing binary does not declare, so the list survives every save the previous release makes and is enforced
again on roll-forward. The hazard is the opposite one: a partial write by a release that DECLARES the field
dropped it, which is why this release's org settings route keeps the stored list unless the body names it (see
decision 7). A client of this release against
the previous server never sees `PROVIDER_NOT_ALLOWED` or a coded connection-test refusal and falls back to its
generic handling; a client of the previous release against this server renders the new code as a generic error
bubble with the server's message. The metrics golden is the only artefact to refresh on either direction.

## Open

- An org settings panel for `AI.AllowedProviders` (wire-additive; the backend enforces it today for a list
  saved through `POST /api/system/settings/org/{id}`).
- A declared cache for the policy's org settings read, if the listing shows the cost.
- The shared ledger (Phase 5b) behind `spendLedger`; the fail-closed rule is pinned, the implementation is not.
- A test that drives `pad_usage_dropped_total{reason="runner"}`: it needs the process-wide usage runner at its
  cap (1024 blocked writes), which would close the runner for the rest of the package's tests.
- `GetDailyUsage` sums every row of the user for the personal total, org rows included (the safe direction;
  the comments now say so): a personal budget also counts org spend.
