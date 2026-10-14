# AI providers: the spec registry

An AI provider is described once, as an `ai.ProviderSpec` in `internal/ai/spec_<id>.go`, registered at init. Every
list the backend keeps about providers is derived from `ai.Specs()`, and `internal/ai/testdata/providers.golden`
pins the table (refresh with `go test ./internal/ai/ -run TestProviderSpecs_Golden -update-provider-specs` and
review the diff). The decision is ADR 0025.

## The spec

| Field | What it decides |
|---|---|
| `ID`, `Name`, `Order` | the id on the wire and in the keystore, the display name, the display position |
| `AuthType` | `authType` on the wire: `api_key` or `oauth` (unchanged enum; the document providers report `api_key`) |
| `Caps` | `chat`, `stream`, `embeddings` (Embed works, `EmbeddingIdentifier` names the identity), `tools`, `reasoning` (the request's effort reaches the wire), `model_aliases` (ids are deployment names: served-model memo, unpriced), `live_listing` (Models asks the vendor), `org_shared` (an org may lend a credential; without it every org rule collapses to the member's own key), `prompt_cache` |
| `Modes` | the `capability.Profile`s the provider is offered in; the listing and the catalogue filter on the running one |
| `Egress` | `fixed_host` (a vendor constant; the operator's `*_API_URL` override is trusted), `user_endpoint` (a host the user names, allow-listed, dial-guarded in the cloud profile), `loopback` (the user's machine only; never in the cloud profile) — `ai.ConfigureProviders` applies the deployment's policy to each at boot |
| `Credential` | how the credential is entered: `Kind` `api_key` (the generic form), `oauth_device` (the device flow), `document` (the provider's own panel), `none`; `Fields` (name, label, type `text`/`secret`/`url`/`select`/`number`/`json`, required, placeholder, help, options); `HelpURL` |
| `Keys` | the keystore rows: `Primary` (the key, the PAT or the document), `OAuthToken` (the device-flow token, Copilot), `Family` (`<Primary>/<id>` rows, Azure resources) |
| `Catalog`, `DefaultModel` | the `modelCatalog` key ("" = whatever the user's server serves) and the model offered first, whose catalogue row is the provider-wide price |
| `Pricing` | `catalog` (per model, the default model's row as fallback), `free` (nothing metered), `unpriced` (the vendor's portal is the source; nothing reserved) |
| `Wire` | `anthropic`, `openai`, `gemini`, `responses` — metadata today (the codecs stay per provider); the cache-read multiplier keys on it |
| `Resolver` | reads the credential itself when the rule is not "Keys.Primary under the member/org rule" (Copilot: token first, then PAT) |
| `Ctor` | builds the raw provider from a `Credential`; `Ctor(Credential{})` is the metadata provider |
| `Listing` | a `ListingHook` for a family whose configured state and model list live outside the keystore and the catalogue (the Azure AI Foundry registry) |
| `Color`, `Icon` | the generic panel's accent and glyph |

`RegisterProvider` swaps a constructor or adds a stub for a test; production code never calls it
(`TestArch_RegisterProviderOnlyInTests`). A stub is built, never listed.

## What derives from it

- `internal/ai`: `AvailableProviders`, `SupportsEmbeddings`, `EmbeddingFallbackOrder` (embedding-capable specs in
  display order), `KnownProvider`, `UsesModelAliases`, `storageKey`, `GetMetadataProvider`, each provider's `ID()`,
  `Name()`, `DefaultModel()`, `PricePerMillionTokens()`; the factory's one `build` (credential rule, resolver,
  constructor); the audited wrapper's `pricingFor` and `ReservationPricing` (`Pricing`).
- `internal/service`: `isConfigured` (either keystore row), `isOrgConfigured` (`CapOrgShared`, then the hook or the
  org row), `ListProviders` (the running profile, the hook for a family, the spec's wire fields), `ProviderCatalog`,
  the connection test's unknown-id answer, the OAuth flows' key names, the chat turn's unknown-id refusal.
- `internal/api`: the `/api/keys/*` gate (`KnownProvider`), the catalogue route.
- `internal/rag`: the default embedding provider (the first embedding-capable spec), the "registry-embedded" rule
  (`CapModelAliases`: opt-in only, never a fallback).
- `internal/di`: `ConfigureProviders` (egress per spec), the `PAD_EMBEDDING_PROVIDER` check against the registry,
  the Azure `Listing` hook installed on the spec.
- Copies that cannot import `ai`, each pinned to the registry by a test: `internal/config.EmbeddingProviderIDs`
  (`TestEmbeddingProviderIDs_MatchRegistry`), `core/models.DefaultSettings().AI.Providers`
  (`TestDefaultSettings_ProvidersMatchRegistryAndCatalog`), the frontend's known set (the tolerant client renders
  unknown ids from the wire).

## The hooks

A `ListingHook` answers `Configured(ctx, scope)`, `OrgConfigured(ctx, orgID)` and `Picker(ctx, scope, orgID)`
(`ProviderListing{Models, DefaultModel, Configured, Shared, SuggestDiscovery}`). The service lists a provider with a
hook from the hook (no live listing), reads "configured" from it, and the catalogue's `configured` too. A hook is
installed at runtime with `ai.InstallListing(id, hook)` because a registry is a service built after init; DI does
it for Azure AI Foundry (`service.AzureListing`). A service built with the registry reaches it directly as well
(`listingHook`), so a test that wires a registry needs no process-global install, and `registryFamily` is the one
place that knows the registry serves the azure-foundry spec. The registry's own operations that are not a listing
(the single-document delete, the resource probe behind the deprecated test, chat's model resolution) stay the
registry's, reached through the same adapter.

## On the wire

`GET /api/providers/list` entries carry, optionally, `caps[]`, `modes[]`, `credential{kind, fields[], helpUrl?}`,
`pricing`, `color`, `icon`, `configurable`. `GET /api/providers/catalog` (the policy of `/list`) answers
`{providers: [...]}` for every registered provider of the running profile, configured or not, in display order:

```json
{"id": "claude", "name": "Claude", "authType": "api_key", "modes": ["desktop", "local+db", "cloud"],
 "caps": ["chat", "stream", "tools", "reasoning", "org_shared", "prompt_cache"],
 "credential": {"kind": "api_key", "fields": [{"name": "key", "label": "API key", "type": "secret", "required": true, "placeholder": "sk-ant-..."}], "helpUrl": "https://console.anthropic.com/settings/keys"},
 "endpoints": {"save": "/api/keys/save", "test": "/api/providers/test"},
 "configured": false, "color": "#d4a574", "icon": "cloud", "pricing": "catalog", "configurable": true}
```

`endpoints` names today's routes per credential kind: the key routes for `api_key`, the device flow's four under
`/api/providers/{github,copilot}/` for `oauth_device`, the test alone for a `document` provider with a panel, nothing
for a document provider without one (`configurable: false`: the client shows "configured on the server").

An id no spec describes is `PROVIDER_UNKNOWN` (400) on `/api/keys/*`, chat create and the context preview,
`{ok: false, code: "PROVIDER_UNKNOWN"}` on `/api/providers/test`, and the stream error code of a turn that reaches
the factory with one; the codes are listed in [api-conventions.md](api-conventions.md).

## Adding a provider

1. A catalogue entry in `internal/ai/catalog.go` (models, context limits, prices) and a `spec_<id>.go` with
   `func init() { mustRegisterSpec(spec) }`: `Caps` as the provider truly behaves (`TestProviderSpecs_Complete`
   checks `CapTools` against `SupportsTools()`, `CapEmbeddings` against `EmbeddingIdentifier`, the default model
   against the catalogue), `Modes`, `Egress`, `Credential`, `Keys{Primary: id}`, `Pricing`, `Wire`, `Ctor`, a
   unique `Order`.
2. A `Ctor` over an existing wire family (`openaiBase` for an OpenAI-compatible vendor) or a new provider file.
   A new provider file does not spell its own id: it reads it from the spec (`spec.ID`) or takes it from the
   constructor, since the literal ratchet (step 5) fails on any file outside the specs that is not pinned.
3. Refresh and review `providers.golden`; add a row to `core/models.DefaultSettings` (the pin requires one). An
   embedding-capable provider (`CapEmbeddings`) is also added to `internal/config.EmbeddingProviderIDs` — config
   cannot import `ai`, so it keeps a copy that `TestEmbeddingProviderIDs_MatchRegistry` holds to the registry.
4. Nothing else for a bare-key provider: the factory, `/api/keys/*`, the listing, the catalogue and a client that
   renders from the catalogue carry it. A device-flow provider also needs its auth routes (the two existing ones
   keep historical directories; a generic `/api/providers/{id}/oauth/*` is open). A family with its own registry
   adds a `ListingHook` and installs it from DI.
5. The literal ratchet (`TestArch_ProviderIDLiteralsOutsideSpecs`) refuses a new `== "<id>"` branch outside the
   spec files: ask the spec (`Caps`, `Keys`, `Modes`, `Listing`) instead. Its id list is `ai.Specs()`, so a new
   spec is counted the moment it registers.

## The in-flight budget ledger across replicas (addendum to ADR 0028, Phase 5b)

[ADR 0028](../adr/0028-ai-budget-hook-and-provider-policy.md) made the in-flight ledger a seam (`spendLedger`) and
pinned one rule: a ledger that cannot answer makes the check fail closed. With more than one replica the ledger has to
be shared, and this addendum records how, and what the rule means once it can fail. The ADR itself is unchanged.

- **Which ledger.** `PAD_MAX_REPLICAS > 1` and `PAD_REDIS_URL` set: `redisLedger` (`internal/service/ai_budget_redis.go`, on
  `redisx.InflightLedger`). Anything else: `memLedger`, exactly as before. In particular **a single replica keeps the
  in-process ledger even with `PAD_REDIS_URL` set** (rate limits, rooms and stream resume use that Redis): the Redis
  ledger fails closed, and giving it to a deployment that has nothing to share would turn every Redis blip, such as a
  managed cache patched for ten minutes, into `AI_BUDGET_UNAVAILABLE` for a replica that had a perfectly good
  ledger of its own (`provideAIBudget`, `internal/di/services.go`). `hold` and `release` now return an error; `hold`
  failing refuses the stream with `ErrAIBudgetUnavailable` before a worker is counted (`tryStartStream`), `release`
  failing is logged and left to age out.
- **Keys.** `baki:ai:inflight:user:<scope>` and `baki:ai:inflight:org:<orgID>`, hashes, one field per running stream
  (a random handle, never the user or the client's stream ID), value `{"v":1,"kind":"ai.inflight","cost":x,"exp":ms}`.
  The cost is the stream's spend so far plus the reservation of one more maximum response, rewritten every second by
  the ledger's own refresh goroutine (it runs only while this replica holds a stream) and published in the same round
  trip as every budget check's read, so of two checks that overlap in time one sees the other. Readers sum the other
  streams' fields and skip their own, fields past `exp` (a 30 s lease: a replica that dies stops counting after it) and
  fields that do not decode (never deleted: another release may have written them). The hash TTL is 11 minutes, the
  stream cap plus slack. Both keys are declared in the redisx registry (`on-down: closed`).
- **The bound.** A stream is admitted while the others' in-flight spend is under the budget, as in one process, so the
  admitted reservations total less than the budget plus one reservation, whichever replicas the starts land on. A start
  stampede can refuse all of its members, which costs a retry, not budget. Between refreshes a stream's cost can grow
  by at most what its reservation already covers. Replica clocks are compared for the lease, so a skew of seconds
  shifts when an orphan stops counting, not the bound.
- **Where the write happens.** `tryStartStream` reserves the stream's slot (the per-caller cap and the stream-id check)
  under the stream-cap lock and writes the ledger entry after releasing it, then takes the lock again to count the
  worker, refusing the start (and giving the entry back) if the server began stopping meanwhile. A slow Redis therefore
  delays only the start that waits for it, never the other starts or `Shutdown`; with Redis marked degraded the
  ledger does not ask at all (`hold`, `others` and the refresh refuse or skip at once), which is the same fail-closed
  answer without the up-to-2-second wait.
- **What an evicted record does.** Every ledger key has a TTL, so `volatile-lru` can evict one when Redis reaches
  `maxmemory`, and nothing errors when it does: the budget would under-count. Two things bound that. A held stream
  rewrites its record every second, and a write that has to create a record that was written before (the field was
  not there) is recognised (the `HSET` reply says so): the next budget check on that replica fails closed once
  (`AI_BUDGET_UNAVAILABLE`, counted `pad_ai_ledger_errors_total{op="lost"}`) and the record is whole again for the
  check after. Peers' records are rewritten by their owners within a second, so the under-count lasts one refresh
  interval at most. The server's `evicted_keys` is exported as `pad_redis_evicted_keys_total` (polled every 30 s with
  the health ping) and a WARN names it; alert on any increase, and size `maxmemory` with headroom (the SSE logs are
  the large tenant: up to 200 frames per active user). The policy itself is the operator's (`volatile-lru` or
  `noeviction`); the app warns at boot if INFO reports an `allkeys-*` policy.
- **Redis down.** Only while a daily budget is configured (`AI.DailyBudget > 0`): with none, nothing is held, read or
  released, and a Redis outage costs the chat nothing. With one, every entry point refuses with `ErrAIBudgetUnavailable`
  (`AI_BUDGET_UNAVAILABLE`, the existing code and the existing 503 on the knowledge routes): a new turn at admission,
  the turn's start check, every tool-loop re-check, and the audited wrapper's per-call hook. A running stream stops at
  its next re-check or provider call with "response stopped: couldn't verify today's AI budget: the shared usage
  counter is unreachable — try again in a moment". There is no grace period and no last-known-sum cache: the rule is
  the one the ADR pins ("unreadable means refuse"), applied to a stream that is already running. A stream whose provider
  call is in progress finishes that call. Recovery is automatic; nothing needs a restart.
- **Mixed versions.** A replica of the release before keeps its in-process ledger and neither reads nor writes these
  keys, so it does not see the others' streams. That is acceptable only at one replica, which is the first deploy's
  `maxReplicas`; roll the fleet to this release before raising it.
- **Metrics.** `pad_ai_ledger_errors_total{op}` (hold, refresh, read, release, lost), `pad_ai_ledger_held` (streams this
  replica holds), `pad_ai_ledger_expired_total` (orphans found past their lease). Refusals are counted where they
  always were, `pad_ai_budget_refusals_total{reason="unavailable"}`.
