# 40. Settings and credentials say what the server holds

Date: 2026-10-26. Status: accepted, implemented (AI roadmap step 5).

## Context

Several settings and credential paths reported a state the server did not have:

- The member settings route stored the ai section as received. The custom instructions suffix, which is appended to every system prompt, had no cap, and neither had the prompt lists; the route had no body cap of its own.
- The client posted the whole settings document after a one-second debounce. A refused save rolled back with no message, and the chat store's `selectedProvider` was written and never read.
- In cloud the member panel offered the embedding provider and model, which land in the member's own document; the knowledge indexer reads only the deployment's.
- `/api/keys/save` stored an empty or arbitrary key, and the catalogue then called the provider configured.
- An unreachable OS keychain made a removal look done, and every save or removal that failed on it answered an opaque 500.
- A key sealed under a previous encryption key read as configured in the catalogue and failed every turn with "try again in a moment".

## Decision

1. **Every settings writer checks the ai section it stores.** `validateAISection` (`internal/service/settings_validation.go`) runs on the member route and on the org route, which on the desktop writes the deployment's document. It refuses, with `ErrSettingsInvalid` naming the field:
   - a suffix over 4000 characters;
   - more than 20 prompts in a list, or a prompt over 500 characters;
   - an unknown active provider, a new provider entry no spec describes, or an embedding provider that cannot embed;
   - an embedding or default model id over 200 characters;
   - a temperature outside 0..2, Max tokens outside 0..32000, a context budget outside 0..128000, or an unknown reasoning effort.

   A value equal to the stored one is not checked. The client posts the whole section it holds, so a value stored before a cap existed must not refuse a later write that changes something else. A write that does not name the section is never refused for it. The route reads at most 1 MiB, as the org route does.
2. **One list of caps.** The caps are mirrored in `frontend/src/lib/constraints.ts`, and `TestSettingsCaps_MirrorTheFrontendConstraints` compares the two. The fields stop at them: the suffix, the prompts, the prompt lists and the key.
3. **A client write posts only the sections it changed.** The settings store's persist queue collects the sections of the coalesced writes and posts `{version, …sections}` from the store as it is at the flush; a reset posts the whole document. Two tabs editing different sections no longer overwrite each other.

   A refused write puts back each field it changed, unless a later change has taken that field over since, and is reported once, however many changes it carried. The report is `saveError`, which the AI panels' status line and the app-wide toast show; a layout-only or aborted write is put back without one. The document is re-read once no write is pending, and a re-read keeps the store's value for any section with a write still pending. A save covering the reported sections clears the report.
4. **`activeProvider` is the one record of the active assistant.** The chat store's `selectedProvider` and `setProvider` are gone.
5. **On a cloud server the member panel does not offer the embedding pair.** It points at the admin's AI embeddings card, the pair's one writer since the tier-0 pass (`PUT /api/admin/settings/ai`). On the desktop and in local mode the panel still writes it.
6. **A key is a key.** `SaveApiKey` stores it trimmed. It refuses one that is empty, over 8 KiB, or holds a control or invisible format character (a zero-width space, a byte-order mark), with 400 `KEY_INVALID`. The route reads at most 64 KiB and refuses a body field that is neither `provider`, `key` nor a field the provider's credential declares.
7. **One mapping for a secret store failure.** `render.Error` passes every error through `service.SecretStoreError`:
   - `storage.ErrSecretStorageUnavailable` answers 503 `SECRET_STORAGE_UNAVAILABLE`, the Azure routes' code for it;
   - `storage.ErrSecretDecryptFailed` answers 422 `KEY_UNREADABLE`, with the admin's copy and `details.shared` when the key was an organisation's (`ai.OrgKeyError`).

   The desktop keyring's `Delete` reports an unreachable keychain instead of claiming the key is gone. `Get` and `Has` keep reading an absent backend as no key, the documented headless behaviour.
8. **A credential has a status.** The catalogue and listing entries carry `credentialStatus` (`configured`, `unreadable` or `missing`), read with `Get` rather than checked for a row; the catalogue also carries `orgCredentialStatus`. An empty legacy row is `missing`. `/api/keys/has` answers `unreadable: true` beside `has: true`.

   An unreadable own key is not passed over for the organisation's: the turn reports the keystore failure rather than fall through (`ProviderFactory.resolveKey`), so the listing says what the turn will.

   A chat turn whose bare key or Copilot token cannot be decrypted is `PROVIDER_NOT_CONFIGURED`, with copy that asks for the key or the sign-in again. When the key is the organisation's (`ai.OrgKeyError`), the copy carries the shared flag and points at the admin. The device-flow user routes answer `KEY_UNREADABLE` instead of "not connected", and the settings form and the device-flow card ask for the key or the sign-in again.

## Consequences

- An API client that posted a value over a cap now gets a 400 naming the field. Values already stored keep working until they change.
- A desktop with its keychain down now sees 503 on saving, removing and revoking a credential. These calls used to answer 500, or succeed without doing anything.
- `render.Error` maps a decrypt failure to 422 on every route that renders one. Routes with their own mapping, the Azure registry's, keep theirs.
- The catalogue reads each bare key with `Get`, which decrypts it, where it used to check for a row. That costs one AES-GCM open per configured provider per catalogue call. The keystore logs a row's decrypt failure once per process.
- An older client ignores the new fields and codes. An older server sends no `credentialStatus`, and the client then shows the form it showed before.
