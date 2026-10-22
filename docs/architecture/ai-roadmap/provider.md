# AI roadmap: Provider layer

Items of category `provider` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-provider-2

**Keyring unavailability is masked as 'no key' and 'deleted'; keys/save and local/config answer 500 where the Azure routes answer 503** · tier 1

**Problem.** The desktop keyring store hides an unreachable secret service:
- `Get` returns `ErrSecretNotFound` (`keyring.go:55-63`).
- `Has` returns `false, nil` (66-75).
- `Delete` returns `nil` (78-83). A removal that did not happen is reported as done, and the form flips to "not configured".

So while the keychain is down, a configured key reads as absent. `GetLocalConfig` (`providers.go:1195-1198`) also treats any `Get` error as "unconfigured", masking the outage the same way.

On the save side, the keyring store's `Save` returns `ErrSecretStorageUnavailable` (`keyring.go:45-47`), but nothing maps it to a code:
- `/api/keys/save` renders any `SaveApiKey` error as a 500 (`handlers_system.go:284-286`).
- `renderLocalConfigError` maps anything but `*ai.LocalConfigError` to 500 (`handlers_providers.go:341-345`).
- `render.Error` replaces any >=500 message with "internal server error" and code `INTERNAL_ERROR`.

The client therefore gets an opaque `INTERNAL_ERROR` instead of a code it can branch on. Meanwhile the Azure registry maps `storage.ErrSecretStorageUnavailable` to 503 `AzureCodeSecretStorageUnavailable` (`azure_models.go:118-119`), so the client cannot branch on the same condition consistently.

**Evidence.**
- `internal/storage/keyring.go:55` — Get: an unavailable backend returns `ErrSecretNotFound` (55-63).
- `internal/storage/keyring.go:66` — Has: an unavailable backend returns `false, nil` (66-75).
- `internal/storage/keyring.go:78` — Delete: an unavailable backend returns `nil`, with the comment "don't surface as an error" (78-83). Save returns `ErrSecretStorageUnavailable` (45-47).
- `internal/api/handlers_system.go:284` — a `SaveApiKey` error is rendered with `render.Error` and 500.
- `internal/api/handlers_providers.go:341` — `renderLocalConfigError` turns anything but `*ai.LocalConfigError` into a 500.
- `internal/api/render/render.go:183` — any >=500 message becomes "internal server error" (176-185); a 500 gets the code `INTERNAL_ERROR` (82).
- `internal/service/providers.go:1195` — `GetLocalConfig` treats any `Get` error as unconfigured (1195-1198).
- `internal/service/azure_models.go:118` — Azure maps `ErrSecretStorageUnavailable` to 503 `AzureCodeSecretStorageUnavailable`.

**Change.**
- Keep the documented headless behaviour: `Get` and `Has` stay "not found" when the backend is permanently absent.
- Make `Delete` return `ErrSecretStorageUnavailable` only for a backend error other than not-found, so a removal that did not happen is no longer reported as done.
- Map `storage.ErrSecretStorageUnavailable` to 503 `SECRET_STORAGE_UNAVAILABLE` in one place, either `render.errorStatus`/`statusCode` or an `apperr` kind returned from `SystemService.SaveApiKey`. Then `/api/keys/*`, `/api/providers/local/config` and the Azure routes share it.

**Touches.** `internal/storage/keyring.go`, `internal/api/handlers_system.go`, `internal/api/handlers_providers.go`, `internal/service/providers.go`, `internal/service/system.go`, `frontend/src/components/settings/providers/CredentialForm.tsx`

**Done when.**
- With a keyring backend returning an error other than `ErrNotFound`, `POST /api/keys/delete` answers 503 `SECRET_STORAGE_UNAVAILABLE`, and the UI keeps showing the key as configured.
- `POST /api/keys/save` and `POST /api/providers/local/config` answer 503 `SECRET_STORAGE_UNAVAILABLE`, not 500, on `storage.ErrSecretStorageUnavailable`. A test asserts the same code on the Azure create route.

### R-provider-3

**A hung upstream (connection accepted, headers never sent) is invisible to retry and to the closed breaker, while a caller's own deadline during a probe restarts the cooldown** · tier 1

**Problem.** Go's `http.Transport` reports a `ResponseHeaderTimeout` as a `timeoutError` whose `Is()` matches `context.DeadlineExceeded` (`transport.go:2775-2777`). `retry.isTransportError` excludes `DeadlineExceeded` (`retry.go:27`). So a header timeout is neither retried nor wrapped as `ErrProviderDown` (`asProviderDown`, 40-45). The breaker's closed-circuit path ignores it because it is not retryable (`circuitbreaker.go:324-326`).

An overloaded gateway that accepts connections but never answers therefore never opens the circuit. Every user call waits the full header timeout (30 s on the shared transport, 5 min on Azure and local) and then fails with a generic error. `azure_foundry_config.go:493-495` already documents that a header timeout satisfies `context.DeadlineExceeded`, which the retry wrapper excludes.

The inverse is also wrong. `probeSaysNothing` lists `context.Canceled` but not `DeadlineExceeded` (`circuitbreaker.go:272-277`). So a half-open probe whose caller had a short deadline restarts the cooldown for everyone on that circuit (311-320), and `circuitbreaker_probe_test.go:129` pins that behaviour. Two cases:
- The 800 ms RAG embed runs on the provider's embedding circuit (`stateFor("")`, `circuitbreaker.go:208-216`), so it restarts the cooldown for every caller of that embedding circuit.
- A short-deadline chat caller, such as a 15 s connection test, affects its own `provider|model` circuit.

**Evidence.**
- `C:/Program Files/Go/src/net/http/transport.go:2775` — `timeoutError.Is(err)` returns `err == context.DeadlineExceeded`. `errTimeout` at 2777 is "net/http: timeout awaiting response headers".
- `internal/ai/retry.go:27` — `isTransportError` returns false for `context.DeadlineExceeded`, so `isRetryable` (49-51) is false and `asProviderDown` (40-45) leaves the error unwrapped.
- `internal/ai/circuitbreaker.go:324` — `recordSt` on a closed circuit returns without counting when `!isRetryable(err)`.
- `internal/ai/circuitbreaker.go:272` — `probeSaysNothing` lists `context.Canceled` but not `DeadlineExceeded`; 311-320 restart the cooldown for every other half-open failure.
- `internal/ai/circuitbreaker.go:208` — `Embed` uses the embed circuit, `stateFor("")` (208-216).
- `internal/ai/circuitbreaker_probe_test.go:129` — asserts that `probeSaysNothing(context.DeadlineExceeded)` is false, pinning the cooldown restart for a caller's own deadline.
- `internal/ai/azure_foundry_config.go:493` — comment: the shared floor aborted billed calls, and "a header timeout satisfies context.DeadlineExceeded, which the retry wrapper excludes".

**Change.**
- Distinguish the transport's own timeout from the caller's deadline at the call site. Add a shared helper around `client.Do`, used by `openaiBase.post`, the Claude and Gemini posts, and Azure `postResponses`. When the error matches `DeadlineExceeded` (or is a `net.Error` with `Timeout()`) while `ctx.Err() == nil`, the helper wraps it as `ErrProviderDown` ("upstream did not answer within the header timeout"). Retry and the breaker then treat it as the outage it is.
- In the breaker, pass `ctx` into `recordSt`. A half-open probe that fails while `ctx.Err() != nil` releases the probe without restarting the cooldown.
- Keep a bare `DeadlineExceeded` with `ctx.Err() == nil` (the upstream's timeout) restarting the cooldown.
- Update the pinned assertion at `circuitbreaker_probe_test.go:129`; this is an intended behaviour change.

**Touches.** `internal/ai/openai_compat.go`, `internal/ai/claude.go`, `internal/ai/gemini.go`, `internal/ai/responses.go`, `internal/ai/retry.go`, `internal/ai/circuitbreaker.go`, `internal/ai/circuitbreaker_probe_test.go`, `internal/ai/retry_transport_test.go`

**Done when.**
- An httptest server that sleeps past a scaled `ResponseHeaderTimeout` makes `Chat` return an error with `errors.Is(err, ErrProviderDown)`, and the retry wrapper makes a second attempt.
- A threshold number of consecutive header timeouts on one `provider|model` opens the circuit (`ErrCircuitOpen` on the next call).
- A half-open probe run under a 1 ms caller deadline returns the deadline error, and the very next caller is admitted as the new probe (no cooldown restart). A probe that hits the upstream header timeout restarts the cooldown.

### R-provider-4

**No TLS handshake timeout on any provider transport, and no dial timeout on the base shared client** · tier 1

**Problem.** `sharedHTTPClient`'s transport literal (`httpclient.go:34-47`) sets neither `TLSHandshakeTimeout` nor `DialContext`. Go arms the handshake timer only when `TLSHandshakeTimeout != 0` (`transport.go:1727`). When `DialContext` is nil, it dials through a zero `net.Dialer` (1303, 1320), so no dial timeout is configured in Go; the OS TCP connect timeout still applies.

The guarded clones installed at boot add a 30 s dialer (`httpclient.go:97`, `azure_foundry_config.go:545`). But `Clone()` copies the zero `TLSHandshakeTimeout`, and `GuardAllowListedTransport` replaces only `DialContext` (`egress.go:281-286`); a grep for `TLSHandshakeTimeout` over `internal/` finds nothing. `localModelsClient` (`local_provider.go:296-298`) and the unconfigured/default Azure client (`azure_foundry_config.go:517-518`, 548) use the base transport with no dialer at all.

Consider a gateway that completes TCP but stalls the TLS handshake. While a provider call has produced no output yet, `awaitingOutput` is set, and `idleLimitFor` raises the watchdog to `hostedStepIdleTimeout`. So the stall holds a chat stream for about 5 minutes, not until the 90 s `streamIdleTimeout`, and holds a non-streaming `Chat` until the caller's deadline. The resulting error is the caller's own deadline or cancel, so [R-provider-3](provider.md#r-provider-3)'s reclassification, which requires `ctx.Err() == nil`, does not apply to this case.

**Evidence.**
- `internal/ai/httpclient.go:34` — the `sharedHTTPClient` transport literal (34-47) sets Proxy, idle and connection limits, `ResponseHeaderTimeout` and `ForceAttemptHTTP2`, but no `TLSHandshakeTimeout` and no `DialContext`.
- `C:/Program Files/Go/src/net/http/transport.go:1727` — `addTLS` arms the handshake timer only `if d := pconn.t.TLSHandshakeTimeout; d != 0`. With no `DialContext`, the transport dials through `zeroDialer` (1303, 1320), which has no `Timeout`.
- `internal/egress/egress.go:281` — `GuardAllowListedTransport` sets `DialTLSContext = nil` and `DialContext` only; `TLSHandshakeTimeout` is untouched.
- `internal/ai/httpclient.go:97` — the guarded fixed-host clone gets a 30 s dialer. `azure_foundry_config.go:545` does too, but only in `BlockInternal` mode.
- `internal/ai/local_provider.go:296` — `localModelsClient` uses `azureTransport(sharedTransportBase())` with no dialer; so do `azure_foundry_config.go:517-518` and 548.
- `internal/service/chat_resume.go:50` — `streamIdleTimeout` is 90 s. While `awaitingOutput` is set (`chat_stream_control.go:139-142`, 246), `idleLimitFor` raises the limit to `hostedStepIdleTimeout` = 5 min (`chat_resume.go:55`, 196-209).

**Change.**
- Set `TLSHandshakeTimeout` (e.g. 15 s) and a default `DialContext` with a 30 s connect timeout on `sharedHTTPClient`'s transport, so every clone inherits them. The guard replaces `DialContext` with a dialer of the same timeout.
- Add a test that constructs the shared, fixed-host, Azure and local clients and asserts both fields.
- Add an httptest TLS listener that never completes the handshake, and assert that the request fails within the bound.

**Touches.** `internal/ai/httpclient.go`, `internal/ai/azure_foundry_config.go`, `internal/ai/local_provider.go`, `internal/ai/httpclient_test.go` (new)

**Done when.**
- `sharedTransportBase().TLSHandshakeTimeout > 0`, and every derived client reports the same non-zero value: `fixedHostClient()`, `azureHTTPClient()`, `localModelsClient()` and `authHTTPClient`.
- A TCP listener that accepts and never speaks TLS makes a provider `Chat` fail within `TLSHandshakeTimeout` plus dial time, not at the caller's 10-minute cap.
- A listener that never accepts (simulated with an unroutable address under a long caller ctx) fails within the dialer timeout on the base client too.

### R-provider-5

**The 30 s header floor on the shared fixed-host transport contradicts the non-streaming fallback every OpenAI-compatible provider can take** · tier 1

**Problem.** Every fixed-host provider uses `fixedHostClient()`: Claude (`claude.go:46`), Gemini (`gemini.go:36`), OpenAI (`openai.go:22`), xAI (`xai.go:18`), GLM (`glm.go:19`), GitHub Models (`github_models.go:37`) and Copilot (`copilot.go:67`, 87). It is a clone of the shared transport with `ResponseHeaderTimeout` 30 s (`httpclient.go:42-44`). Only Azure and local raise it to 5 minutes (`azure_foundry_config.go:496`, `local_provider.go:296-298`), and `azure_foundry_config_test.go:498-500` requires the shared client to keep 30 s.

Yet `openaiBase.streamInner` routes any stream-refusing deployment to `streamViaChat`, one non-streaming request (`openai_compat.go:742-743`, 751-756). The code's own comment says a reasoning model can spend minutes before the first header (777-778). On the `openaiBase` providers (OpenAI, xAI, GLM, GitHub Models and Copilot), such a call aborts at 30 s with the tokens already billed upstream. This is the exact failure the Azure comment at `azure_foundry_config.go:493-495` describes, and because of [R-provider-3](provider.md#r-provider-3) the abort is not even classified.

Claude and Gemini share the client but have no non-streaming fallback. Their only non-streaming `Chat` caller is the connection test, which runs under its own 15 s context (`service/providers.go:948`), so the 30 s floor never fires there.

**Evidence.**
- `internal/ai/httpclient.go:42` — the comment "B1.8: a header-only floor" and `ResponseHeaderTimeout` 30 s on the shared transport.
- `internal/ai/claude.go:46` — `fixedHostClient()` at construction. The same at `gemini.go:36`, `openai.go:22`, `xai.go:18`, `glm.go:19`, `github_models.go:37` and `copilot.go:67`/87.
- `internal/ai/azure_foundry_config.go:496` — `azureResponseHeaderTimeout` = 5 min, raised because the shared floor aborted billed answers (493-495). `local_provider.go:296-298` reuses `azureTransport`.
- `internal/ai/azure_foundry_config_test.go:498` — asserts the shared client keeps its 30 s `ResponseHeaderTimeout`.
- `internal/ai/openai_compat.go:742` — `streamInner` routes a stream-refusing model to `streamViaChat` (742-743, 751-756) on every `openaiBase` provider.
- `internal/ai/openai_compat.go:777` — the comment "a reasoning model can spend minutes before the first header" (777-778).
- `internal/service/providers.go:948` — the connection test runs under its own 15 s context.

**Change.**
- Limit the change to `openaiBase`. Add a long-header (5-minute) guarded clone of the fixed-host client for non-streaming requests (`chat` and `streamViaChat`), selected by the `stream` flag in `post()`. `ConfigureFixedHostClient` installs it beside the short one, which keeps 30 s for streaming requests. A per-request pre-header deadline through `httptrace.GotFirstResponseByte` is an alternative.
- Leave the Claude and Gemini clients alone: their only non-streaming production caller is the 16-token connection test.

**Touches.** `internal/ai/httpclient.go`, `internal/ai/openai_compat.go`, `internal/ai/azure_foundry_config_test.go`, `internal/ai/openai_compat_foundry_test.go`

**Done when.**
- A stream-refusing stub on an OpenAI-family provider that sends headers after twice the (scaled) streaming header floor completes successfully through `Stream` → `streamViaChat`.
- A streaming request whose headers never arrive still fails at the short floor, not the long one.
- The fixed-host clients used for non-streaming calls carry `NoRedirects` and the egress guard exactly like the streaming one (`ConfigureFixedHostClient` installs both).

### R-provider-6

**Half-open probe resolution is wrong in both directions: a Chat/Embed probe locks everyone out for its whole duration, and a locally synthesised keepalive resolves a Stream probe before the upstream answered** · tier 1

**Problem.** While a circuit is half-open, `checkSt` turns away every caller except the single probe until that probe resolves (`circuitbreaker.go:291-294`). `Stream` resolves the probe on the first chunk where `c.Error == nil` (`circuitbreaker.go:239-245`). But `streamViaChat` emits `Chunk{LocalKeepalive:true}` every 20 s while its non-streaming request is still pending (`openai_compat.go:779`, `793-801`), and the breaker does not exclude these chunks. That is despite `provider.go:168-169` saying a `LocalKeepalive` "proves nothing about the request", and despite the audited wrapper already excluding it from `sawAny` (`audited.go:209`). A half-open probe against a deployment that refuses streaming therefore closes the circuit after 20 s, before the upstream has answered, and every waiting caller is released into the same outage. The other direction: `Chat` and `Embed` resolve the probe only in `recordSt`, after the inner call returns (`circuitbreaker.go:197-217`). After a transient outage, the first non-streaming caller holds the gate for its whole duration, and every other caller on that key sees "temporarily unavailable after repeated failures". Chat goes through the chain only for the connection test (`providers.go:973`, capped at 15 s at `providers.go:948`). Embed probes use the provider-wide key `stateFor("")` (`circuitbreaker.go:209`), not provider|model. This second lockout is bounded: at most one 15 s connection test or one knowledge-base embed batch.

**Evidence.**
- `internal/ai/circuitbreaker.go:281` — `checkSt`: a half-open circuit rejects every caller (`291-294`) until the probe resolves
- `internal/ai/circuitbreaker.go:239` — `Stream` resolves the probe on the first chunk with `c.Error == nil` (`242-245`), `LocalKeepalive` included
- `internal/ai/openai_compat.go:800` — `streamViaChat` emits `Chunk{LocalKeepalive:true}` every `streamViaChatKeepalive` (20 s, line 779) while `b.chat` is still running
- `internal/ai/provider.go:168` — `LocalKeepalive` "feeds the idle watchdog but proves nothing about the request"
- `internal/ai/audited.go:209` — the audited wrapper excludes `LocalKeepalive` from `sawAny`; the breaker has no such exclusion
- `internal/ai/circuitbreaker.go:197` — `Chat` (`197-206`) and `Embed` (`208-217`) resolve the probe only through `recordSt` after the inner call returns; `Embed` uses `stateFor("")` (`209`)
- `internal/service/providers.go:973` — the connection test's `provider.Chat`, under the 15 s timeout set at `948`
- `internal/service/chat_errors.go:248` — `ErrCircuitOpen` reaches the user as "temporarily unavailable after repeated failures"

**Change.** (a) First, a small fix that is clearly correct: in `CircuitBreakerProvider.Stream`, resolve the probe only on chunks that carry evidence from the upstream, never on `LocalKeepalive`. Text, Reasoning, Done, ToolCalls, ToolEvent, ServerStep, SilentStep, usage, an upstream keepalive, or a Notice emitted after a 200 all qualify. (b) Lower priority, because the Chat/Embed lockout is bounded: put a time limit on the exclusive half-open window in `checkSt` and admit further callers once it has passed. Prefer this over a `probeAccepted` func carried in the context (set by the breaker, called on a 2xx status). That alternative would have to be threaded through the `openai_compat`, `responses`, `claude` and `gemini` HTTP helpers.

**Touches.** `internal/ai/circuitbreaker.go`, `internal/ai/circuitbreaker_probe_test.go`; only for the `probeAccepted` variant: `internal/ai/openai_compat.go`, `internal/ai/responses.go`, `internal/ai/claude.go`, `internal/ai/gemini.go`

**Done when.**
- Half-open circuit; the inner `Stream` emits `Chunk{LocalKeepalive:true}` then `Chunk{Error: ErrProviderDown}`: the circuit is open afterwards with `lastFailure` restarted, and no concurrent caller was admitted in between.
- Half-open circuit; the inner `Stream` emits an empty upstream keepalive `Chunk{}` then text: the circuit closes on the keepalive (unchanged behaviour for chunks the upstream sent).
- A Chat probe is in flight that has received 200 headers but no body yet. A concurrent Chat on the same key is admitted once the bounded window has elapsed instead of getting `ErrCircuitOpen` (under the `probeAccepted` variant, as soon as the headers arrive).

### R-provider-10

**Gemini classifies errors body-first and leaves embeddings untyped: bodyless/HTML 429 and 5xx, every 401/403 and every embedding failure escape retry, the breaker and the typed HTTP codes** · tier 1

**Problem.** Gemini maps HTTP status to the shared sentinels only in some paths:
- `GeminiProvider.Chat` maps a status only inside `if err := json.Unmarshal(respBody, &apiErr); err == nil` (`gemini.go:360-382`), with a generic error at 382.
- `Stream` additionally requires a non-empty `error.message` (468-471) and never maps 401/403 to `ErrApiKeyInvalid`. Only a bodyless >=500 is mapped (488-491).
- `embedBatch` turns every non-200 into `gemini embeddings error (status N)` (127-128).

`RetryingProvider` retries only `ErrRateLimited`, `ErrProviderDown` and transport errors (`retry.go:49-51`), and its `Embed` returns anything else at once (184). A closed circuit counts only retryable errors (`circuitbreaker.go:324`). This affects a gateway 429 or 502 with an empty or HTML body, a 403 `PERMISSION_DENIED`, and a 429 or 503 on `batchEmbedContents`. Such an error:
- is never retried and never opens the breaker;
- shows the user an unclassified message instead of `PROVIDER_RATE_LIMITED` or `PROVIDER_AUTH`;
- on the knowledge-base routes, falls through the typed ladder (`org_knowledge.go:168-178`) to the `rag.ErrEmbeddingFailed` catch-all: a 400 `EMBEDDING_PROVIDER_REJECTED` with no `Retry-After` (179-181).

The OpenAI-compatible family classifies by status first precisely so that a gateway 429 or 502 with an empty or HTML body still counts (`openai_compat.go:662-664`). The `Chat` half matters mainly for the connection test, the only `Chat` caller through the chain (`service/providers.go:973`). `Stream` and `Embed` carry the user-facing impact.

**Evidence.**
- `internal/ai/gemini.go:360` — Chat: the 429, 5xx and API-key mapping sits inside the `json.Unmarshal` success branch; line 382 returns a generic error otherwise.
- `internal/ai/gemini.go:468` — Stream: the mapping requires `apiErr.Error.Message != ""` (471). 401/403 are never mapped; only a bodyless >=500 is (488-491).
- `internal/ai/gemini.go:127` — embedBatch: every non-200 becomes `gemini embeddings error (status N)`. A 429 is not a `RateLimitError`, and a 503 is not `ErrProviderDown`.
- `internal/ai/retry.go:49` — `isRetryable` accepts only `ErrRateLimited`, `ErrProviderDown` or a transport error; `Embed` (184) returns anything else at once.
- `internal/api/org_knowledge.go:168` — the 429 and 503 codes need the typed sentinels (168-178). The catch-all at 179-181 answers 400 `EMBEDDING_PROVIDER_REJECTED`.
- `internal/ai/openai_compat.go:662` — for contrast, `handleChatError` classifies by status first and body second, for exactly this reason.
- `internal/service/providers.go:973` — the connection test is the only `Chat` caller through the chain.

**Change.** Add one `classifyGeminiError(resp, body)` used by `Chat`, `Stream` and `embedBatch`, which checks in this order:
1. 401/403, and a 400 mentioning "API key", map to `ErrApiKeyInvalid`. Google's `PERMISSION_DENIED` status maps to `permissionDeniedError`.
2. 429 or status `RESOURCE_EXHAUSTED` maps to `rateLimitErr(resp)`, with the retry-after taken from the header or the `RetryInfo` detail.
3. >=500 or `UNAVAILABLE` maps to `ErrProviderDown` with the message.
4. Then `detectContextLimitError` and `detectToolsUnsupportedError` run.

A bodyless or HTML answer keeps the status-derived sentinel. Extend `internal/ai/gemini_errors_test.go` to cover `Chat`, `Stream` and `Embed` through `RetryingProvider` and `renderKnowledgeEmbeddingError`.

**Touches.** `internal/ai/gemini.go`, `internal/ai/gemini_errors_test.go`, `internal/ai/embeddings_test.go`, `internal/api/handlers_org_knowledge_test.go`

**Done when.**
- An httptest server answering 429 with an empty body makes `Chat`, `Stream` and `Embed` return an error for which `errors.Is(err, ErrRateLimited)` is true, and `RetryingProvider` makes a second attempt. A Gemini `batchEmbedContents` 429 with `Retry-After: 1`, once retries are exhausted, renders 429 `EMBEDDING_RATE_LIMITED` with the hint.
- A 502 with an HTML body yields `errors.Is(err, ErrProviderDown)` on `Chat` and `Stream`. A Gemini 503 on `Embed` renders 503 `EMBEDDING_PROVIDER_UNAVAILABLE` and increments the breaker's failure count.
- A 403 with `{"error":{"status":"PERMISSION_DENIED","message":"…"}}` yields an error matching both `ErrProviderPermissionDenied` and `ErrApiKeyInvalid`, and is not retried. A 400 mentioning "API key" on `Embed` renders `EMBEDDING_AUTH_FAILED`.

### R-provider-11

**Gemini thinking models get no reasoning floor, no thinkingConfig and no reasoning-effort lever, and an empty MAX_TOKENS stop on the non-streaming path is a generic error** · tier 1

**Problem.** Gemini 2.5-series models think by default, and baki bills their thoughts as output: `ThoughtsTokenCount` is folded into `tokensOut` at `stream.go:873`. But `reasoningModelRe` has no Gemini pattern (`reasoning_floor.go:19`), so `ReasoningOutputFloor` returns 0. The turn goes out with `MaxOutputTokens` `orDefault(req.MaxTokens, 4096)` (`gemini.go:329` for Chat, 439 for Stream), while the Claude and OpenAI reasoning families get the floor. There is also no lever for thinking depth:
- The Gemini request body never sets a `thinkingConfig`; a grep for `thinkingConfig`/`thinkingBudget` over `internal/ai` finds nothing.
- The spec lacks `CapReasoning` (`spec_gemini.go:8`).
- `ReasoningEffort` is serialised only for Claude, chat completions and Responses (`claude.go:436`, `openai_compat.go:145`, `responses.go:407`).

A Gemini turn that spends its budget thinking ends with `MAX_TOKENS` and no answer. On the stream path, `isOutputLimitFinish` knows `"MAX_TOKENS"` (`chat_errors.go:76-78`), so the turn becomes `PROVIDER_OUTPUT_LIMIT`. `Chat`, however, returns the generic "gemini returned no content" whatever the `finishReason` (`gemini.go:415-416`); Claude maps the same stop to `ErrOutputLimit` (`claude.go:688-690`). The non-streaming path is reached only by the connection test (`service/providers.go:973`). That test sends `MaxTokens` 16 on the default `gemini-2.5-pro` (`spec_gemini.go:14`) and treats `ErrOutputLimit` as success (`providers.go:997-1000`), so it fails on a thinking model. The user also has no lever to lower the thinking depth.

**Evidence.**
- `internal/ai/reasoning_floor.go:19` — `reasoningModelRe` lists the o-series, gpt-5/6, deepseek, grok, phi-4 and claude families, with no gemini pattern. `ReasoningOutputFloor` (26-53) returns 0 for any gemini id.
- `internal/ai/gemini.go:329` — Chat's `GenerationConfig` sets `MaxOutputTokens` `orDefault(req.MaxTokens, 4096)` with no `thinkingConfig`; Stream does the same at 437-440.
- `internal/ai/gemini.go:415` — `text == "" && no tool calls` returns `errors.New("gemini returned no content")` regardless of `FinishReason`.
- `internal/ai/spec_gemini.go:8` — Caps are `CapChat | CapStream | CapEmbeddings | CapTools | CapOrgShared`, with no `CapReasoning`. `DefaultModel` is `gemini-2.5-pro` at :14.
- `internal/ai/stream.go:873` — `tokensOut = CandidatesTokenCount + ThoughtsTokenCount`, so thinking is billed output on this route.
- `internal/service/providers.go:973` — the connection test calls `Chat` with `MaxTokens` 16 (973-978).
- `internal/service/providers.go:997` — the connection test treats `ErrOutputLimit` as success; a generic error fails it.

**Change.**
- Return `fmt.Errorf("%w (finishReason MAX_TOKENS)", ErrOutputLimit)` from `Chat` when the candidate has no parts and `finishReason` is `MAX_TOKENS`. This is the most urgent part, because it alone decides whether the connection test passes on a thinking model.
- Add gemini-2.5/3 ids, and "thinking"-suffixed ids, to `reasoningModelRe`.
- Map `ReasoningEffort` to `generationConfig.thinkingConfig`: `thinkingBudget` tiers or `thinkingLevel` per family, with `includeThoughts: false`.
- Declare `CapReasoning` on the Gemini spec so the UI offers the effort control.
- Keep `thoughtsTokenCount` in output usage, and refresh `providers.golden`.

**Touches.** `internal/ai/gemini.go`, `internal/ai/reasoning_floor.go`, `internal/ai/reasoning_floor_test.go`, `internal/ai/spec_gemini.go`, `internal/ai/gemini_errors_test.go`, `internal/ai/testdata/providers.golden`

**Done when.**
- `Chat` on a stub answering `candidates[0]` with no parts and `finishReason` `MAX_TOKENS` returns `errors.Is(err, ErrOutputLimit)`.
- `ReasoningOutputFloor("gemini-2.5-pro") == 16384` and `ReasoningOutputFloor("gemini-2.0-flash") == 0`.
- A Gemini Stream request with `ReasoningEffort` `"low"` serialises `generationConfig.thinkingConfig` with a `thinkingBudget`; `""` omits it. `providers.golden` lists reasoning in Gemini's caps.

### R-provider-15

**pricingFor has no policy for an unknown, zero or free price: a $0 listing row is authoritative, a missing row bills at the default model's rate, and a PricingFree provider is billed from its paid catalogue rows when the listing fails** · tier 1

**Problem.** `pricingFor` short-circuits only `PricingUnpriced`. Otherwise it returns the first listing entry with `m.ID == modelID`, as found, whatever its `Pricing` (`audited.go:605-616`). When the model is absent, `usageMetric` falls back to `PricePerMillionTokens`, records a fallback metric and logs a WARN naming the model (513-520). Every bare-key provider implements that fallback as `specDefaultPricing`: the catalogue row of the spec's `DefaultModel`.

For paid providers a $0 listing row does not occur. Claude, OpenAI, Gemini, xAI and GLM list only the static catalogue, and every one of those rows is priced. Live listings with $0 rows exist only for free providers (Copilot, GitHub Models), where $0 is correct. Two real gaps remain:
- **(a) Uncatalogued models.** An uncatalogued, client-chosen model is billed at the default model's rate, not at $0. A new, more expensive model is therefore under-billed, and the daily cap trips only at the default model's rate. In the metric, `pricingMetricModel` collapses the name to `uncatalogued`; the WARN line is the only place it appears.
- **(b) GitHub Models outages.** The GitHub Models spec is `PricingFree` ("nothing is metered"), and the live listing sets every row to 0/0. But `fetchModels` silently returns `catalogModels` on a request error, non-200, decode error or empty list, with `live=false` and no error, and the github-models catalogue rows carry paid prices. A turn during a `/models` outage therefore records real cost against the daily budget for a service the spec declares free. The no-token branch cannot occur on a chat turn.

**Evidence.**
- `internal/ai/audited.go:605` — `pricingFor`: only `spec.Pricing == PricingUnpriced` returns zero (606-608). The first listing entry with `m.ID == modelID` is returned as found whatever its `Pricing` (609-614).
- `internal/ai/audited.go:514` — `usageMetric`: when not found, it uses `inner.PricePerMillionTokens`, calls `recordPricingFallback` and logs a WARN that names the model (513-520).
- `internal/ai/spec.go:414` — `specDefaultPricing` is the `DefaultModel`'s catalogue row, and `Pricing{}` for free or unpriced providers (414-424). It is used by `claude.go:81`, `openai.go:51`, `xai.go:32` and `glm.go:49`.
- `internal/ai/claude.go:92` — Claude's `Models` is the static catalogue. OpenAI's is too (`openai.go:57-59`), and every Claude, OpenAI, Gemini, xAI and GLM catalogue row is priced (`catalog.go:14-56`).
- `internal/ai/copilot.go:170` — the Copilot live listing sets $0 rows (170-175), as does GitHub Models (`github_models.go:130-135`).
- `internal/ai/served_model.go:263` — `pricingMetricModel` collapses an unknown name to `uncatalogued` (263-271).
- `internal/ai/spec_github_models.go:16` — `Pricing: PricingFree`; the header comment (3-4) says "Free tier: nothing is metered".
- `internal/ai/github_models.go:93` — `fetchModels` returns `catalogModels` with `live=false` and no error in five cases: no token (97-98), request error (102, 108), non-200 (113), decode error (121) and an empty list (139). The live rows set `Pricing` 0/0 (134).
- `internal/ai/catalog.go:59` — the github-models catalogue rows are priced 2.5/10, 0.15/0.6, 0.7/0.9, 2.0/6.0, 0.07/0.14, 0.49/0.94 and 2.0/8.0 (59-67).

**Change.**
- Keep this item to the tier-1 fix. In `pricingFor`, `UsagePricing` and `ReservationPricing`, return a zero `Pricing` that counts as found when `spec.Pricing == PricingFree`, as `PricingUnpriced` already does.
- Strip the prices from the github-models catalogue rows and keep their windows.
- Add a `spec_test` assertion that a `PricingFree` catalogue has no priced rows.

Two ideas are dropped from this item: an `UnknownPricePolicy` per deployment (price an uncatalogued model at the provider's most expensive catalogue row, or refuse the call), and a rule that a $0 row on a `PricingCatalog` provider means "unknown". Both reverse the documented pricing decisions (`docs/adr/0025-provider-specs.md:12`). They belong in a separate tier-3 item with its own ADR. That item would also surface "price unknown" in the model picker and the done payload; the done path already distinguishes `priced=false` (`internal/service/chat.go:1572-1573`).

**Touches.** `internal/ai/audited.go`, `internal/ai/spec.go`, `internal/ai/catalog.go`, `internal/ai/github_models.go`, `internal/ai/audited_pricing_test.go`, `internal/ai/spec_test.go`

**Done when.**
- With the GitHub `/models` GET stubbed to 500, a chat turn's usage row has `estimated_cost` 0 and the done payload's rates are 0. A test asserts that every `PricingFree` catalogue row is unpriced.
- Only in the separate tier-3 item, under its own ADR: a listing entry with `Pricing{}` on an openai-compatible `PricingCatalog` provider is billed at the provider's maximum catalogue rate under the default policy. Policy `refuse` makes a call on an uncatalogued model fail before the request, with a classified code.
- `pad_ai_pricing_fallback_total` still increments on the fallback path.

### R-provider-18

**Bare-key providers have no 'unreadable after rotation' state** · tier 1

**Problem.** After an encryption-key rotation, `EncryptedKeyStore.Get` returns `ErrSecretDecryptFailed` (`keystore.go:144-150`), while `Has` only checks existence (156-167). The Azure registry classifies that state as unreadable, with re-enter copy. Bare keys get no such state:
- `isConfigured` uses `Has` (`providers.go:645-651`), so the catalogue and the listing say "configured".
- `memberKeyLookup` maps only `ErrSecretNotFound` (`services.go:174-179`), and `resolveKey` wraps the decrypt error as `get <provider> key` (`factory.go:105-107`).
- The chat path special-cases `ErrSecretDecryptFailed` only on the Azure connection branch (`chat.go:511-514`), and re-enter copy exists only for `errAzureUnreadableCredentials` (`chat_errors.go:289-290`).

The user therefore gets `errProviderSetup`'s "try again in a moment" text (`chat_errors.go:140`), classified as `PROVIDER_UNAVAILABLE`. The org twin has the same gap:
- `isOrgConfigured` uses `HasOrg` (`providers.go:671`).
- `orgKeyLookup` maps only `ErrSecretNotFound` (`services.go:196-203`).
- `resolveKey` wraps the failure as `get <provider> org key` (`factory.go:113-115`).

`GetGitHubUser` returns `nil, nil` on any `Get` error (`providers.go:1100-1104`), so the device-flow card says "not connected" while the catalogue says "configured". Only the operator log mentions rotation. Only the encrypted database keystore is affected; the desktop keyring maps every `Get` failure to `ErrSecretNotFound` (`storage/keyring.go:55-63`).

**Evidence.**
- `internal/storage/database/keystore.go:144` — a decrypt failure produces a warn log and `ErrSecretDecryptFailed` (144-150). `Has` (156-167) only checks existence.
- `internal/service/providers.go:645` — `isConfigured` calls `Has` on `OAuthToken`/`Primary` (645-651); `isOrgConfigured` calls `HasOrg` at :671.
- `internal/di/services.go:174` — `memberKeyLookup` maps only `ErrSecretNotFound`, so decrypt failures are errors; `orgKeyLookup` does the same (196-203).
- `internal/ai/factory.go:105` — `resolveKey` wraps the failure as `get %s key: %w`, and as `get %s org key: %w` at 113-115.
- `internal/service/chat.go:511` — `ErrSecretDecryptFailed` is special-cased only on the Azure connection branch (513-514).
- `internal/service/chat_errors.go:289` — re-enter copy exists only for `errAzureUnreadableCredentials`. Any other setup failure becomes `errProviderSetup` ("try again in a moment", :140), classified `PROVIDER_UNAVAILABLE` (298-299).
- `internal/service/providers.go:1100` — `GetGitHubUser` returns `nil, nil` on any `Get` error (1100-1104).
- `internal/storage/keyring.go:55` — the desktop keyring maps every `Get` failure to `ErrSecretNotFound` (55-63).
- `core/models/chat.go:281` — `ProviderCatalogEntry`.

**Change.**
- Add a credential status (`configured | unreadable | missing`) to the catalogue and listing entries, for both the member and the org credential. Compute it with `Get` rather than `Has` for bare keys, or with a cheap keystore `Probe` that tries to open the ciphertext without returning plaintext.
- On the non-Azure chat path, map `ErrSecretDecryptFailed` to `PROVIDER_NOT_CONFIGURED`, with copy telling the user to enter the key again.
- Have `CredentialForm` render an "enter the key again" state.
- Have the device-flow card distinguish "unreadable" from "not connected".

**Touches.** `internal/service/providers.go`, `internal/service/chat.go`, `internal/service/chat_errors.go`, `internal/storage/database/keystore.go`, `core/models/chat.go`, `frontend/src/components/settings/providers/CredentialForm.tsx`

**Done when.**
- With a `provider_keys` row sealed by a key that is not configured, `GET /api/providers/catalog` reports status `unreadable` for that provider, and `/api/keys/has` reports `has: true, unreadable: true`.
- A chat turn on that provider answers `PROVIDER_NOT_CONFIGURED`, not `PROVIDER_UNAVAILABLE`, with copy saying the key must be entered again.
- `GET /api/providers/github/user` on an unreadable token answers a coded error rather than `null`.

### R-provider-25

**OpenAI-family 403s read as 'invalid API key' and 429 insufficient_quota as a transient rate limit (retried, told to wait)** · tier 1

**Problem.** `openaiBase.unauthorized` runs `handle403` only when a provider sets it, then `handle401` only when set, then `unauthorizedError` (`openai_compat.go:83-96`). `unauthorizedError` returns `ErrApiKeyInvalid` for everything except code `PermissionDenied` (650-660), and only Azure sets `handle403` (`azure_foundry.go:88`). A 403 from OpenAI, xAI, GLM or GitHub Models is therefore shown as a wrong key, whether it comes from a region block, a project permission or a billing hard-limit. The user is sent to re-enter a valid credential.

A 429 is always `rateLimitErr` unless `handle429` says otherwise (672-678). OpenAI sets no `handle429` (`openai.go:18-30`), and only GLM maps balance codes (`glm.go:24-29`). A quota-exhausted 429 (OpenAI's `insufficient_quota`) is therefore attempted three times: `retryMaxAttempts` is three tries in total, one initial plus two retries. A `Retry-After` hint over 20 s returns at once. The error is reported as `PROVIDER_RATE_LIMITED` with "wait a moment and try again" (`chat_errors.go:245-246`), although `ErrInsufficientBalance` and the `PROVIDER_BALANCE` copy exist (255-256).

**Evidence.**
- `internal/ai/openai_compat.go:83` — `unauthorized` runs `handle403` only if set, `handle401` only if set, else `unauthorizedError` (83-96).
- `internal/ai/openai_compat.go:650` — `unauthorizedError` returns `ErrApiKeyInvalid` for everything except code `PermissionDenied` (650-660).
- `internal/ai/azure_foundry.go:88` — `handle403 = azureForbidden` is set only for Azure.
- `internal/ai/openai_compat.go:672` — a 429 goes to `handle429` (if any), else `rateLimitErr`. OpenAI (`openai.go:18-30`) sets no `handle429`; only GLM does (`glm.go:24-29`).
- `internal/ai/retry.go:49` — `ErrRateLimited` is retryable. `retryMaxAttempts = 3` counts total tries (12-13). A `Retry-After` hint above `maxRetryAfterWait` (20 s, :72) returns at once (78-81).
- `internal/service/chat_errors.go:245` — `ErrRateLimited` gets "wait a moment and try again"; the `ErrInsufficientBalance` → `PROVIDER_BALANCE` copy exists at 255-256.
- `internal/service/chat_errors.go:267` — the `ErrProviderPermissionDenied` copy is Azure-specific ("assign it the Foundry User role", 267-269). The shared-credential variant is Azure-only too (211-212).
- `internal/ai/provider.go:230` — `permissionDeniedError` unwraps to both `ErrProviderPermissionDenied` and `ErrApiKeyInvalid` (230-232).

**Change.**
- Give the compat family a default `handle403` that returns `permissionDeniedError` carrying the vendor's message, so retry and breaker rules stay those of an auth failure. That alone does not give the right copy: the `ErrProviderPermissionDenied` copy and its shared-credential variant are Azure-specific, and `permissionDeniedError` also unwraps to `ErrApiKeyInvalid`. The fix therefore also needs vendor-neutral `PROVIDER_PERMISSION` copy, or a per-provider branch in `classifyFailureBase`.
- Add a default `handle429` for OpenAI, xAI and GitHub Models that maps code `insufficient_quota`, and messages containing "quota" or "billing", to `ErrInsufficientBalance`. Keep GLM's own.
- Claude's 403 `permission_error` is already handled, so it needs no change.

**Touches.** `internal/ai/openai_compat.go`, `internal/ai/openai.go`, `internal/ai/xai.go`, `internal/ai/github_models.go`, `internal/ai/openai_compat_errors_test.go`

**Done when.**
- An OpenAI stub 403 `{"error":{"message":"unsupported_country_region_territory"}}` yields `errors.Is(err, ErrProviderPermissionDenied)` and keeps the message. `TestChatError_403MapsToInvalidKey` is updated accordingly.
- An OpenAI stub 429 `{"error":{"code":"insufficient_quota"}}` yields `errors.Is(err, ErrInsufficientBalance)`, not `ErrRateLimited`, and `RetryingProvider` makes exactly one attempt.
- GLM code 1113 still maps to `ErrInsufficientBalance`.

### R-provider-20

**The direct OpenAI, xAI, Gemini, GLM and Claude pickers are hand-kept catalogues with no live listing, no freshness check, retired rows and OpenAI rows without output caps** · tier 2

**Problem.** `claude`, `openai`, `gemini`, `xai` and `glm` have no `live_listing` cap (`providers.golden:2-6`). Their `Models()` return only `catalogModels` (`openai.go:57-59`, `xai.go:42-44`, `glm.go:59-61`, `gemini.go:151-153`, `claude.go:93-94`). The catalogue carries manual "checked 2026-09-28/29" stamps and one assumed price (`catalog.go:12-13`, `19-21`, `77`). The gaps per vendor:

- **OpenAI:** only three rows (gpt-4o, gpt-4o-mini, gpt-4-turbo), none with `MaxOutputTokens` (`34-38`). Meanwhile `openai.go:32-42` sends `max_completion_tokens` for every model on the default OpenAI URL.
- **Gemini:** two 2.5 rows (`39-42`), also without `MaxOutputTokens`.
- **xAI:** the direct picker (`43-47`, default `grok-3-mini` per `providers.golden:5`) offers no grok-4-family model. The Azure section already sizes grok-4/4.1/4.3/4.6 (`147-152`) and calls grok-3 and grok-3-mini retired for Azure deployments (`142`).
- **Claude:** current, and every row has `MaxOutputTokens` (`14-31`), so it needs a freshness check at most.

The consequences:

- Users cannot select a newer vendor model without a release.
- Nothing signals a price change on a catalogued model. `pad_ai_pricing_fallback_total` counts only models missing from the catalogue (`metrics.go:59-60`, `161-164`), so the daily budget is mis-metered until someone notices.
- `ModelMaxOutputTokens` returns 0 ("don't clamp", `provider.go:371-373`) for every OpenAI model, so an over-sized MaxTokens reaches the API.

For an assistant working on large PAD flows, the chosen model's window and output cap are load-bearing inputs to truncation.

**Evidence.**
- `internal/ai/testdata/providers.golden:2` — claude, openai, gemini, xai and glm (lines 2-6) have no `live_listing` cap
- `internal/ai/openai.go:57` — Models returns `catalogModels(openaiSpec.Catalog)`; the same holds at `xai.go:42`, `glm.go:59`, `gemini.go:151` and `claude.go:93`
- `internal/ai/catalog.go:12` — a manual "checked 2026-09-28" stamp; at 19-21 claude-mythos-preview's "rate is assumed"; at 77 "2026-09-29"
- `internal/ai/catalog.go:34` — the openai rows (34-38) are three models with no `MaxOutputTokens`; the gemini rows (39-42) are two 2.5 models; the xai rows (43-47) are grok-3, grok-3-mini and grok-2
- `internal/ai/catalog.go:142` — "grok-3/-3-mini are retired but kept for legacy deployments", in the Azure section, which sizes the grok-4 family at 147-152; the direct xAI rows have no grok-4-family model
- `internal/ai/provider.go:371` — `ModelMaxOutputTokens` returns 0 when the cap is unset, and callers treat 0 as "don't clamp"

**Change.** Refresh the OpenAI, Gemini, xAI and GLM catalogues with prices and `MaxOutputTokens`: current OpenAI o-series, gpt-4.1 and gpt-5.x; the grok-4 family; gemini-2.5-flash-lite and newer; current GLM. Claude needs no refresh.

Add `CapLiveListing` to the OpenAI-wire direct vendors. They list through GET `{base}/v1/models` via `modelListCache`, as Copilot does, with the catalogue as the price, window and output-cap overlay and a conservative default for unknown ids. OpenAI's `/v1/models` also returns non-chat ids (embeddings, tts, image), so the listing must filter them out or overlay only known chat families.

The cheapest guards are two requirements: every row has `MaxOutputTokens > 0`, and the xAI default is not retired. A freshness test that compares the table with a pinned vendor fixture under `testdata` (refreshed deliberately, so drift shows up as a reviewed diff) only moves the manual step elsewhere. It is still the only thing that would catch a price change on a catalogued model. Refresh `providers.golden` and `TestProviderCatalogConsistency`.

**Touches.** `internal/ai/catalog.go`, `internal/ai/catalog_test.go`, `internal/ai/spec_openai.go`, `internal/ai/spec_xai.go`, `internal/ai/spec_glm.go`, `internal/ai/openai.go`, `internal/ai/xai.go`, `internal/ai/glm.go`, `internal/ai/gemini.go`, `internal/ai/testdata/providers.golden`

**Done when.**
- For a provider with the new cap, `Models()` against a stub `/v1/models` returns the live chat ids (non-chat ids filtered out), with catalogue prices applied where known and a non-zero ContextLimit on every entry. Without a credential it returns the catalogue.
- Every catalogue row of a catalogue-priced provider, Gemini rows included, has `MaxOutputTokens > 0`.
- The xAI catalogue offers a grok-4-family model and a non-retired default.
- Each provider's connection-test default model exists in the returned listing.
- A catalogue row whose price differs from the pinned fixture fails `TestCatalogFreshness`, naming the model.

### R-provider-27

**Org-shared credentials exist only for Azure although six specs advertise org_shared** · tier 2

**Problem.** `claude`, `openai`, `gemini`, `xai`, `glm` and `github-models` declare `CapOrgShared` (`spec_claude.go:9`, `spec_gemini.go:8`, `spec_github_models.go:9`; `providers.golden:2-7`). The rest of the stack treats them as shareable:

- the catalogue reports `Caps.Names` including `org_shared` (`providers.go:873`);
- the factory resolves org keys for any spec (`factory.go:112-119`);
- `isOrgConfigured` checks `HasOrg` (`providers.go:656-673`).

But the only production writers of `org_provider_keys` are the Azure registry paths. A grep for `SaveOrgIfUnchanged(` outside tests finds `azure_models.go:1173`, `azure_models_legacy.go:597` and `providers.go:1378`, all Azure. `OrgKeyStore.SaveOrg` (`interfaces.go:45`) has no bare-key caller. An org admin therefore cannot lend an OpenAI or Claude key. Members on those providers get "not configured", while the catalogue and the allowed-provider policy suggest that sharing works. ADR 0025 lists this as open (`0025-provider-specs.md:21`).

**Evidence.**
- `internal/ai/spec_claude.go:9` — `CapOrgShared` (also `spec_gemini.go:8` and `spec_github_models.go:9`; `providers.golden:2-7` shows `org_shared` on all six)
- `internal/service/providers.go:873` — the catalogue reports `Caps.Names` including `org_shared`; at 656-673 `isOrgConfigured` uses `HasOrg`
- `internal/ai/factory.go:112` — `resolveKey` reads the org key for any spec (112-119)
- `internal/service/interfaces.go:45` — `OrgKeyStore.SaveOrg` exists; grep shows `SaveOrgIfUnchanged` called only from `azure_models.go:1173`, `azure_models_legacy.go:597` and `providers.go:1378`
- `docs/adr/0025-provider-specs.md:21` — "Open: org-shared credentials are only wired for Azure"

**Change.** Add org routes for bare keys:

- `POST`/`DELETE /api/orgs/{id}/providers/{provider}/key` and a has/meta read, writing through `SaveOrgIfUnchanged` with the lender stamp;
- extend the admin-demotion disable in `DisableOrgAzureCredentialsSavedBy` to the bare rows;
- publish `CredentialChanged` with the org scope;
- show the keys in `OrganizationsPanel` next to `OrgAzureSection`.

`TestOrgProviderConnection` is already reachable over HTTP, but only for Azure (`internal/api/org_azure.go:197`). Generalise that route beyond `azure-foundry`.

`github-models` is a personal OAuth device-flow token (`spec_github_models.go:8-12`). Copilot is the same kind of credential, and `isOrgConfigured` excludes it as "a person's subscription" (`providers.go:654-655`). Decide whether `github-models` keeps `CapOrgShared` at all, rather than adding a lend route for it.

**Touches.** `internal/api/org_providers.go` (new), `internal/service/providers.go`, `internal/service/azure_models_lifecycle.go`, `internal/service/system.go`, `frontend/src/components/settings/OrganizationsPanel/`, `frontend/src/api/providers.ts`

**Done when.**
- An org admin saves an openai key for the org. A member with no key of their own gets a working ForOrg chain whose usage row names the org, and `GET /api/providers/list?orgId=` marks openai shared.
- After the lending admin is demoted, the member's next turn fails with PROVIDER_NOT_CONFIGURED and the catalogue shows openai unshared.
- The catalogue advertises `org_shared` only for providers the deployment can actually share. The `github-models` decision (keep `CapOrgShared` with a lend route, or drop it) is recorded and reflected in `providers.golden`.

**ADR.** Listed as open in ADR 0025; not implemented.

### R-provider-c2

**A cut-short project listing replaces the stale complete listing that registry providers keep on purpose, and callers cannot tell it is partial** · tier 2

**Problem.** Registry providers never wait on the network. They serve the cached project listing for up to an hour past expiry because, in the code's own words, "a listing an hour old is still the better guess than the undecorated entries". When a background refresh is cut short (extras timeout, 429, network error, agents failure), `listingFlight` stores whatever `projectExtras` returned for 30 s and overwrites the stale complete entry. After a timeout or 429, that result is the bare configured deployments:

- no served model;
- the provider-wide 128k window, unless the deployment name itself is catalogued;
- no output cap;
- no project-only deployments;
- no Claude relisting.

Once the 30 s pass, the degraded entry itself is served under the same one-hour grace until a refresh replaces it. `azureModelsEntry` holds only the models and an expiry (`azure_foundry.go:529-532`), so no caller can tell the entry is partial.

The impact is narrower than the listing alone suggests:

- The audited wrapper's `Models` still applies the process-local served-model memo (`audited.go:697-699`, `served_model.go:198-214`, 24 h TTL at `served_model.go:21`). Any deployment that answered a turn for this user on this replica keeps its ServedModel, catalogue window and output cap even on the degraded listing.
- The user's registry overrides still apply (`audited.go:702`).
- Registry agent entries stay listed through `addUnlistedRegistryModels` (`audited.go:696`, `azure_registry.go:421-440`).

Until the next successful refresh, three things are lost:

- the served model and limits of deployments not yet served on this replica;
- project-only deployments;
- the Claude relisting of chat-kind entries (`relistedClaude`, `azure_foundry.go:419-429`).

**Evidence.**
- `internal/ai/azure_registry.go:483` — `azureModelsStaleGrace` is 1 h: "a listing an hour old is still the better guess than the undecorated entries"
- `internal/ai/azure_foundry.go:400` — registry providers read with `cachedAzureModelsWithin(key, azureModelsStaleGrace)`; routing reads the same way at 412-413
- `internal/ai/azure_registry.go:505` — `refreshListing` treats an expired (stale) entry as not live and starts a new flight
- `internal/ai/azure_foundry.go:636` — `listingFlight` stores any non-empty result unconditionally; an incomplete one gets `azureModelsRetryAfter` (30 s) and replaces the previous entry
- `internal/ai/azure_agents.go:85` — a deployments failure on a ctx deadline returns `models, false`, which is only the configured deployments; a 429 does the same at 92-94
- `internal/ai/azure_agents.go:99` — an agents failure leaves the merged deployments without agents, stored as incomplete

**Change.** Add `complete bool` and `fetchedAt` to `azureModelsEntry`. In `listingFlight`, handle incomplete results this way:

- If the existing entry for the key is complete and within `azureModelsStaleGrace`, keep the old models instead of overwriting them, and push back only the entry's next-attempt time (a separate `retryAt` field).
- If deployments succeeded but agents failed, merge the new deployments with the previous entry's agents.

Return completeness and age with the listing (a `ModelsListing{Complete, AsOf}` accessor, used by `windowsFor`). Do this together with [R-provider-c3](provider.md#r-provider-c3)'s "keep partial pages" work, since both change what `listingFlight` stores for an incomplete result.

**Touches.** `internal/ai/azure_foundry.go`, `internal/ai/azure_agents.go`, `internal/ai/azure_registry.go`, `internal/ai/azure_registry_test.go`, `internal/ai/azure_listing_test.go`, `internal/service/azure_models_runtime.go`

**Done when.**
- With a complete registry listing cached and then expired (within the grace), a refresh whose deployments call times out leaves `p.Models` returning the earlier listing: `prod-chat` still has the ServedModel that listing gave it.
- The failed refresh is retried no sooner than `azureModelsRetryAfter`, and a later complete refresh replaces the entry with the full TTL.
- A refresh whose agents call fails but whose deployments succeed keeps the previous agents and stores the new deployment decorations.

### R-provider-c3

**One 10 s budget covers both paginated project listings; partial pages are thrown away and a slow or large project never completes** · tier 2

**Problem.** `azureProjectExtrasTimeout` is a single 10 s bound over the whole deployments listing and the whole agents listing (up to 10 pages each, 100 agents per page). Both run at the same time under that one context, and the fresh-token retry after a 401 also runs inside it.

The background refresher gives a job 45 s (`azure_models_runtime.go:578`). But `listingFlight` runs the fetch under `context.WithoutCancel(ctx)` (`azure_foundry.go:635`), which strips that deadline. As a result:

- the 45 s only bounds how long `refreshListing` waits on the flight (`azure_registry.go:509-514`);
- the token fetch runs under the detached context;
- the extras get at most 10 s.

Pages fetched before a deadline are discarded, because `fetchProjectDeployments` and `fetchAgentsReport` return nil on the first error. A project that needs more than 10 s, or one with a slow page, therefore never produces a complete listing. Each attempt is cached for a fixed 30 s, whatever Retry-After the project sent. The downstream effect is the same degraded registry listing as in [R-provider-c2](provider.md#r-provider-c2), which the served-model memo partly masks (`audited.go:699`).

**Evidence.**
- `internal/ai/azure_foundry.go:527` — `var azureProjectExtrasTimeout = 10 * time.Second` covers deployments and agents, every page
- `internal/ai/azure_foundry.go:701` — the token is obtained under the flight's detached ctx, then `context.WithTimeout(ctx, azureProjectExtrasTimeout)` wraps all extras (705)
- `internal/service/azure_models_runtime.go:30` — `azureRefreshTimeout` is 45 s per refresh job (used at 578); it bounds only the wait on the flight, because `listingFlight` detaches the fetch (`azure_foundry.go:635`)
- `internal/ai/azure_agents.go:37` — `azureProjectMaxPages` is 10, `azureProjectMaxAgents` is 200, and the agents page size is 100
- `internal/ai/azure_agents.go:66` — both listings run concurrently under the same ctx
- `internal/ai/azure_agents.go:203` — an error on any deployments page returns `nil, err` and drops the pages already read; agents do the same at 337-340

**Change.** Treat this as tier 3 and fold it into [R-provider-c2](provider.md#r-provider-c2)'s `listingFlight` change:

- Keep the deployments pages read before an error and mark the result incomplete, instead of discarding them.
- Set the incomplete entry's next attempt to `max(azureModelsRetryAfter, RateLimitError.RetryAfter)`.
- Give the deployments listing its own budget, so a slow agents listing cannot use it up.
- Carry a budget taken from the caller's deadline (the refresher's 45 s) into the flight explicitly, because `context.WithoutCancel` drops it. Keep a fallback cap for detached `Models` flights.

Per-page timeouts (for example 10 s per GET) are optional.

**Touches.** `internal/ai/azure_foundry.go`, `internal/ai/azure_agents.go`, `internal/ai/azure_listing_test.go`, `internal/service/azure_models_runtime.go`

**Done when.**
- A test server whose deployments pages each take 3 s (4 pages) yields a complete listing under `RefreshAzureFoundryListing` with a 45 s ctx.
- A server that times out on page 3 of 4 yields a listing that includes the deployments from pages 1-2, marked incomplete.
- A 429 with `retry-after-ms: 120000` from the deployments route sets the next attempt no earlier than 120 s, and `refreshListing` makes no project request before then.

### R-provider-7

**A retried Responses attempt inherits an unbalanced SilentStep, so it runs under the 5-minute silent-phase idle limit** · tier 3

**Problem.** `parseResponsesSSE` emits `SilentStep` +1 when a reasoning output item opens (`responses.go:1429`) and −1 only on `output_item.done` (`:1485`). The chat loop turns +1 into `ctl.hostedStep(open)` (`chat.go:1223-1224`). While `hostedOpen > 0`, the idle limit rises from 90 s to the hosted-step limit (`chat_resume.go:198-200`). `RetryingProvider.Stream` forwards `SilentStep` chunks without marking the attempt delivered. `delivered` is set only by Text, Done, ToolCalls, ToolEvent and ServerStep (`retry.go:256-268`). A pre-content error after the reasoning item opened (a mid-stream 5xx or overloaded event, a transport reset) is therefore retried. The balancing −1 never arrives, and `hostedOpen` is reset only by `endProviderTurn`, after the whole retrying `Stream` call returns (`chat_stream_control.go:240-243`). Before its first output, the retried attempt runs under the 5-minute limit by design anyway, through `awaitingOutput` (`chat_stream_control.go:139-142`, `chat_resume.go:200`). The defect comes after that. Once the retried attempt has emitted its first text, tool call or Done, a stall mid-answer is caught after 5 min instead of 90 s, for the rest of that provider call. A second retry adds another +1 on top.

**Evidence.**
- `internal/ai/responses.go:1429` — `emit(Chunk{SilentStep: 1})` on a reasoning `output_item.added`; −1 only on `output_item.done` (1485)
- `internal/ai/retry.go:265` — `delivered` is set only by Text/Done/ToolCalls/ToolEvent/ServerStep; `SilentStep` chunks are forwarded (268), so a later pre-content error retries
- `internal/service/chat.go:1223` — tool loop: `SilentStep` → `ctl.hostedStep(c.SilentStep > 0)`
- `internal/service/chat_stream_control.go:240` — `endProviderTurn` resets `hostedOpen` only after `provider.Stream` returns
- `internal/service/chat_stream_control.go:139` — `awaitingOutput` keeps the hosted limit until a provider call's first output (139-142); `sawOutput` clears it (249-253)
- `internal/service/chat_resume.go:198` — `idleLimitFor`: `hostedOpen > 0` or `awaitingOutput` raises the idle limit to the hosted-step limit (198-210)

**Change.** Have `RetryingProvider` close what a failed attempt opened. Track the net `SilentStep` balance per attempt, and before starting the next attempt, emit a closing `SilentStep` −1 for each unbalanced open. Alternatively, hold `SilentStep` chunks like the held error and release them only once content follows. Pin it with a retry test that sums `SilentStep` and a chat test that reads `hostedOpen` after the retried attempt's first text.

**Touches.** `internal/ai/retry.go`, `internal/ai/retry_test.go`, `internal/service/chat.go`, `internal/service/chat_stream_control.go`

**Done when.**
- `RetryingProvider.Stream`: attempt 1 emits `SilentStep` +1 then a pre-content `ErrProviderDown`; attempt 2 emits +1, −1, text, Done. The caller sees a net `SilentStep` sum of 0, with +1/−1 balanced before attempt 2's chunks.
- After the retried attempt's first Text chunk, `hostedOpen` is 0 and `idleLimitFor` returns the 90 s default.

### R-provider-12

**Learned context windows are alias-only and only help the next chain build; the learning turn always fails** · tier 3

**Problem.** `detectContextLimitError` produces a `ContextLimitError{Limit}` whenever a rejection uses the OpenAI or Claude wording (`provider.go:259-265`). Gemini's wording returns a bare `ErrContextLimit` (`:243`, `:253`). `recordContextWindow` discards the limit unless the provider uses model aliases: it returns when `!aliasModelProviders[providerID]` (`learned_window.go:31`). This is a documented, test-pinned choice (`learned_window.go:10-14`, `26-28`; `learned_window_test.go:100-111`).

Even for Foundry, the knowledge does not help the turn that learns it:
- `Models` is memoised per audited instance, and `applyLearnedWindows` is applied once (`audited.go:689-707`).
- The tool loop fixes `ctxLimit` at loop start (`chat.go:1074`) and reuses it in `trimmedTurn` (`1123`).
- `isRetryable` excludes `ErrContextLimit` (`retry.go:49-51`).

The learning turn therefore fails, and the knowledge is used only by the next request. `archtest/state_test.go:69` accepts "one wasted rejected call per replica".

**Evidence.**
- `internal/ai/learned_window.go:29` — `recordContextWindow` returns early when `!aliasModelProviders[providerID]` (31); the alias-only rule is documented at 10-14 and 26-28
- `internal/ai/learned_window_test.go:100` — `TestLearnedWindows_OnlyNarrowAndOnlyAliasProviders` pins that a non-alias provider records nothing (100-111)
- `internal/ai/provider.go:238` — `detectContextLimitError` builds `ContextLimitError{Limit}` only from the OpenAI and Claude wordings (`contextWindowRes`, 259-265); Gemini's wording yields bare `ErrContextLimit` (243, 253)
- `internal/ai/audited.go:689` — `Models` is memoised with `modelsOnce`; `applyLearnedWindows` is applied once, at the first listing (703)
- `internal/service/chat.go:1074` — `ctxLimit: ai.ContextLimitFor(...)` is captured once in `newToolLoopState`; `trimmedTurn` reuses `st.ctxLimit` (1123)
- `internal/ai/retry.go:47` — `isRetryable` excludes `ErrContextLimit` (49-51), so the rejection ends the turn
- `internal/archtest/state_test.go:69` — the `learnedWindows` registry row accepts "one wasted rejected call per replica"

**Change.** Narrow the item to an in-turn retry. When an iteration fails with a `*ContextLimitError` that carries a Limit, lower `st.ctxLimit`, re-run `truncateForContextWindowMemo` and retry the iteration once before failing. Keep learning alias-only unless a concrete direct-provider mismatch is shown. If one is, fix it with the catalogue row or a registry override, not with runtime learning. Recording for every provider would help only Claude and OpenAI-compatible routes in any case, unless a Gemini regex is added.

**Touches.** `internal/service/chat.go`, `internal/service/chat_context.go`, `internal/service/chat_context_window_test.go`

**Done when.**
- In the tool loop, an iteration that fails with `ContextLimitError{Limit}` is re-truncated to the stated window and retried once. The turn succeeds, and only one rejected call is billed.
- A rejection with no stated window still fails the turn with `CONTEXT_LIMIT` (no infinite retry).
- `TestLearnedWindows_OnlyNarrowAndOnlyAliasProviders` still passes.

### R-provider-16

**Server error codes the client does not classify (AI_BUDGET_UNAVAILABLE, CONTEXT_LIMIT, PROVIDER_RATE_LIMITED, TOOL_LOOP_LIMIT, STREAM_TIMEOUT) render as a generic bubble with Retry** · tier 3

**Problem.** `classifyChatFailure` returns `'generic'` for any code it does not list (`chatErrors.ts:19`); `CHAT_ERROR_CODES` at 4-12 lists `AI_BUDGET_EXCEEDED` but none of these five. `PROVIDER_ERRORS` covers only the `PROVIDER_*` codes (92-119). Every other code the server emits loses its class (`chat_errors.go:16-27`, classified at 241-246): `AI_BUDGET_UNAVAILABLE`, `CONTEXT_LIMIT`, `STREAM_TIMEOUT`, `TOOL_LOOP_LIMIT` and `PROVIDER_RATE_LIMITED`. The bubble offers Retry on every error (`MessageBubble.tsx:506`). For three of the codes that is the intended remedy:
- `AI_BUDGET_UNAVAILABLE` fails closed before any provider call (`audited.go:132-146`), so the refusal is not a paid attempt. Its copy says "try again in a moment" (`chat_errors.go:132`); it only loses its budget-style presentation.
- `PROVIDER_RATE_LIMITED` reaches the client only after the retrying provider has retried with backoff (`retry.go:47-51`), and its copy already says to wait and try again (`chat_errors.go:246`).
- `STREAM_TIMEOUT` already gets the plain Retry it needs.

The concrete defects are these:
- A context-window overflow, whose copy says "start a new conversation or remove some history", offers Retry, which fails the same way, instead of Compact.
- `TOOL_LOOP_LIMIT` carries no guidance.
- `errorCode` is stored on the message only for a provider error (`events.ts:114`), so the bubble cannot branch on any other code later.

**Evidence.**
- `internal/service/chat_errors.go:16` — `ChatErr*` codes, including `AI_BUDGET_UNAVAILABLE` (17), `PROVIDER_RATE_LIMITED` (19), `CONTEXT_LIMIT` (23), `STREAM_TIMEOUT` (25) and `TOOL_LOOP_LIMIT` (27)
- `internal/service/chat_errors.go:241` — budget unavailable is classified as `ChatErrBudgetUnavailable`; 244 has the `CONTEXT_LIMIT` copy; 246 the rate-limited copy
- `internal/service/chat_errors.go:126` — `budgetUnavailable`: "couldn't verify today's AI budget — try again in a moment" (126-135)
- `internal/ai/audited.go:132` — `admit` refuses before the provider call (132-146)
- `internal/ai/retry.go:47` — `ErrRateLimited` is retryable, so it is retried before it surfaces (47-51)
- `frontend/src/lib/chatErrors.ts:19` — `if (code) return 'generic'` for any unknown code; `CHAT_ERROR_CODES` (4-12) lists `AI_BUDGET_EXCEEDED` only
- `frontend/src/lib/chatErrors.ts:92` — `PROVIDER_ERRORS` has no entry for the five codes (92-119)
- `frontend/src/components/chat/hooks/stream-engine/events.ts:103` — generic branch (103-105); `errorCode` is kept only for a provider error (114)
- `frontend/src/components/chat/MessageBubble.tsx:506` — Retry is offered on every error bubble (`isError && onRetry`)
- `internal/service/chat.go:887` — the pre-send window check fails the turn with the `CONTEXT_LIMIT` message (887-888); the tool loop does the same at `:1125`

**Change.**
- Store `errorCode` on every coded error message.
- For `CONTEXT_LIMIT`, render a Compact action (`handleCompact`) and hide Retry.
- For `TOOL_LOOP_LIMIT`, add a hint to disable tools or narrow the question.
- For `AI_BUDGET_UNAVAILABLE`, use the budget-style copy and toast; Retry stays the remedy.
- Leave `STREAM_TIMEOUT` with its plain Retry.
- Add no automatic resend for `PROVIDER_RATE_LIMITED`, because the server has already retried it.

Tier 3.

**Touches.** `frontend/src/lib/chatErrors.ts`, `frontend/src/components/chat/hooks/stream-engine/events.ts`, `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts`, `frontend/src/components/chat/MessageBubble.tsx`, `frontend/src/locales (chat errors)`, `frontend/src/components/chat/ChatInput.tsx`, `frontend/src/components/chat/AITab.tsx`, `frontend/src/i18n/en.ts`

**Done when.**
- In `chatErrors.test`, `CONTEXT_LIMIT`, `TOOL_LOOP_LIMIT` and `AI_BUDGET_UNAVAILABLE` each map to a non-generic kind with the specified info and actions.
- A `useChatStreamEngine` test shows that an error event with `CONTEXT_LIMIT` commits a bubble whose `errorCode` is `CONTEXT_LIMIT`, and that `MessageBubble` renders a Compact action and no Retry.
- A `useChatStreamEngine` test shows that `AI_BUDGET_UNAVAILABLE` shows the budget-style copy and toast, and that `PROVIDER_RATE_LIMITED` triggers no automatic resend.

### R-provider-19

**Keystore and configured-state reads ignore the request context and serialise per spec** · tier 3

**Problem.** Every `EncryptedKeyStore` method runs on `context.Background` with a fixed `keystoreTimeout` of 5 s (`keystore.go:21`; `117`, `134`, `157`, `171`). The catalogue, listing and "has key" paths also call `hook.Configured` and `OrgConfigured` with `context.Background` (`providers.go:635`, `665`; `system.go:373`). `ProviderCatalog` checks each offered spec in a serial loop (863-876):
- `isConfigured` issues one `Has` per bare-key spec, and two for Copilot when its OAuth-token row is absent (`providers.go:645-651`).
- `isOrgConfigured` adds one `HasOrg` only when `orgID != ""` and the spec is `org_shared` (`providers.go:656-672`). In cloud mode 7 of the 8 specs are `org_shared` (`providers.golden:2-9`).
- Azure answers through its registry hook instead.

That is roughly 9-17 sequential round trips per catalogue request, and `ListProviders` follows the same serial pattern (`providers.go:697-714`). Under normal latency this costs tens of milliseconds. It reaches tens of seconds only when each operation approaches the 5 s timeout. The real defects are that the work cannot be cancelled by the client and that it keeps pool connections busy after the caller is gone when the database is slow.

**Evidence.**
- `internal/storage/database/keystore.go:21` — `keystoreTimeout` is 5 s; 117, 134, 157 and 171 each use `context.Background` per operation
- `internal/service/providers.go:635` — `hook.Configured(context.Background(), scope)`; 665 `OrgConfigured` likewise
- `internal/service/providers.go:645` — up to two `Has` calls per spec (OAuth token, then primary key) (645-651)
- `internal/service/providers.go:656` — `isOrgConfigured`: nothing without an org or for a spec that is not `org_shared`, otherwise a hook or one `HasOrg` (656-672)
- `internal/service/providers.go:876` — `Configured: isConfigured(scope) || isOrgConfigured(orgID)` per spec, in a serial loop (863)
- `internal/service/providers.go:697` — `ListProviders` resolves each spec's listing serially through the same probes (697-714)
- `internal/ai/testdata/providers.golden:2` — 8 specs run in cloud mode (2-9), 7 of them with `org_shared`
- `internal/service/system.go:373` — `HasApiKey`: `hook.Configured(context.Background(), scope)`

**Change.** The more valuable half is one keystore query per scope that the catalogue and the listing both consult, instead of per-spec existence probes. Model it on `ListFamily` (`keystore.go:393`): `SELECT provider FROM provider_keys WHERE user_id=$1`, plus the org twin. Cover `ProviderCatalog` and `ListProviders`. As a secondary step, thread ctx through `KeyStore` and `OrgKeyStore` (`SaveCtx`, `GetCtx`, `HasCtx`, `DeleteCtx`) with the 5 s cap as a ceiling, not a replacement. The archtest layering rules already permit the service-to-storage direction. Tier 3: with eight specs and indexed `EXISTS` queries, the cost matters only on an already-degraded database.

**Touches.** `internal/service/interfaces.go`, `internal/storage/database/keystore.go`, `internal/storage/keyring.go`, `internal/service/providers.go`, `internal/service/system.go`, `internal/testutil/`

**Done when.**
- `GET /api/providers/catalog` issues at most two keystore queries, whatever the number of specs (query counter in the test backend).
- A cancelled request context aborts an in-flight keystore read (tested with a blocking fake DB).

### R-provider-21

**Request has no tool_choice or structured-output field, so the harness cannot force a tool call or request JSON output from any provider** · tier 3

**Problem.** `ai.Request` carries `Model`, `Messages`, `SystemPrompt`, `Temperature`, `MaxTokens`, `TemperatureSet`, `ReasoningEffort`, `Tools` and `OrgID` (`provider.go:63-81`). There is no way to:
- require the model to call a specific tool, or any tool
- disable parallel tool calls
- ask for a JSON or schema-constrained answer

The `Cap` set (`spec.go:95-113`) has no capability for either. `buildRequest` ends with max tokens, reasoning and stream options (`openai_compat.go:138-152`). A grep for `tool_choice|response_format|json_schema` over `internal/ai` finds only a test fixture of a `parameter_not_supported` error shape. Schema-structured actions already go through function tools whose JSON-schema parameters reach the model: `propose_fix`, `apply_fix` and `apply_fixes` (`tools.go:306-328`), serialised by `toOpenAITools` (`openai.go:190-205`). The gap is forcing a call (`tool_choice`) and a JSON-mode final answer, and no current caller needs either.

**Evidence.**
- `internal/ai/provider.go:63` — `Request` fields (63-81): no `ToolChoice`, `ResponseFormat` or `ParallelToolCalls`
- `internal/ai/spec.go:95` — `Cap` set (95-113): chat, stream, embeddings, tools, reasoning, `model_aliases`, `live_listing`, `org_shared`, `prompt_cache`; no structured-output or tool-choice capability
- `internal/ai/openai_compat.go:138` — `buildRequest` ends with max tokens, reasoning and stream options (138-152); nothing serialises a tool choice or response format (a grep over `internal/ai` hits only `openai_compat_foundry_test.go:99`)
- `internal/ai/openai.go:73` — `openAIRequest` (73-90) has no `tool_choice`, `response_format` or `parallel_tool_calls` fields
- `internal/ai/openai.go:190` — `toOpenAITools` sends each tool's `InputSchema` as function parameters (190-205)
- `internal/ai/tools.go:306` — `propose_fix`, `apply_fix` (315) and `apply_fixes` (328) are schema-typed function tools
- `internal/ai/claude.go:104` — `claudeRequest` (104-112) has no `tool_choice`; `Thinking` at 112
- `internal/ai/responses.go:28` — `responsesRequest` (28-41) has no `tool_choice` or `text.format`
- `internal/ai/gemini.go:167` — `geminiRequest` (167-172) has no `toolConfig` or `responseSchema`

**Change.** Treat this as an enabler, not a defect, and do it when a concrete workflow needs it, for example a forced final tool call. Start with `ToolChoice` alone (`auto | none | required | {Name}`), plus `ParallelToolCalls *bool`. Serialise them in four places:
- `openaiBase`: `tool_choice` and `parallel_tool_calls`
- Claude: `tool_choice {type: any|tool}`
- Gemini: `toolConfig.functionCallingConfig`
- the Responses codec

Claude does not accept a forced `tool_choice` (any or tool) together with extended thinking, so the Claude codec must fall back to auto when `Thinking` is set (`claude.go:112`). Route a rejection of the parameter through the existing unsupported-parameter repair, so an unsupported deployment degrades once and the repair is memoised. Add `ResponseFormat` with a JSON-schema option behind a new `CapStructuredOutput`, which the catalogue route reports, only once something consumes it. Tier 3.

**Touches.** `internal/ai/provider.go`, `internal/ai/spec.go`, `internal/ai/openai_compat.go`, `internal/ai/claude.go`, `internal/ai/gemini.go`, `internal/ai/responses.go`, `internal/ai/spec_*.go`, `internal/ai/testdata/providers.golden`, `internal/ai/openai.go`, `internal/ai/stream_tools_test.go`

**Done when.**
- A Request with `ToolChoice{Name:"propose_edit"}` produces `tool_choice {type:function, function:{name}}` on the OpenAI wire and `{type:tool, name}` on the Claude wire (golden request bodies). With `Thinking` set, the Claude codec sends auto.
- A model that rejects `tool_choice` with the known "unsupported parameter" shape is retried without it exactly once, and the repair is memoised like other repairs.
- When `ResponseFormat` lands, `providers.golden` lists `structured_output` only for providers whose codec sends a response format.

### R-provider-22

**Device-flow slow_down is collapsed into pending with no interval hint, the client polls at a fixed interval, and GitHub device auth has no tests** · tier 3

**Problem.** `PollToken` for GitHub Models and Copilot returns Status "pending" for both `authorization_pending` and `slow_down` (`github_auth.go:107-112`, `copilot_auth.go:112-117`). The decode struct discards the body's other fields, including the new interval GitHub sends with `slow_down` (`github_auth.go:99-102`; `copilot_auth.go:104-107`). `GitHubAuthResult` has no interval field (36-40); `DeviceAuthResponse.Interval` exists only on the start response (33). The service passes the result through unchanged (`providers.go:1069-1084`, `1118`). `DeviceAuthLoginButton` keeps the interval from the start response and re-arms the same value after every poll (`DeviceAuthLoginButton.tsx:88`, `121`, `124`), and it swallows poll exceptions silently (`118-119`).

RFC 8628 §3.5 requires the client to add 5 s after `slow_down`. A client that does not is throttled for the rest of the flow, and the sign-in eventually expires. The client then shows the `deviceAuth.expired` copy (`DeviceAuthLoginButton.tsx:93-96`) but not the cause. `expired_token` and `access_denied` reach the client only as the raw error string (`github_auth.go:113-114`). A glob for `github_auth*_test.go` finds nothing; only the Copilot twin is covered, and `copilot_auth_test.go:99-116` pins `slow_down` to pending.

**Evidence.**
- `internal/ai/github_auth.go:107` — `authorization_pending` and `slow_down` both return Status "pending" (107-112); any other error is the raw string (113-114)
- `internal/ai/github_auth.go:99` — the decode struct keeps only `access_token` and `error` (99-102)
- `internal/ai/copilot_auth.go:112` — the same collapse for the Copilot client id (112-117); decode struct at 104-107
- `internal/ai/github_auth.go:36` — `GitHubAuthResult` carries Status, Token and Error, but no interval; `DeviceAuthResponse.Interval` exists only on the start response (33)
- `internal/service/providers.go:1069` — `PollGitHubAuth` and `PollCopilotAuth` (1118) return the result unchanged apart from blanking the token
- `frontend/src/components/chat/DeviceAuthLoginButton.tsx:88` — the interval is fixed from the start response and re-armed at 121 and 124; expiry copy at 93-96; poll exceptions are swallowed at 118-119
- `internal/ai/copilot_auth_test.go:99` — `TestCopilotAuth_PollToken_SlowDown` pins `slow_down` to "pending" (99-116)

**Change.**
- Add `github_auth_test.go`, mirroring `copilot_auth_test.go`. It covers the device-code start, pending, `slow_down`, error, success, a non-OK status, and `GetUser`.
- Return distinct statuses for `expired_token` and `access_denied`, and give them translated copy in the frontend.
- Optionally, as hardening, add an interval hint to the poll result. Pass through the interval the server sent with `slow_down` rather than computing +5 s, and have the client re-arm with it. If the hint is added, update `TestCopilotAuth_PollToken_SlowDown` deliberately.

The client already waits a full interval after each response and has expiry copy. Tier 3.

**Touches.** `internal/ai/github_auth.go`, `internal/ai/copilot_auth.go`, `internal/ai/github_auth_test.go`, `internal/service/providers.go`, `frontend/src/components/chat/DeviceAuthLoginButton.tsx`, `frontend/src/types/providers.ts`

**Done when.**
- `github_auth_test.go` exists and covers `StartDeviceFlow`, `PollToken` (pending, `slow_down`, error, success, non-200) and `GetUser`.
- `PollToken` on `{"error":"expired_token"}` or `{"error":"access_denied"}` returns a terminal status distinct from a generic error, and the button shows translated copy for each.
- If the interval hint is added, `PollToken` on `{"error":"slow_down"}` returns the server's new interval, and the `DeviceAuthLoginButton` test re-arms its timer with it.

### R-provider-26

**Gemini embeds chat queries with taskType RETRIEVAL_DOCUMENT; no query-side task type exists** · tier 3

**Problem.** `GeminiProvider.embedBatch` hardcodes TaskType "RETRIEVAL_DOCUMENT" for every input (`gemini.go:105`); the comment at 87-88 says it is for stored and searched documents. The RAG query embed goes through the same `provider.Embed` (`search_cache.go:179`). Gemini's embedding models are asymmetric: documents should be embedded as `RETRIEVAL_DOCUMENT` and queries as `RETRIEVAL_QUERY`. Sending the document task type for queries lowers the similarity between a question and the passages that answer it, which pushes hits under the similarity floor. No code path sends `RETRIEVAL_QUERY`, and `embeddings_test.go:161` pins `RETRIEVAL_DOCUMENT`. The identity string (`gemini|model`) need not change, because the stored document vectors stay the same.

**Evidence.**
- `internal/ai/gemini.go:105` — `TaskType: "RETRIEVAL_DOCUMENT"` for every request item; the comment at 87-88 says it is for stored and searched documents
- `internal/ai/embeddings_test.go:161` — the test pins `RETRIEVAL_DOCUMENT` on the request
- `internal/rag/search_cache.go:179` — the query embed calls the same `provider.Embed`
- `internal/rag/search_cache.go:157` — query embeddings are cached under the embedding identity plus the query's SHA-256 (157-164)

**Change.** Add an embedding task hint on the context (`ai.WithEmbedTask(ctx, ai.EmbedTaskQuery)`). Search sets it in `embedQuery`; `compareSamples` and uploads leave it unset. `GeminiProvider` reads the hint and sends `RETRIEVAL_QUERY`, and the OpenAI-compatible family ignores it. `EmbeddingIdentity` stays unchanged. The in-process query-embedding cache is keyed by identity and query hash (`search_cache.go:157-164`), so query vectors cached under the document task type before the change keep being served until they expire. This is harmless; bump the cache `Param` if a test needs exact parity.

**Touches.** `internal/ai/gemini.go`, `internal/ai/embedding_identity.go`, `internal/rag/search_cache.go`, `internal/ai/embeddings_test.go`

**Done when.**
- The Gemini request from `embedQuery` carries `taskType` `RETRIEVAL_QUERY`, and the one from `AddDocument` carries `RETRIEVAL_DOCUMENT`.
- The identity-frozen test still passes (`gemini|model` unchanged).

### R-provider-28

**Catalogue credential fields other than key are dropped on save; no per-tenant endpoint for OpenAI-compatible gateways** · tier 3

**Problem.** The spec registry defines text, URL, select, number and JSON credential field types (`spec.go:55-62`). `CredentialForm` collects every non-key field into `extra` (`CredentialForm.tsx:113-119`), and `saveApiKey` spreads `extra` into the POST body (`providers.ts:75-76`). `handleSaveApiKey` decodes only `Provider` and `Key` (`handlers_system.go:262-265`), so any extra field is silently dropped. Nothing is lost today. Every spec with an `api_key` credential is the single-field `apiKeyCredential` (`spec.go:462-470`; `providers.golden:2-6` show `fields=key`). The only multi-field schemas, azure-foundry and local, are refused on this route with `USE_PROVIDER_CONFIG` (`handlers_system.go:275-283`). The loss is latent: the first provider that needs a base URL or region beside its key will appear to save and then fail to build. Fixing the handler is not enough. `ai.Credential` holds only `Key`, `OAuthToken` and `OAuth` (`spec.go:190-194`), and `SystemService.SaveApiKey` takes `(scope, provider, key)` (`handlers_system.go:284`), so no `Ctor` can receive extra fields. A separate gap is a new feature, not a regression. PAD teams often need the common enterprise setup: routing OpenAI or Anthropic traffic through a gateway (APIM, LiteLLM) per user or org. Today that works only through process-wide `*_API_URL` environment overrides read at init (`providerconfig.go:12-17`; `spec.go:162`, "a constant in the binary (an operator may override it by environment)"), which bypass the per-tenant model.

**Evidence.**
- `internal/ai/spec.go:55` — `FieldText`/`URL`/`Select`/`Number`/`JSON` field types (55-62); `apiKeyCredential` is single-field (462-470)
- `frontend/src/components/settings/providers/CredentialForm.tsx:113` — non-secret fields are collected into `extra` and posted (113-119)
- `frontend/src/api/providers.ts:75` — `saveApiKey` spreads `extra` into the body
- `internal/api/handlers_system.go:262` — the body struct has `Provider` and `Key` only; azure-foundry and local are refused with `USE_PROVIDER_CONFIG` (275-283); `SaveApiKey(ctx, scope, provider, key)` (284)
- `internal/ai/testdata/providers.golden:2` — claude, openai, gemini, xai, glm: `credential=api_key fields=key` (2-6)
- `internal/ai/spec.go:190` — `Credential` carries `Key`, `OAuthToken`, `OAuth` only (190-194)
- `internal/ai/providerconfig.go:12` — base URLs come only from the environment, read at init
- `internal/ai/spec.go:162` — `EgressFixedHost`: "a constant in the binary (an operator may override it by environment)"

**Change.** Split the item. Do now: make `/api/keys/save` validate the body against the spec's `CredentialSchema` (required fields, field types, URL host rules) and answer 400 for an undeclared field or a missing required one. Store a JSON credential document for a multi-field spec (the Azure document path is the model) and keep the bare string for single-field ones. Extend `ai.Credential` and the `SystemService.SaveApiKey` signature so a `Ctor` receives the extra fields. Defer the per-tenant `baseUrl` on the OpenAI-wire specs until a concrete gateway need exists. When it is built, guard it with a host allow-list that reuses the `PAD_AZURE_FOUNDRY_ALLOWED_HOSTS` semantics (`internal/config/loader.go:447`) and the dial-time egress guard.

**Touches.** `internal/api/handlers_system.go`, `internal/service/system.go`, `internal/ai/spec.go`, `internal/ai/factory.go`, `internal/ai/openai.go`, `frontend/src/components/settings/providers/CredentialForm.tsx`

**Done when.**
- A test spec with fields `key` and `region`: `POST /api/keys/save {provider, key, region}` stores both and the `Ctor` receives `region`. A missing required `region` answers 400 naming the field, and an undeclared field answers 400.
- Deferred gateway part: with an allow-listed `baseUrl` saved for openai, the chain dials that host. A host not on the list is refused at save time with `HOST_NOT_ALLOWED`.

### R-provider-29

**Foundry agents' hosted tools are an ungoverned egress path for scrubbed flow context** · tier 3

**Problem.** An Azure AI Foundry agent entry always runs the tool loop (`chat.go:897-898`). The scrubbed flow context reaches the agent through the request messages (`chat.go:615-617`); the tool loop only adds the results of baki's own function tools. The Responses run may also execute the agent's own hosted tools. `replayableItemTypes` accepts `web_search_call` and the other hosted items as normal run items (`responses.go:223-232`), and `hostedToolLabels` lists web search, file search, code interpreter, MCP and the rest, but only as trail labels (`responses.go:1166-1179`). `ToolGate` governs only baki's function tools (`tools.go:84-87`). `ToolEvent` chunks from hosted tools are forwarded to the trail and otherwise ignored (`chat.go:1233-1235`). The org registry is already an allow-list of agents: `ResolveChat` lets members use only entries registered in the caller's or the org's registry, and anything else is `ErrModelNotAvailable` (`azure_models_runtime.go:104-120`, `136-145`). What is missing is visibility of the hosted tool types an agent may run and an audit record of the ones that did. Nothing refuses or warns when a run calls an external tool. Baki also tells users to turn MCP approval off (`responses.go:978`, `docs/ai-foundry.md:9`). Injected flow content can therefore make an agent send flow details to a third party through its own tools, outside the browser CSP and outside baki's function-tool approvals.

**Evidence.**
- `internal/service/chat.go:615` — the scrubbed context and guidelines reach the provider in the request messages (`buildScopedMessages`, 615-617)
- `internal/service/chat.go:898` — `if req.UseTools || ts.model.agent()`: an agent turn always runs the tool loop (897-901), which adds only baki's function-tool results
- `internal/ai/responses.go:223` — `replayableItemTypes` accepts `web_search_call` and other hosted items as normal run items (223-232)
- `internal/ai/responses.go:1166` — `hostedToolLabels` lists web search, file search, code interpreter, MCP and more; trail labels only, no policy
- `internal/ai/tools.go:84` — `ToolGate` governs only baki's function tools; the comment names an agent's own `function_call` as what it catches (84-87)
- `internal/service/chat.go:1233` — hosted-tool `ToolEvent` chunks go to the trail (`hostedStep`, then the trail emit) and are otherwise ignored
- `internal/service/azure_models_runtime.go:104` — only entries in the caller's or the org's registry resolve; otherwise `ErrModelNotAvailable` (104-120, 136-145)
- `internal/ai/responses.go:978` — `mcpApprovalError` tells the user to set `require_approval` to "never"; `docs/ai-foundry.md:9` says the same

**Change.** Registering the agent is the opt-in, and its tools are governed in Foundry, so add no per-entry opt-in and no run refusal. Record the hosted tool types that ran in the chat turn's audit meta. These are the `ToolEvent` names, which are the output item types (`responses.go:1437`). The existing chat audit entry is written when the stream is accepted (`internal/api/handlers_chat.go:86-88`), before any hosted tool runs, so the types need a record written when the turn ends. In `docs/ai-foundry.md`, next to the existing agent notes (line 9), add a caveat that flow context sent to an agent can leave through the agent's own tools.

**Touches.** `internal/service/chat.go`, `internal/ai/responses.go`, `docs/ai-foundry.md`

**Done when.**
- The audit meta of a chat turn on an agent lists the hosted tool types that ran.
- `docs/ai-foundry.md` states that flow context sent to an agent can leave through the agent's own tools.

### R-provider-30

**A spec Resolver silently bypasses the member/org credential rule; nothing forbids Resolver together with CapOrgShared** · tier 3

**Problem.** `resolveCredential` collapses the rule for a spec without `CapOrgShared` (`factory.go:174-179`). It then returns `spec.Resolver(f, scope, orgID)` before the `memberFirst`/`orgFirst`/`orgOnly` rule is applied (180-182). This is safe today only because the sole Resolver provider, Copilot, lacks `CapOrgShared` (`spec_copilot.go:18`), and its resolver reads only the member's keys (45-55). The spec invariants tie an `OAuthToken` key to a Resolver (`spec_test.go:131-133`) but do not tie a Resolver to `!CapOrgShared`. The Resolver doc says "nil reads Keys.Primary under the builder's member/org rule", but the rule is never passed to a Resolver (`spec.go:231-233`). A future device-flow provider that is also org-shareable would therefore ignore `ForOrgCredential`/`ForEmbeddingOrg` semantics. An admin's org-only connection test would run on the member's token, and an org corpus could be embedded with a member's credential.

**Evidence.**
- `internal/ai/factory.go:174` — `!CapOrgShared` collapses the rule (174-179); then `if spec.Resolver != nil { return spec.Resolver(f, scope, orgID) }` at 180-182 ignores the rule
- `internal/ai/spec_copilot.go:18` — Copilot `Caps` omit `CapOrgShared`; its resolver (45-55) reads only the member's keys
- `internal/ai/spec_test.go:131` — the only Resolver invariant: an `OAuthToken` key goes with a Resolver, and the reverse
- `internal/ai/spec.go:231` — Resolver doc: "nil reads Keys.Primary under the builder's member/org rule"; the rule is not passed to a Resolver

**Change.** Choose one of two fixes. Either add the spec invariant `Resolver != nil ⇒ !CapOrgShared` to `TestProviderSpecs_Complete` (`spec_test.go:23`) and `mustRegisterSpec` (`spec.go:258`). Or change the `Resolver` signature to receive the `credentialRule` and make the factory's `orgOnly`/`orgFirst` semantics binding for it. Document the choice under "Adding a provider" in `docs/architecture/ai-providers.md` (line 9).

**Touches.** `internal/ai/spec.go`, `internal/ai/spec_test.go`, `internal/ai/factory.go`, `docs/architecture/ai-providers.md`

**Done when.**
- `mustRegisterSpec` panics, and `TestProviderSpecs_Complete` fails, for a spec with both a Resolver and `CapOrgShared`. Under the signature variant: `ForOrgCredential` on a Resolver provider with no org credential returns `ErrKeyNotConfigured`.
- The existing Copilot factory tests (PAT and OAuth per-user scope, org ignored) keep passing.

### R-provider-31

**Memo maps are wiped wholesale at their cap (learnedRepairs, streamRefusals, agentKinds, responsesToolRoutes) and servedMemo evicts in random order** · tier 3

**Problem.** Four memos reset completely at their bound: `learnedRepairs` (`openai_compat.go:387-390`), `streamRefusals` (`:855-857`), `responsesToolRoutes` (`responses.go:763-765`) and `agentKinds` (`azure_agents.go:545-547`). When one reaches its bound, the whole map is replaced by an empty one. `servedMemo` and `learnedWindows` first drop expired entries, then delete arbitrary keys in map order until there is room (`served_model.go:108-121`; `learned_window.go:37-49`). The wholesale resets are a documented, deliberate trade-off ("Cheap to repopulate", `openai_compat.go:388`; `served_model.go:114-115`). Each costs one rejection round trip per deployment. A burst of distinct endpoint+model keys past the cap (many tenants × deployments) resets every learned repair at once. Every active deployment then pays its rejection round trip again in the same window. The burst needs more than 1024 distinct keys within the 1 h TTL (`openai_compat.go:352-353`). The concrete defect is in `rememberRepairs`, which overwrites a key with only the current request's repaired set (`openai_compat.go:379-391`). A request that omitted `temperature` forgets a learned `temperature` repair. A request without tools that learns another repair drops a learned `reasoning_effort_none`, because that repair is applied, and so recorded again, only when the request has tools (`openai_compat.go:220-226`). This is tier 3.

**Evidence.**
- `internal/ai/openai_compat.go:387` — `learnedRepairs`: "start over" with a fresh map at the cap (387-390); "Cheap to repopulate (one rejection per deployment)" (388)
- `internal/ai/openai_compat.go:352` — `learnedRepairsMax = 1024`, `learnedRepairsTTL = time.Hour` (352-353)
- `internal/ai/openai_compat.go:855` — `streamRefusals`: same wholesale reset (855-857)
- `internal/ai/azure_agents.go:545` — `agentKinds`: same (545-547)
- `internal/ai/responses.go:763` — `responsesToolRoutes`: same (763-765)
- `internal/ai/served_model.go:108` — `servedMemo`: expire, then delete in map order until there is room (116-121), "random map order, acceptable" (114-115); `learned_window.go:37-49` the same
- `internal/ai/openai_compat.go:379` — `rememberRepairs` overwrites the entry with this request's repaired set (391)
- `internal/ai/openai_compat.go:220` — the learned `reasoning_effort_none` repair is applied only when the request has tools (220-226)
- `internal/archtest/state_test.go:69` — the `learnedWindows`, `servedMemo` and `servedByResource` rows (69-72)

**Change.** Move the six memos onto package-level `cache.Declare` specs built with `cache.New`: replica-local, with `WithMaxEntries` plus `WithTTL` (`core/cache/spec.go:121`, `core/cache/typed.go:35`, `core/cache/sized.go:66`, `:78`). Refresh `TestCacheRegistry_Golden`. Add their state rows in `internal/archtest/state_test.go` next to `servedMemo`/`learnedWindows` (`:69-72`). In `rememberRepairs`, merge instead of overwriting. Take the union with the live entry for the key, or carry into the stored set the `learned` entries whose parameter was absent from this request.

**Touches.** `internal/ai/openai_compat.go`, `internal/ai/responses.go`, `internal/ai/azure_agents.go`, `internal/ai/served_model.go`, `internal/ai/learned_window.go`, `internal/ai/openai_compat_repairs_test.go`, `internal/archtest/state_test.go`, `internal/di/testdata/caches.golden`

**Done when.**
- Inserting `learnedRepairsMax+1` keys keeps the newest entry and the `learnedRepairsMax-1` entries before it, and drops only the oldest.
- A deployment that learned `temperature` keeps it after a later request to the same key learns `max_tokens` without sending `temperature`.

### R-provider-c5

**Nested withTokenRetry on the Responses reroute replays the whole turn a second time, beyond the documented single token replay** · tier 3

**Problem.** `Stream` wraps `p.stream` in `withTokenRetry` (`azure_foundry.go:382`). When chat completions rejects tools with reasoning, `streamInner` calls the reroute hook (`openai_compat.go:758-764`). The hook wraps `modelStream` in a second `withTokenRetry` (`azure_foundry.go:119-125`). Each level installs its own `entraTokenUse` recorder (`:177`, `:193`), so the outer recorder only ever sees the token the chat-completions request used. Take a /responses route that keeps rejecting the token with `ErrEntraTokenRejected`, which unwraps to `ErrApiKeyInvalid` and not to `errEntraToken` (`azure_auth_errors.go:24-26`). The inner level invalidates the token, fetches a fresh one and replays `modelStream`. When that fails too, the outer level calls `Invalidate` on a token the source no longer holds, which is a no-op (`azure_entra.go:291-296`). It then gets the inner level's fresh token, sees `fresh != failed` (`azure_foundry.go:187-194`), and replays `p.stream` in full. The replayed closure does not consult the route memo: that check sits outside it (`azure_foundry.go:373-380`), and `modelStream` records the route only on a 200 (`responses.go:703-705`). So the replay sends the chat-completions request again and is rejected again. It then enters a second inner `withTokenRetry`, which invalidates the token the source now holds and clears the shared token a second time. One turn therefore sends two chat-completions requests and more than two /responses requests. That goes beyond the single token replay that `withTokenRetry` documents (`azure_foundry.go:174-175`).

**Evidence.**
- `internal/ai/azure_foundry.go:382` — `Stream`: `p.withTokenRetry(ctx, ... p.stream ...)`
- `internal/ai/openai_compat.go:758` — `streamInner` calls `b.rerouteToolsRejection` on the tools-with-reasoning 400
- `internal/ai/azure_foundry.go:123` — the hook wraps `modelStream` in a second `p.withTokenRetry`
- `internal/ai/azure_foundry.go:177` — each `withTokenRetry` installs its own recorder (also at 193); the outer one never sees the /responses token
- `internal/ai/azure_foundry.go:187` — the outer level calls `Invalidate(failed)` then `Token()`; `fresh != failed` triggers a full replay of `fn` (`p.stream`)
- `internal/ai/azure_entra.go:291` — `Invalidate` is a no-op when the source's token is not the failed one, but clears the shared token when it is
- `internal/ai/azure_foundry.go:373` — the route-memo check runs before the closure, not inside it (373-380)
- `internal/ai/responses.go:703` — `modelStream` remembers the route only on a 200 (703-705)
- `internal/ai/azure_auth_errors.go:24` — `entraTokenRejectedError` unwraps to `ErrEntraTokenRejected` and `ErrApiKeyInvalid` (24-26)

**Change.** Make `withTokenRetry` re-entrant. If ctx already carries an active retry marker, run `fn` once and let the outermost level own the replay. Have the recorder chain to its parent, so the outer level records the token the inner call actually sent. Make the replayed closure re-read the route memo. Record the chat-completions rejection as proof that the deployment needs /responses (a pending route state), so the outer replay goes straight to /responses instead of resending the chat-completions request.

**Touches.** `internal/ai/azure_foundry.go`, `internal/ai/responses.go`, `internal/ai/reasoning_models_test.go`, `internal/ai/azure_entra_test.go`

**Done when.**
- Chat/completions returns the gpt-5.6 tools rejection, and `/openai/v1/responses` always returns 401 under Entra ID. `Stream(toolsRequest)` returns `ErrEntraTokenRejected` after at most 1 chat-completions request and 2 /responses requests.
- In that test, the token source's `Invalidate` clears a cached token at most once per turn.
- When the first /responses 401 comes from a rotated key and the fresh token succeeds, the turn completes with exactly 1 chat-completions request and 2 /responses requests (unchanged behaviour).
