# 25. Provider specs: one description per AI provider, every list derived, a catalogue route

Date: 2026-10-10. Status: accepted, implemented (Phase 4b, providers waves 1 and 2).

## Context

Nine AI providers were described in ~twelve hand-kept lists across the backend and three in the frontend, with ~48 `== "copilot"`/`id == azure` branch lines; the lists drifted (a default Gemini model id that did not exist) and an unknown provider id was logged as a server fault. The wire carried no credential schema or capabilities, so a client could not render an unknown provider.

## Decision

1. One `ai.ProviderSpec` per provider in `internal/ai/spec_<id>.go`, registered at init: identity (`ID`, `Name`, `AuthType`, `Order`), capabilities (`Caps`: chat, stream, embeddings, tools, reasoning, model_aliases, live_listing, org_shared, prompt_cache), deployment profiles (`Modes`), outbound rule (`Egress`), credential description (`Credential`: kind api_key | oauth_device | document | none, form fields, help URL), keystore rows (`Keys`: primary, OAuth token, family), catalogue key and default model, pricing mode (catalog | free | unpriced), credential `Resolver`, `Ctor`, optional `Listing` hook, panel colour/icon. `Specs()` returns display order; `testdata/providers.golden` pins the table.
2. Every list is derived from the spec: `AvailableProviders`, `SupportsEmbeddings`, `EmbeddingFallbackOrder`, `storageKey`, `UsesModelAliases`, `KnownProvider`, provider-wide pricing (the catalogue row of `DefaultModel`; unpriced = a zero that counts as found), the factory's single `build`. `internal/config.EmbeddingProviderIDs` and `core/models.DefaultSettings` keep a pinned copy (those modules cannot import `ai`); `ProvideAI` refuses a `PAD_EMBEDDING_PROVIDER` the registry cannot embed with.
3. The `authType` wire enum is unchanged (`api_key | oauth`); `credential.kind` is new and optional.
4. Azure AI Foundry stays a family behind a `Listing` hook (`Configured`, `OrgConfigured`, `Picker`, installed at boot via `ai.InstallListing`); the spec names the family (`Keys.Family`, `Credential.Kind document`) and the ten `s.azure != nil && id == azure` sites ask the hook.
5. Catalogue route `GET /api/providers/catalog` (policy of `/api/providers/list`, network-free) answers `{providers: [...]}` — every registered provider of the running profile with `id, name, authType, modes[], caps[], credential{kind, fields[], helpUrl?}, endpoints{save?, test?, start?, poll?, revoke?, user?}, configured, color?, icon?, pricing, configurable`. `ProviderInfo` on `/list` gains the same optional fields.
6. `PROVIDER_UNKNOWN` (400) on `/api/keys/{save,has,delete}`, chat create and context preview, `ProviderTestResult{ok:false, code:"PROVIDER_UNKNOWN"}` on test, and the stream error code — never logged as a setup failure.
7. Ratchet: `TestArch_ProviderIDLiteralsOutsideSpecs` counts provider-id string literals outside the spec files (pinned per file); `RegisterProvider` is test-only.

## Consequences

A new bare-key provider is one `spec_<id>.go` plus a catalogue entry; a client rendering from the catalogue offers it without a frontend release. Rollback is additive wire fields only (an old client ignores them and never calls the catalogue; an old server 404s it and the client keeps its hard-coded panel); no schema change. Keystore row names are stable (`claude`, `github-models-token`, `copilot`, `copilot-oauth-token`, `azure-foundry`, `azure-foundry/<id>`, `local`). Open: org-shared credentials are only wired for Azure; fixed-host `*_API_URL` overrides bypass every egress guard (operator-controlled).
