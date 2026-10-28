# 41. A provider call fails as what it is, and the breaker and the telemetry can tell

Date: 2026-10-26. Status: accepted, implemented (AI roadmap step 6).

## Context

Retry, the circuit breaker, the chat's idle watchdog and the user's error bubble all act on how a provider failure is classified. Several failures were classified as something else, or not at all:

- The provider transports bounded neither the dial nor the TLS handshake, so a gateway that accepted TCP and stalled the handshake held a call until the caller's own deadline.
- A response-header timeout matches `context.DeadlineExceeded`, which retry and the breaker read as the caller's deadline. An upstream that accepted connections and never answered was never retried and never opened the circuit. Conversely, a half-open probe ended by its caller's own deadline restarted the cooldown for everyone.
- The fixed-host transport's 30 s header floor also applied to the OpenAI family's non-streaming fallback, which can think for minutes before its first header; the call aborted while its tokens were billed.
- A locally synthesised keepalive resolved a half-open stream probe before the upstream answered, and a Chat or Embed probe refused every other caller for its whole duration.
- Gemini classified errors body-first, so a bodyless or HTML 429/5xx, every 401/403 and every embedding failure escaped retry, the breaker and the typed codes. A thinking model's empty MAX_TOKENS stop failed the connection test, thinking models got no reasoning floor, and the effort setting never reached Gemini. Its SSE parser fed no keepalives, dropped the usage of a filtered finish, could not classify an error event and never carried the served model.
- An OpenAI-family 403 read as an invalid key, and a quota-exhausted 429 as a rate limit to wait out.
- A retried Responses attempt inherited the failed attempt's open silent phase; the Azure Responses reroute nested a second token retry inside the turn's own and replayed the whole turn twice.
- The breaker's metric and log line named only the provider; the latency histogram stopped at 10 s; the provider spans ignored error chunks, the served model and their status.

## Decision

1. **The transports bound the connection.** The shared transport dials with a 30 s timeout and bounds the TLS handshake at 15 s; every clone and every guard inherits or replaces them with the same values.
2. **A transport timeout is an outage.** Provider calls go through `doProviderRequest`: a timeout the transport hit while the caller's context is live becomes `ErrProviderDown` (`upstreamTimeoutError`, which does not unwrap to `DeadlineExceeded`), so the breaker counts it and retry retries it — except on a long-wait client, where the upstream may already have produced and billed the answer: that timeout is never replayed. The caller's own cancel or deadline is returned unchanged.
3. **Two header floors on the fixed hosts.** Streaming requests keep the 30 s floor; non-streaming chat requests of the OpenAI family use a clone with the 5-minute floor (`fixedHostClientFor(false)`), with the same egress guard and redirect policy, and a connection pool of its own (`PAD_AI_MAX_CONNS_PER_HOST` bounds each pool). Claude and Gemini keep one client: their only non-streaming caller is the 15 s connection test.
4. **The probe is resolved by the upstream.** A half-open probe ended by its caller's own context (`ctx.Err() != nil`) releases the probe without restarting the cooldown; an upstream failure restarts it. A `LocalKeepalive` never resolves a stream probe. The exclusive probe window lasts at most `cbProbeWindow` (20 s); past it one more caller is admitted as a probe and the window starts again.
5. **Gemini classifies status first.** `classifyGeminiError` serves Chat, Stream, embeddings and the stream's error events:
   - a 403/`PERMISSION_DENIED` is the provider refusing access (`ErrProviderAccessDenied`), unless it names the API key;
   - a 401/`UNAUTHENTICATED`, or a 400 or 403 naming the API key (invalid, leaked, restricted), is `ErrApiKeyInvalid`;
   - a 429/`RESOURCE_EXHAUSTED` is a rate limit with the header's or `RetryInfo`'s delay, unless its `QuotaFailure` names a per-day quota, which is `ErrInsufficientBalance` with Google's message;
   - a 5xx/`UNAVAILABLE`/`INTERNAL` is `ErrProviderDown`.

   An empty `MAX_TOKENS` answer is `ErrOutputLimit`, and a filtered finish `ErrContentFiltered`. The 2.5 and 3 families and the "-thinking" ids get the reasoning floor. The effort maps to `thinkingConfig`: a level on Gemini 3, a budget of 1024/8192/24576 on 2.5 cut to leave 4096 of the output cap for the answer; Flash-Lite keeps thinking off on "low", and image models get none. The spec declares `CapReasoning`.
6. **The Gemini stream says what happened.** An event that delivers nothing emits a keepalive; a filtered finish carries the reported usage; an error event becomes a classified error chunk with the usage so far and the stream ends with nil; `modelVersion` rides on every chunk as `ServedModel`.
7. **The OpenAI family tells access and quota apart.** A 403 without a provider hook is `ErrProviderAccessDenied` (also a permission refusal and an auth failure, so retry and the breaker do not change) with vendor-neutral copy that keeps the vendor's reason, and the shared-credential variant points at the organisation's admin. OpenAI, xAI, GitHub Models and Copilot map a 429 naming `insufficient_quota` or the current quota to `ErrInsufficientBalance`; a `rate_limit_exceeded` stays a rate limit whatever page its text links.
8. **A retry and a token replay are each once.** The retry wrapper closes a failed attempt's open `SilentStep`s before the next attempt. `withTokenRetry` is re-entrant: inside another it runs once on the outer recorder, the outer owns the one replay, and a replay after a reroute goes straight to the Responses route.
9. **The breaker and the calls can be observed.** Transitions are labelled by provider, a bounded model label (the catalogue's id, `deployment`, `uncatalogued` or `none`) and an 8-character resource hash, and the log line names the same. `pad_ai_circuit_breaker_open{provider}` is counted from the registry at scrape time: half-open circuits and open ones within their cooldown, so an abandoned circuit stops counting. `ErrCircuitOpen` comes as `circuitOpenError` with the model and the remaining cooldown, which the chat copy names. `pad_ai_call_duration_seconds{provider,kind}` and `pad_ai_stream_first_token_seconds{provider}` use buckets from 0.1 s to 600 s; the old `ai_request_duration_seconds` stays as it was. A provider span records the status, `ai.error_class`, the served model, cache tokens and the finish reason, for an error chunk and for a held-back error's usage; a caller's cancel keeps its class without an Error status. Models and CountTokens are spanned too.

## Consequences

- An overloaded upstream now opens its circuit; a slow caller's deadline no longer extends an outage for everyone.
- `ai_circuit_breaker_transitions_total` gained two labels: a dashboard that summed by provider still works, one that matched on the exact label set must add them.
- A user behind a region block or on an exhausted OpenAI quota is told so instead of being sent to re-enter a working key or to wait.
- A Gemini thinking turn is budgeted like the other reasoning families, and the Settings effort control now applies to Gemini.
- Gemini 3's tool loop still needs the function-call thought signatures sent back; the parts carry none, so a second tool iteration on a `gemini-3*` model can fail. Left for the step that opens the Gemini tool path.
