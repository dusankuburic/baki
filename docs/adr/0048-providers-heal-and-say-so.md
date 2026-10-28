# 48. Providers keep what they learned, say how they healed, and do not let a cancellation look like an outage

Date: 2026-10-28. Status: accepted, implemented (AI roadmap open-item plan, phase 3: provider correctness). Builds on ADR 0041 (provider calls fail as what they are) and ADR 0045 (wire goldens).

## Context

Several provider-layer defects:

- **Gemini thought signatures:** they were dropped, so a thinking model's tool loop replayed calls without them. Gemini 3 refuses those calls.
- **Smaller parser gaps (found by the wire fixtures):** Gemini cache tokens, the Responses served model on silent chunks, vLLM's integer error codes on the non-streaming path, and `data:` lines without a space.
- **Context-window rejections:** a rejection that stated the real window always failed the turn.
- **Foundry project listings:**
  - a cut-short refresh replaced the last complete listing;
  - partial pages were thrown away under one 10 s budget;
  - chat turns never triggered a refresh, so a cold replica sized Foundry turns from bare entries.
- **Process memos:**
  - they were wiped wholesale at their cap or evicted at random;
  - the repair memo forgot what it had learned when a later request did not send the same parameter.
- **Metrics:** provider self-healing was visible only in logs, and `ai_request_errors_total` counted every user Stop as a provider error.
- **Copilot:** listing errors carried up to 10 MiB of HTML.
- **The provider catalogue:** it read the keystore once per provider, under a context the request could not cancel.

## Decision

1. **Thought signatures round-trip.** A Gemini turn records each function call's `thoughtSignature`, its provider-supplied call id and the text signature in its replay record (`ProviderItems`, owner `gemini`). Replaying the turn's own record puts them back on the matching parts; foreign history carries none. The tool loop keeps the current turn's records through truncation. A missing-signature refusal, as an HTTP 400 or as a finish reason (both modelled on documentation), is `ErrThoughtSignatureMissing`, which is not retried.
2. **The smaller parser gaps are closed.**
   - **Gemini:** `cachedContentTokenCount` becomes cache reads.
   - **Responses:** every chunk carries the served model once known.
   - **Errors:** `decodeOpenAIError` decodes the error code raw.
   - **SSE:** every parser accepts `data:` and `event:` with or without the space.
3. **A stated window is retried once in the turn.** A `ContextLimitError` that states a smaller window, arriving before anything reached the client, re-trims the turn to that window and retries once, on all three call paths. Learning stays alias-only.
4. **Listings keep their best version.**
   - A cut-short refresh never replaces a complete listing within its stale grace.
   - Partial deployment pages are kept and marked incomplete, and the deployments and agents listings get their own budgets.
   - The caller's deadline reaches the detached flight, and a project's `Retry-After` is honoured up to a cap.
   - A chat turn whose resource listing is due triggers one background refresh through the existing refresher, without ever waiting on the project.
5. **Memos are declared caches.** The provider memos are replica-local `cache.Declare` specs with LRU caps and TTLs, keyed by tenant where they hold per-tenant facts. `rememberRepairs` merges new repairs into what was already learned. Their evictions appear in `pad_cache_evictions_total`.
6. **Healing and failures are counted.**
   - New families: `pad_ai_param_repairs_total{provider,param}`, `pad_ai_route_fallbacks_total{provider,kind}` and `pad_ai_entra_token_events_total{kind}`.
   - `ai_request_errors_total` gains a closed `class` label, and cancellations go to `pad_ai_requests_cancelled_total{provider,reason}`.
   - The reason travels on the cancellation itself: the chat stream's context is built with `context.WithCancelCause`, and its cancel code maps to client, idle, subscriber_gone, shutdown or deadline.
7. **Bounded errors and request-scoped key reads.**
   - Copilot listing errors keep the status, the request id and one line of the body.
   - The catalogue and the provider listing read each scope's keys in one query, through explicitly wired `KeyReads`. The reads stop when the request is cancelled.

## Consequences

- A Gemini 3 tool loop works on the provider's own history. History handed over from another model may still be refused; that refusal is now named.
- Dashboards and alerts built on `ai_request_errors_total` must aggregate over the new `class` label (`sum by (provider)`). Their past rates included user stops.
- A throttled or slow project keeps its last complete listing for at most the stale grace, then reads as not complete.
- The memos still live per replica. Sharing them across replicas belongs with step 9.
