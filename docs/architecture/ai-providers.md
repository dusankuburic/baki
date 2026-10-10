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
