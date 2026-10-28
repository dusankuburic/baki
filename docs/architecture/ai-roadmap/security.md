# AI roadmap: Security

Items of category `security` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-security-8

**Desktop (keyring + file registry) has no orphan recovery for Azure credentials** · tier 3

**Problem.** The orphan sweep returns early without a `SecretFamilyLister`, because the keyring cannot list (`azure_models_lifecycle.go:182-184`). `credentialStamp` reports "not known" (`:254-256`), so desktop deletes are unconditional (`:274-277`). A failed post-commit keyring delete is only logged, with the note "the orphan sweep retries" (`azure_models.go:1602-1605`). On the desktop the sweep never runs, so that log text is wrong on the keyring path. `rollbackStalePending` covers pending rows only (`azure_models_lifecycle.go:290-310`). A resource whose keychain delete failed leaves a live credential in the OS keyring forever, invisible to the UI. A database deployment without an encryption key has the same no-sweep behaviour, because it keeps provider keys in the OS keyring (`internal/app/keystore.go:69-74`).

**Evidence.**
- `internal/service/azure_models_lifecycle.go:182` — `sweepOrphans`: `s.keyFamilies == nil` → return (182-184)
- `internal/service/azure_models_lifecycle.go:254` — `credentialStamp`: no `keyFamilies` → `known=false` (254-256)
- `internal/service/azure_models_lifecycle.go:274` — `deleteCredentialIfUnchanged`: `!known` → unconditional `deleteCredential` (275-277)
- `internal/service/azure_models.go:1602` — txhook: a failed credential delete is only logged, "the orphan sweep retries" (1602-1605)
- `internal/service/azure_models_lifecycle.go:296` — `rollbackStalePending` handles pending rows only (290-310)
- `internal/app/keystore.go:69` — a database deployment without `PAD_ENCRYPTION_KEY`/`PAD_AUTH_SECRET` keeps provider keys in the OS keyring (69-74)

**Change.** Fix the log text on the keyring path so it no longer promises a sweep. When a keyring delete fails, record the key and a timestamp in a small list in the file registry. Retry each listed delete on the next `prepareOwner`/`rollbackStalePending` pass, and drop the entry once the keyring delete succeeds. The panel counter and the GDPR-export changes are dropped.

**Touches.** `internal/storage/azure_registry_file.go`, `internal/storage/interfaces/azure_registry.go`, `internal/service/azure_models.go`, `internal/service/azure_models_lifecycle.go`

**Done when.**
- File-registry test: a delete whose keyring `Delete` fails leaves an entry in the list, and the next `prepareOwner` retries it and clears it once `Delete` succeeds.
- The log for a failed keyring delete no longer says the orphan sweep retries.
