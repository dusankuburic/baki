# AI providers: the spec registry

A provider is described once, as an `ai.ProviderSpec` in `internal/ai/spec_<id>.go`, registered at init (ADR 0025). Every provider list is derived from `ai.Specs()` — the factory, the keystore keys, the embedding order, the listing, the catalogue route — and `internal/ai/testdata/providers.golden` pins the table: refresh with `go test ./internal/ai/ -run TestProviderSpecs_Golden -update-provider-specs` and review the diff. Never branch on a provider id by name: ask the spec (`Caps`, `Keys`, `Modes`, `Listing`); `TestArch_ProviderIDLiteralsOutsideSpecs` pins the literal count per file (only downward), and `TestArch_RegisterProviderOnlyInTests` keeps `RegisterProvider` out of production code.

Load-bearing spec fields: `Caps` (`chat`, `stream`, `embeddings` — with `EmbeddingIdentifier`, `tools`, `reasoning`, `model_aliases`, `live_listing`, `org_shared`, `prompt_cache`); `Modes` (the capability profiles the provider is offered in); `Egress` (`fixed_host`, `user_endpoint` — allow-listed and dial-guarded in the cloud profile — or `loopback`, never in the cloud profile); `Credential` (kind `api_key`/`oauth_device`/`document`/`none`, with typed `Fields` and `HelpURL`); `Keys` (the keystore rows: `Primary`, `OAuthToken`, `Family` for Azure resources); `Wire` (`anthropic`, `openai`, `gemini`, `responses`); `Resolver` (reads the credential when the rule is not "Keys.Primary"); `Ctor` (builds the raw provider); `Listing` (a `ListingHook` for a family whose state and models live outside the keystore — the Azure AI Foundry registry, installed from DI with `ai.InstallListing`). Copies that cannot import `ai` are pinned to the registry: `internal/config.EmbeddingProviderIDs` (`TestEmbeddingProviderIDs_MatchRegistry`), `core/models.DefaultSettings().AI.Providers`, the frontend's known set (the tolerant client renders unknown ids from the wire).

On the wire, `GET /api/providers/list` entries carry optional `caps[]`, `modes[]`, `credential{kind,fields[],helpUrl?}`, `color`, `icon`, `configurable`, and `GET /api/providers/catalog` (same policy) answers every registered provider of the running profile, configured or not, with its credential schema and per-kind `endpoints`. An id no spec describes is `PROVIDER_UNKNOWN` (400 on `/api/keys/*`, chat create and the context preview; 200 `{ok:false}` on `/api/providers/test`; the stream error code) — the codes are in [api-conventions.md](api-conventions.md).

## Adding a provider

1. A catalogue entry in `internal/ai/catalog.go` and a `spec_<id>.go` registering at init: `Caps` as the provider truly behaves (`TestProviderSpecs_Complete`), `Modes`, `Egress`, `Credential`, `Keys{Primary: id}`, `Wire`, `Ctor`, a unique `Order`.
2. A `Ctor` over an existing wire family (`openaiBase` for an OpenAI-compatible vendor) or a new provider file — which never spells its own id literal (the ratchet fails on it).
3. Refresh `providers.golden`; add a `core/models.DefaultSettings` row (a pin requires one); an embedding-capable provider also joins `internal/config.EmbeddingProviderIDs`.
4. A device-flow provider needs its auth routes; a family with its own registry adds a `ListingHook` installed from DI. A bare-key provider needs nothing else.

## Budgets and prices

There are none ([ADR 0037](../adr/0037-remove-ai-budgets-and-pricing.md)): the catalogue keeps model ids, windows and output caps, a usage row records tokens only, and nothing refuses a call for spend. Spend is read in the vendors' consoles and the Azure portal.

## What is still missing

Known gaps in the provider layer — error classification, transport timeouts, the half-open probe, model catalogues — are items in the [AI assistant roadmap](ai-roadmap.md); the provider ones are in [`ai-roadmap/provider.md`](ai-roadmap/provider.md).
