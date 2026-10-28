# AI roadmap: Provider layer

Items of category `provider` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

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
