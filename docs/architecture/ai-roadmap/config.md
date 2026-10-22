# AI roadmap: Configuration and credentials

Items of category `config` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-config-2

**The ai settings section is stored unvalidated (negative/NaN budgets, unknown provider ids, unbounded prompt suffix) and the user settings route has no body cap of its own** · tier 2

**Problem.** `UpdateUserSettings` runs only `validateProfileSection` before the write (`internal/service/system.go:180`), and `UpdateSettings` persists whatever it is given (`system.go:98-108`). Every `ai.*` field is therefore stored as received: activeProvider, embeddingProvider, embeddingModel, dailyBudget, perUserDailyBudget, the per-provider temperature, maxTokens, contextTokenBudget and reasoningEffort, systemPromptSuffix and prompts. The model has plain float64/string fields with no constraints (`core/models/settings.go:89-109`). The client does not cap the suffix either: the textarea has no `maxLength` (`frontend/src/components/settings/AIBehaviorPanel.tsx:170-179`), and `frontend/src/lib/constraints.ts` holds no ai caps. On the desktop and in local mode, the route writes the deployment document (`system.go:183-191`). There `AIBudget.check` treats `DailyBudget <= 0` as unlimited and applies `PerUserDailyBudget` only when it is above 0 (`ai_budget.go:204`, `228`), so -5 behaves exactly like 0, a value the operator may set anyway. In cloud the member route writes the user's own document (`system.go:193-200`), which `AIBudget` does not read (`ai_budget.go:203`). `handleUpdateSettings` uses `decodeBody` with no `MaxBytesReader` (`handlers_system.go:93-97`), while the org route caps at 1 MiB (`handlers_system.go:153`). The router's global 10 MiB cap (`router.go:199`) is the only bound. `systemPromptSuffix` is only `TrimSpace`'d (`chat_context.go:654`) before it is concatenated onto every system prompt with no length cap (`ai/context.go:42-43`). `handlers_system.go:108` already maps `ErrSettingsInvalid` to 400, so the plumbing exists.

**Evidence.**
- `internal/service/system.go:180` — only `validateProfileSection` runs before the write; `UpdateSettings` (98-108) has no validation at all
- `internal/service/ai_budget.go:204` — `DailyBudget <= 0` means unlimited; `PerUserDailyBudget` applies only when > 0 (228)
- `core/models/settings.go:89` — `AISettings` fields are plain float64/string with no server-side constraints (89-109)
- `internal/api/handlers_system.go:93` — user route: `decodeBody`, no `MaxBytesReader` (93-97); the org route caps at 1 MiB (153); `ErrSettingsInvalid` → 400 is already wired (108-110)
- `internal/api/router.go:199` — the global 10 MiB `MaxBytesReader` is the only bound on the user settings body
- `internal/service/chat_context.go:654` — `systemPromptSuffix` is only `TrimSpace`'d; it is concatenated onto the system prompt uncapped at `internal/ai/context.go:42-43`

**Change.** Two changes are needed. Put a 1 MiB `MaxBytesReader` on the user route, as on the org route. Add a rune cap on `systemPromptSuffix` and on each prompt, plus a count cap on the prompt lists. Check them in a new `validateAISection` next to `validateProfileSection`, reject with `ErrSettingsInvalid` naming the field, and mirror the caps in `frontend/src/lib/constraints.ts`. Storage-time checks of the per-turn parameters are optional consistency hardening, not a fix for a silent bypass, because those values are already refused or clamped at use. The checks would be:
- activeProvider and embeddingProvider must be known ids (`ai.KnownProvider` / `SupportsEmbeddings`).
- Temperature must be within 0..2, and maxTokens and contextTokenBudget within the client's bounds.
- reasoningEffort must be one of {"", low, medium, high}.

At use today, `reasoningEffort` is refused (`internal/service/chat.go:694`), temperature and maxTokens are clamped (`chat.go:138-154`), and an unsupported embeddingProvider fails fast (`internal/rag/service.go:277-279`). Budget validation (finite, >= 0, PerUser <= Daily when both are above 0) belongs on the admin writer from along with logging and auditing budget changes (old → new). `encoding/json` already rejects NaN and out-of-range numbers.

The tier-0 pass (2026-10-22) moved part of this behind the deployment writer: `validateDeploymentAISettings` (`internal/service/system.go`) checks the budgets (finite, >= 0), the embedding provider (`ai.SupportsEmbeddings`, and not an alias-model provider such as Azure AI Foundry, which embeds only under the operator's `PAD_EMBEDDING_PROVIDER` pin) and the model length for `PUT /api/admin/settings/ai`, and the caller's context budget is clamped at use to `service.MaxContextTokenBudget` (mirrored by `CONTEXT_TOKEN_BUDGET_MAX` in `frontend/src/lib/constraints.ts`). Two pieces were left here on purpose: the member Settings panel (`frontend/src/components/settings/AIBehaviorPanel.tsx`) still offers the embedding provider and model in cloud mode, where a member's write lands in their own document and the indexer never reads it, so those controls should point at the admin writer or disappear there; and `validateAISection` should reuse the deployment checks for the fields the member route keeps storing. `BackendSettingsProvider.UpdateWith` re-reads the document inside the request's transaction but holds no row lock between the read and the write (its comment says so); a section-level SQL write is the fix if two admin writers ever race.

**Touches.** `internal/service/system.go`, `internal/service/settings_validation.go` (new), `internal/api/handlers_system.go`, `frontend/src/lib/constraints.ts`, `internal/service/system_test.go`

**Done when.**
- `POST /api/system/settings` with a 64 KiB `systemPromptSuffix`, or a prompt list over its cap, answers 400 with `ErrSettingsInvalid` copy naming the field, and the stored value is unchanged.
- A 2 MiB user settings body is refused like the org route's.
- A test asserts that the server caps equal the constraints the frontend exports.

### R-config-3

**/api/keys/save stores an empty or arbitrary key with no server-side contract, producing a provider that looks configured and never works** · tier 2

**Problem.** `handleSaveApiKey` decodes `{provider, key}` with `decodeBody`. That is a plain `json.Decode` with no `DisallowUnknownFields` and no byte cap of its own beyond the router's 10 MiB (`internal/api/handlers_system.go:261-268`, `handler_helpers.go:22-23`). The handler checks only that the provider id is known and is not azure-foundry or local (269-282). It passes the key straight to `SaveApiKey` (284), which calls `secrets.Save` with no emptiness, whitespace, length or charset check (`internal/service/system.go:344-346`). The keystore upserts the ciphertext of any string, "" included (`keystore.go:112-123`). `Has` is a row-existence query (`keystore.go:156-166`), so `isConfigured` reports the provider as configured in the catalogue and listing (`providers.go:645-651`). `resolveKey`, by contrast, treats "" as absent (`factory.go:109`). With no org key, every turn fails with `ErrKeyNotConfigured` (`factory.go:121`). When the org holds a shared credential, `resolveKey` falls through to it (`factory.go:112-119`) and chat still works. The listing does not: `isConfigured` is true, so `resolveListing` picks the personal scope first (`providers.go:676-679`), and the listing and the chat path disagree. The frontend's `canSave` guard is the only barrier. Any other client, or a direct call, can produce a provider that looks configured and, without an org key, never works.

**Evidence.**
- `internal/api/handlers_system.go:261` — decodes Provider and Key only; validates the provider id (269-282) and nothing about the key before `SaveApiKey` (284)
- `internal/api/handler_helpers.go:22` — `decodeBody`: `json.NewDecoder(r.Body).Decode`, no `DisallowUnknownFields`, no byte cap
- `internal/service/system.go:344` — `SaveApiKey` persists whatever arrives through `s.secrets.Save` (346)
- `internal/storage/database/keystore.go:112` — `Save` upserts the ciphertext of any string, "" included (112-123); `Has` is `SELECT EXISTS` on the row (156-166)
- `internal/service/providers.go:650` — `isConfigured` uses `secrets.Has`, which is true for an empty row
- `internal/ai/factory.go:109` — `resolveKey`: `key == ""` falls through; with no org key it ends in `ErrKeyNotConfigured` (121)

**Change.** Keep it small. In the service, trim the key, refuse an empty one, cap its length (for example 8 KiB) and refuse control characters, answering 400 with code `KEY_INVALID`. Add a `MaxBytesReader` on the route. If unknown body fields are refused, the allow-list must accept the catalogue's declared credential field names. The client spreads them into the body (`{...extra, provider, key}`, `frontend/src/api/providers.ts:75-76`; `frontend/src/components/settings/providers/CredentialForm.tsx:113-119`). Every spec that reaches this route declares one field today (`internal/ai/spec.go:467`, `internal/ai/spec_copilot.go:23`). A cheap optional per-spec format check (prefix hints in `CredentialField`) can follow. The harm is limited to the caller's own key scope (`h.security.KeyScope(r)` at `handlers_system.go:284`), so this belongs in tier 2 or 3, not tier 1.

**Touches.** `internal/api/handlers_system.go`, `internal/service/system.go`, `internal/api/handlers_system_test.go`

**Done when.**
- `POST /api/keys/save` with key `""` or `"   "` answers 400 `KEY_INVALID`, and `Has` stays false.
- A 1 MiB key answers 413/400 and nothing is stored.
- A body with an unknown field, other than the catalogue's declared credential fields, answers 400.

### R-config-5

**Resilience knobs are unbounded: PAD_RETRY_MAX_ATTEMPTS and PAD_CB_FAILURES accept any integer, the durations have no ceiling, and none is documented outside .env.example** · tier 2

**Problem.** `PAD_CB_FAILURES` and `PAD_RETRY_MAX_ATTEMPTS` are bound with `intEnv` and never range-checked (`internal/config/loader.go:418`, `420`). The fields are bare ints with defaults 5 and 3 (`config.go:383-385`, `438-441`), and grep finds no other validation site. `validateDurations` checks `PAD_CB_OPEN_DURATION` and `PAD_RETRY_BASE_DELAY` for positivity only, with `max=0` (`loader.go:788-789`). `ResilienceConfig.withDefaults` silently replaces values <= 0 and passes any positive value through (`internal/ai/factory.go:61-75`). `cappedBackoff` honours a base delay above the 30 s ceiling (`retry.go:53-59`). `retry.go:18-21` documents that attempts are "operator-configurable with no bound"; the 30 s cap exists only to survive the resulting overflow. The effects of bad values:
- `PAD_RETRY_MAX_ATTEMPTS=50` turns every provider outage into up to 49 retries per call, each up to 30 s. That inflates `pad_ai_retry_attempts_total` and holds a stream's 10 min wall budget in backoff; `retry_observability_test.go:28-29` names this misconfiguration.
- `PAD_CB_FAILURES=1` trips the shared circuit on any single 5xx, and 10000 silently disables the breaker.
- `PAD_RETRY_BASE_DELAY=10m` parks chat turns past the idle watchdog.

The only loader test rejects a non-integer, not an out-of-range value (`loader_test.go:307-308`). The only operator-facing description is `.env.example:490-497`; grep over `docs/` for `PAD_CB_` or `PAD_RETRY_` finds nothing.

**Evidence.**
- `internal/config/loader.go:418` — `intEnv` bindings for `PAD_CB_FAILURES` (418) and `PAD_RETRY_MAX_ATTEMPTS` (420) with no range check; fields and defaults at `config.go:383-385`, `438-441`
- `internal/config/loader.go:788` — `validateDurations` rows for `PAD_CB_OPEN_DURATION` and `PAD_RETRY_BASE_DELAY`: `positive=true`, `max=0` (no ceiling); there is no integer counterpart
- `internal/ai/factory.go:61` — `withDefaults` replaces only Threshold/OpenFor/Attempts/BaseDelay <= 0 (62-73); any positive value passes through
- `internal/ai/retry.go:18` — "retryMaxAttempts is operator-configurable with no bound" is the overflow guard; `cappedBackoff` (53-59) uses `max(retryMaxDelay, baseDelay)`, so a huge base is honoured
- `internal/config/loader_test.go:307` — the only `PAD_CB_FAILURES` / `PAD_RETRY_MAX_ATTEMPTS` tests reject a non-integer, not a range (307-308); `retry_observability_test.go:28-29` names the unbounded misconfiguration
- `.env.example:490` — the only operator-facing description (490-497); grep over `docs/` for `PAD_CB_` or `PAD_RETRY_` finds nothing

**Change.** Add a `validateInts` table beside `validateDurations`, which already names both the env var and the config field. Bound `PAD_RETRY_MAX_ATTEMPTS` to [1, 10] and `PAD_CB_FAILURES` to [1, 100]. The error names the env var and the config field, and the check applies to both env and config-file sources. Add ceilings through `durationMaxReasons`: `PAD_RETRY_BASE_DELAY` <= 30 s and `PAD_CB_OPEN_DURATION` <= 10 min. Document all four knobs with their defaults, bounds and the retry/breaker semantics in `docs/architecture/operations.md` and `.env.example`. This only bites under explicit operator misconfiguration, and the defaults are sane. Backoff also partly limits itself. It gives up with the real error when the caller's deadline cannot cover the wait (`retry.go:86-88`). A Retry-After above 20 s is reported at once (`retry.go:78-82`). Tier 2 or 3.

**Touches.** `internal/config/loader.go`, `internal/config/config.go`, `internal/config/loader_test.go`, `internal/config/load_test.go`, `docs/architecture/operations.md`, `.env.example`

**Done when.**
- Config load fails with a message naming the variable and field for each of: `PAD_RETRY_MAX_ATTEMPTS=0` or `=11`, `PAD_CB_FAILURES=0` or `=101`, `PAD_RETRY_BASE_DELAY=31s`, `PAD_CB_OPEN_DURATION=11m`. A config file with `{"Runtime":{"RetryMaxAttempts":0}}` fails the same way.
- The existing defaults (3 attempts, 5 failures, 500ms, 30s) still boot, and `TestProviderFactory_ResilienceIsAppliedToEveryChain` is unchanged.
- `docs/architecture/operations.md` contains each knob's name, default and bound, checked by a doc test or a grep in CI.

### R-config-4

**Org settings writes are a whole-document read-modify-write without a version token; concurrent ai.* edits lose one write, including the fail-closed provider policy** · tier 3

**Problem.** `OrgAIProvidersSection` GETs the org document (`noDedup`) and POSTs it whole with only `ai.allowedProviders` changed (`frontend/src/components/settings/OrgAIProvidersSection.tsx:170-174`). `handleUpdateOrgSettings` has no `If-Match` or `updatedAt` precondition (`internal/api/handlers_system.go:148-163`). `UpdateOrgSettings` reads the stored document outside the write's lock and then replaces a named `ai` section whole (`internal/service/system.go:271-285`). The service admits this lost-update race in the comment on `UpdateUserSettings` (`system.go:174-177`); `UpdateOrgSettings` has the same unlocked read but no comment saying so. When two admins edit `ai.*` at the same time, or an admin and the Rules panel in another tab both save, one write is lost. The lost write can be the fail-closed allowed-provider list. ADR 0012 records a section-level write API as the follow-up (`docs/adr/0012-settings-sections-with-versions.md:19`).

**Evidence.**
- `frontend/src/components/settings/OrgAIProvidersSection.tsx:170` — GET (`noDedup`), then POST of the whole document with only the list changed (170-174); the rolling-deploy reason is in the comment (171-173)
- `internal/api/handlers_system.go:148` — the org settings write carries no version or CAS token (148-163)
- `internal/service/system.go:174` — the lost-update comment ("the stored document is read before the write, not under its lock") is on `UpdateUserSettings` (174-177)
- `internal/service/system.go:271` — `UpdateOrgSettings` calls `LoadOrgSettings` outside the save and keeps the sections the write does not name (271-285); `allowedProviders` survives a write that omits it only because of its special case (279-284)
- `internal/storage/database/postgres_settings.go:140` — `saveSettingsDoc`; the section rows are locked with `SELECT … FOR UPDATE` (88-91)
- `docs/adr/0012-settings-sections-with-versions.md:12` — sections merge, but "a known field follows the write (absent means cleared)"
- `docs/adr/0012-settings-sections-with-versions.md:19` — "Two concurrent saves of different sections can still lose one; a section-level write API is the follow-up"

**Change.** Tier 3. Add a section-level write: either `PATCH /api/system/settings/org/{id}/sections/{section}`, or a body `{ai:{allowedProviders}}` with an `updatedAt`/ETag precondition. The merge runs under the `settings_sections` row lock that `saveSettingsDoc` already takes (`postgres_settings.go:88-91`), and a stale precondition answers 409. A section-level lock alone does not let two writes to different fields of `ai` both land. Sections are replaced whole (ADR 0012:12), and `allowedProviders` survives today only through its special case (`system.go:279-284`), so the fix also needs either field-level merging inside `ai` or the precondition. Move `OrgAIProvidersSection` and the Rules panel onto the new write only after the rolling-deploy constraint in the component comment (`OrgAIProvidersSection.tsx:171-173`) has passed.

**Touches.** `internal/api/handlers_system.go`, `internal/service/system.go`, `internal/storage/database/postgres_settings.go`, `frontend/src/components/settings/OrgAIProvidersSection.tsx`, `frontend/src/api/settings.ts`

**Done when.**
- Two concurrent org writes, one naming `ai.allowedProviders` and one naming `ai.dailyBudget`, both land and neither field reverts.
- A write with a stale precondition answers 409, and the UI reloads before retrying.

**ADR.** specified as follow-up in ADR-0012, not implemented

### R-config-6

**Package-init env reads (PAD_AI_MAX_CONNS_PER_HOST, *_API_URL) bypass config validation, are pinned archtest exceptions and are undocumented** · tier 3

**Problem.** `internal/ai/httpclient.go` reads `PAD_AI_MAX_CONNS_PER_HOST` at var init and silently falls back to 256 when the value is not an integer or is negative (`httpclient.go:17-23`). `providerURL` in `internal/ai/providerconfig.go` reads eight provider base-URL variables at package init and validates none of them as URLs (`providerconfig.go:12-17`). Seven are listed in its comment (`CLAUDE_API_URL`, `OPENAI_API_URL`, `GEMINI_API_URL`, `GLM_API_URL`, `XAI_API_URL`, `GITHUB_MODELS_API_URL`, `COPILOT_API_URL`; `providerconfig.go:10-11`); the eighth, `COPILOT_MODELS_API_URL` (`internal/ai/copilot.go:13`), is missing from it. Both files are pinned `os.Getenv` exceptions in the archtest env ratchet (`internal/archtest/archtest_test.go:402-405`), and neither variable is bound next to the Runtime resilience knobs (`internal/config/loader.go:417-426`). Neither appears in `.env.example`, where a grep finds no `API_URL` or `MAX_CONNS_PER_HOST` entry. In `docs/`, the only hits are `VITE_API_URL` (`docs/deployment.md:66`) and a design note in ADR 0025 that the fixed-host `*_API_URL` overrides bypass every egress guard (`docs/adr/0025-provider-specs.md:21`); neither is operator documentation of these knobs. The variables are therefore invisible to the config dump path the rest of Runtime goes through. An operator who redirects a provider to a proxy, which is the documented purpose, has no documented knob, and a typo goes unreported.

**Evidence.**
- `internal/ai/httpclient.go:17` — `maxConnsPerHostFromEnv`: a strconv error or `n < 0` silently returns 256 (18-23)
- `internal/ai/providerconfig.go:12` — `providerURL` reads `envKey` at package init with no validation (12-17); the comment lists seven variables (10-11)
- `internal/ai/copilot.go:13` — `COPILOT_MODELS_API_URL`, the eighth variable, missing from the comment
- `internal/archtest/archtest_test.go:402` — both files pinned as `os.Getenv` exceptions (403-404)
- `internal/config/loader.go:417` — the Runtime resilience knobs are bound here (417-426); the two AI env reads are not
- `.env.example:490` — documents `PAD_CB_*`/`PAD_RETRY_*` (490-497) but has no `API_URL` or `MAX_CONNS_PER_HOST` entry
- `docs/adr/0025-provider-specs.md:21` — "fixed-host `*_API_URL` overrides bypass every egress guard (operator-controlled)"
- `internal/di/services.go:157` — `ai.ConfigureProviders(ai.ProviderOptions{…})` already exists at boot (157-161); the type is at `internal/ai/spec.go:348`

**Change.** Tier 3. Move both reads into `config.Runtime`: `AIMaxConnsPerHost int` with a range check, and `AIProviderBaseURLs map[string]string` covering all eight variables, each validated as an absolute http(s) URL. Do not add an egress-policy check. ADR 0025 records the bypass as deliberate (`docs/adr/0025-provider-specs.md:21`), so an egress check would be a policy change, to be decided explicitly by amending ADR 0025. Pass the values through the existing `ai.ProviderOptions` (`internal/ai/spec.go:348`, set up at `internal/di/services.go:157-161`), and have provider constructors read the override from the options instead of a package var. Reduce the two archtest pins to an empty map. Document the knobs in `.env.example`, `docs/architecture/operations.md` and `docs/architecture/ai-providers.md`.

**Touches.** `internal/ai/httpclient.go`, `internal/ai/providerconfig.go`, `internal/ai/spec.go`, `internal/config/config.go`, `internal/config/loader.go`, `internal/di/services.go`, `internal/archtest/archtest_test.go`, `.env.example`, `docs/architecture/operations.md`, `docs/architecture/ai-providers.md`

**Done when.**
- `TestArch_EnvReadsOnlyInConfig` passes with an empty pin map.
- `PAD_AI_MAX_CONNS_PER_HOST=abc` fails config load and names the variable; `=0` disables the cap; the transport's `MaxConnsPerHost` reflects the configured value.
- `OPENAI_API_URL=ftp://x` or a non-absolute URL fails config load; a valid override is used by the OpenAI provider in an httptest-backed test.

### R-config-9

**Flat 4000-token context budget regardless of the model window, with mixed token estimators between the inclusion decision and the final cut** · tier 3

**Problem.** The per-provider context budget defaults to 4000 tokens whatever the model's window is (`internal/service/chat_context.go:337-345`); `DefaultSettings` sets `ContextTokenBudget: 4000` on every provider row (`core/models/settings.go:195-245`). The budget can be changed per provider in Settings (`frontend/src/components/settings/AIBehaviorPanel.tsx:232-238`), so the defect is the flat default and the missing window-derived cap. The real window from `ContextLimitFor` (`internal/ai/provider.go:388-401`) already drives output sizing (`internal/service/chat.go:140`), history truncation (`chat.go:629`), the tool-loop limit (`chat.go:1074`) and the context preview (`chat_context.go:646`), but it never sets the context budget. By default, a model with a 200k or 1M window gets the same 4k slice of a large PAD flow as an 8k model. Selected source files are also capped at 16 bytes per budget token (64 KB at the default) before anything else runs (`internal/ai/context.go:164-167`, `173-175`). Inside `BuildContext`, the provider's `EstimateTokens` decides whether the Flow Overview fits under `TokenBudget/2` (`context.go:150`), while the final cut uses `TruncateToTokenLimit` (`context.go:156`, `internal/ai/tokens.go:99-115`). This is a consistency nit, not a live mismatch: every real provider counts with the same cl100k tokenizer, and the two functions differ only in their fallback when the tokenizer is unavailable (`tokens.go:79` against `tokens.go:118`).

**Evidence.**
- `internal/service/chat_context.go:337` — `tokenBudget := 4000` unless `settings.AI.Providers[id].ContextTokenBudget > 0` (337-345)
- `core/models/settings.go:195` — `DefaultSettings` sets `ContextTokenBudget: 4000` for every provider row (195-245)
- `frontend/src/components/settings/AIBehaviorPanel.tsx:232` — the per-provider budget field, fallback 4000 (232-238)
- `internal/ai/provider.go:388` — `ContextLimitFor` knows the per-model window (388-401)
- `internal/service/chat.go:140` — the window drives output sizing; also used for history truncation (:629), the tool-loop limit (:1074) and the preview (`chat_context.go:646`), never for the context budget
- `internal/ai/context.go:150` — Flow Overview gated by `req.Provider.EstimateTokens(...) < req.TokenBudget/2`
- `internal/ai/context.go:156` — final cut by `TruncateToTokenLimit` (cl100k, `tokens.go:99-115`)
- `internal/ai/tokens.go:79` — `EstimateTokensOpenAI` falls back to a 4.0 ratio, `TruncateToTokenLimit` to 3.5 (:118); with the tokenizer loaded, both count cl100k tokens
- `internal/ai/context.go:164` — `sourceBytesPerToken = 16`, so 64 KB per file at the default budget (164-167, 173-175)

**Change.** Tier 3. Derive the effective budget per turn. A per-provider `ContextTokenBudget` that the user sets stays as an explicit cap (`AIBehaviorPanel.tsx:232-238`). Without one, the budget is a share of `ContextLimitFor` minus the reserve for history, tools and output, with 4000 as the floor. The retrieval tools already give the model access to large flows, so the derived budget only sizes the context sent up front. For the estimator, the Flow Overview check (`context.go:150`) and the cut (`context.go:156`) just need to use the same function, for example a provider-aware `TruncateToTokenLimit`. Show the effective budget in `PreviewContext` and in the settings UI.

**Touches.** `internal/service/chat_context.go`, `internal/ai/context.go`, `internal/ai/tokens.go`, `internal/storage/interfaces/settings.go`, `frontend/src/components/chat/ContextPreviewModal.tsx`, `internal/ai/tools.go`, `core/models/settings.go`, `internal/service/chat_context_window_test.go`, `internal/ai/context_test.go`

**Done when.**
- With a provider that reports a 200k window and no explicit setting, the built context for a large fixture flow exceeds 4000 tokens and stays under the derived cap.
- An explicit per-provider `ContextTokenBudget` still caps the budget.
- `BuildContext` uses a single estimator function for both the Flow Overview decision and the final cut.
