# 25. Provider specs: one description per AI provider, every list derived, a catalogue route

Date: 2026-10-10. Status: accepted, implemented (Phase 4b, providers waves 1 and 2; the frontend's catalogue-driven
settings panel ships in the same phase).

## Context

Nine AI providers were described in about twelve hand-kept lists across the backend (`AvailableProviders`, the
constructor map, the keystore key names, the embedding-capable set and its fallback order, the alias-model set,
the API's `validProviders`, the settings defaults, a per-provider `Name()`/`DefaultModel()`/`PricePerMillionTokens()`
method set) and three in the frontend, with about 48 branch lines that compared a provider id by name
(`== "copilot"` for its two keys and its no-org rule, `s.azure != nil && id == azure` at ten sites for the Azure AI
Foundry registry, `== "github-models"` for its token row). Adding a provider meant editing all of them and shipping a
frontend release; the lists drifted (the default Gemini model id did not exist, the local provider had three names,
the embedding list was copied into `internal/config` and the frontend by hand). An unknown provider id in a chat
turn was logged at WARN as "provider setup failed" and told the user to retry, as if the server were broken.

The wire carried no credential schema, no capabilities beyond `supportsTools` per model, no modes: a client could
parse an unknown provider but had nothing to render it from, and `GET /api/providers/list` hid the local provider
until it was configured, so a generic settings panel had nothing to offer.

## Decision

1. **One `ai.ProviderSpec` per provider, in `internal/ai/spec_<id>.go`, registered at init.** It carries the
   identity (`ID`, `Name`, `AuthType`, `Order`), the capabilities (`Caps`: chat, stream, embeddings, tools,
   reasoning, model_aliases, live_listing, org_shared, prompt_cache), the deployment profiles it is offered in
   (`Modes`, `capability.Profile` values), the outbound rule (`Egress`: fixed_host, user_endpoint, loopback), how
   the credential is entered (`Credential`: kind api_key | oauth_device | document | none, the generic form's
   fields, a help URL), the keystore rows it lives in (`Keys`: primary, OAuth token, family), the catalogue key and
   the default model, the pricing mode (catalog | free | unpriced), the wire family, the credential `Resolver`
   (Copilot's token-then-PAT rule), the `Ctor`, an optional `Listing` hook, and the panel's colour and icon.
   `Specs()` returns them in display order; `testdata/providers.golden` pins the table.
2. **Every list is derived.** `AvailableProviders`, `SupportsEmbeddings`, `EmbeddingFallbackOrder`, `storageKey`,
   `UsesModelAliases`, `KnownProvider`, the metadata provider, the per-provider `Name()`/`DefaultModel()` and the
   provider-wide price (`specDefaultPricing`: the catalogue row of `DefaultModel`; nothing for free or unpriced)
   read the spec. The factory has one `build`: the spec's credential rule (no `CapOrgShared` means no org
   fallback), its `Resolver` or its `Keys.Primary`, its `Ctor`. The service reads "configured" from `Keys`
   (either row of an OAuth provider), the org-shared rule from `CapOrgShared`, the OAuth flows' key names from
   the specs, the listing's local-only rule from `Modes`, the budget's in-flight reservation and the usage pricing
   from `Pricing`. `internal/config.EmbeddingProviderIDs` and `core/models.DefaultSettings` keep a copy (neither
   module can import `ai`), each pinned to the registry by a test; `ProvideAI` refuses a `PAD_EMBEDDING_PROVIDER`
   the registry cannot embed with.
3. **The `authType` wire enum is unchanged** (`api_key | oauth`). The document providers keep reporting `api_key`;
   what a client needs to render the credential is `credential.kind`, which is new and optional.
4. **Azure AI Foundry stays a family behind a hook.** The registry (resources, model entries, designations,
   `m/<id>` ids, per-resource credentials) is a stable sub-system with its own contract suite and 19 frontend
   files; folding it into the generic spec would mean re-expressing resources and kinds generically for a shape no
   other provider has. The spec names the family (`Keys.Family`, `CapModelAliases`, `Credential.Kind document`)
   and the `Listing` hook (`Configured`, `OrgConfigured`, `Picker`) routes the listing, the `has` check and the
   catalogue to the registry. The hook is installed at boot (`ai.InstallListing`) because the registry is a
   service built after the specs register; a service built with the registry reaches it directly as well, so a
   test that wires a registry needs no process-global install and a test without one keeps the single-document
   behaviour. The one place that knows the registry serves the azure-foundry spec is `registryFamily` in the
   service; the ten `s.azure != nil && id == azure` sites ask the hook.
5. **Pricing comes from the catalogue.** The nine provider-wide price constants are deleted; the provider-wide
   rate is the catalogue row of the spec's default model. The audited wrapper's `pricingFor` and
   `ReservationPricing` read `spec.Pricing`: an unpriced family is a zero that counts as found (nothing is reserved
   or billed for a Foundry deployment, whose cost the Azure portal shows); a free provider's unlisted model is
   still a listing miss (the provider-wide $0 applies and the fallback metric moves), so a stub's rate in tests is
   never overridden.
6. **A catalogue route, `GET /api/providers/catalog`**, with the policy of `/api/providers/list` (JWT, the read
   scope, the general rate bucket, outside the request transaction), answers `{providers: [...]}`: every
   registered provider of the running profile (`capability.Of(cfg).Profile()` against `spec.Modes`), configured or
   not, in display order, each with `id, name, authType, modes[], caps[], credential{kind, fields[], helpUrl?},
   endpoints{save?, test?, start?, poll?, revoke?, user?}, configured, color?, icon?, pricing, configurable`.
   `endpoints` names today's routes (`/api/keys/save`, `/api/providers/test`, `/api/providers/{github,copilot}/*`);
   a document provider offers the test alone (its own panel saves it), an unknown document provider nothing
   (`configurable: false`: "configured on the server"). It is network-free: no live model listing. `ProviderInfo`
   on `/list` gains the same optional fields. No `POST /api/providers/{id}/config` this phase.
7. **A stable code for an unknown provider id.** `PROVIDER_UNKNOWN` (400) on `/api/keys/{save,has,delete}` and
   chat create and context preview (refused before any stream), `ProviderTestResult{ok: false, code:
   "PROVIDER_UNKNOWN"}` on `/api/providers/test` (its 200 shape), and the stream error code of a turn that reaches
   the factory with one. `ai.ErrUnknownProvider` is the sentinel; the error text is unchanged. It is the
   client's mistake and is never logged as a setup failure.
8. **A ratchet.** `TestArch_ProviderIDLiteralsOutsideSpecs` counts provider ids spelled as string literals outside
   the spec files, the catalogue and config's copy, pinned per file (95 in 21 files before wave 1; 33 in 16 after
   it; 22 in 15 after wave 2 — the rest are a deployment mode, a user id and an Azure model kind that share a
   spelling, plus the provider files' own ids). `RegisterProvider` stays exported for the tests that swap a
   constructor and is forbidden in production code (`TestArch_RegisterProviderOnlyInTests`).

## Consequences

- A new bare-key provider is one `spec_<id>.go` and a catalogue entry: the factory builds it, `/api/keys/*` accept
  it, the listing and the catalogue carry its form, and a client that renders from the catalogue offers it without
  a release. A provider family with its own registry is a spec plus a `ListingHook`.
- The golden `internal/ai/testdata/providers.golden` is the review surface for a provider change (caps, modes,
  keys, pricing, credential fields).
- Old clients ignore the new `ProviderInfo` fields and never call the catalogue; the new client falls back to its
  built-in table when a listing lacks them (one release).
- The catalogue's `configured` is the keystore's answer (or the family's), so it costs no upstream call; the live
  model listing stays on `/list`.
- `ReservationPricer` has no caller in the chat service (it reserves through its own `inFlightPricing`, which
  answers the same); the interface stays for a caller that needs the wrapper's resolution.

## Rollback

Additive wire fields only: a previous release's client ignores `caps`, `modes`, `credential`, `pricing`, `color`,
`icon`, `configurable` and `code`, and never asks for `/api/providers/catalog`; a previous release's server answers
the new client's `/list` without them (the client's fallback table) and 404s the catalogue (the client keeps its
hard-coded panel). No schema change. The keystore rows keep their names (`claude`, `github-models-token`,
`copilot`, `copilot-oauth-token`, `azure-foundry`, `azure-foundry/<id>`, `local`): a binary of either release reads
what the other wrote.

## Open

- A generic `/api/providers/{id}/oauth/*` for a second device-flow provider; today's two keep their historical
  directories, which the catalogue's `endpoints` name.
- `POST /api/providers/{id}/config` for document providers (the generalisation of the local and Azure legacy
  config routes), deferred: the two document providers keep their panels.
- Org-shared credentials for a non-Azure provider: the factory, the listing and `isOrgConfigured` support any
  `CapOrgShared` spec, but only Azure has write routes (dormant for github-models, stated in its spec).
- A select field's option labels: the catalogue sends the value as the label until a spec names one (only the
  Azure document, rendered by its own panel, has a select today).
- The fixed-host providers' `*_API_URL` overrides bypass every egress guard (operator-controlled, `EgressFixedHost`
  states it); whether `operations.md` should say so.
