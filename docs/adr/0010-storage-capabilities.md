# 10. Storage capabilities: role interfaces for the domain, fx values for the database

Date: 2026-10-08. Status: accepted.

## Context

About 24 call sites found out what the storage backend could do by type-asserting it. Two kinds were mixed:

- **Domain capabilities** that any store could offer but only Postgres did: one finding's triage state, a token
  with its owner in one round trip, a user's role in one organisation, the roster-free membership list, a flow page
  with its total, the batch audit write, the streamed export, refresh tokens and identity links, and the wider
  chat store. Each assertion had a fallback branch for "the backend lacks it", which only the test doubles took, so
  tests passing on the fake exercised code production never ran.
- **Database-only capabilities**: the cross-replica advisory lock (declared three times, in `api`, `audit` and
  `scanner`), the pool for the token blacklist, the encrypted keystore, field encryption, the audit org backfill, the
  blob probe, the Azure registry and its export hook, the Idempotency-Key store, and the request's row-level-security
  transaction. The last one made `internal/api` import the database package.

## Decision

- **Domain capabilities are methods of the role interfaces** `StorageBackend` embeds: `TriageStore.GetFindingStatus`,
  `APITokenStore.GetAPITokenWithOwnerByHash`, `OrgStore.OrgMemberRole` and `ListOrgMembershipsForUser`,
  `FlowStore.ListFlowsWithTotal`, `AuditStore.SaveAuditEvents`, `UserStore.ExportUserDataTo`, and the new role
  interfaces `RefreshTokenStore` and `IdentityLinkStore`. Every backend implements them (Postgres and the test fake),
  the storage contract holds all of them to Postgres's semantics, and the fallback branches are gone. A service
  declares the slice it uses on its field (the chat store names `LoadUserSettings` and `OrgMemberRole`).
- **Database-only capabilities are fx values.** `provideStorageBackend` in `internal/app` (in `main.go` until Phase 5a
  split it into fx modules) provides each from the concrete backend as a value of its own (`StorageResult`), nil
  without a database; the request transaction and the Idempotency-Key store are also nil without row-level security.
  Consumers take the narrow value they need and test it for nil:

  | Value | Consumers |
  |---|---|
  | `lock.GlobalLocker` (`internal/platform/lock`, declared once) | scanner, retention sweep, legacy migration, at-rest backfill, audit org backfill |
  | `storagedb.SQLProvider` | the token blacklist |
  | `storagedb.KeyStoreProvider` | the provider-key keystore |
  | `storagedb.FieldEncryptor` | the at-rest encryption of org channels |
  | `audit.OrgBackfiller` | the boot-time audit org backfill |
  | `service.BlobHealthChecker` | readiness and the admin health page |
  | `storagedb.AzureRegistryProvider` | the Azure registry and the export's configuration hook |
  | `reqtx.Opener` (`internal/platform/reqtx`, via `storagedb.RequestTx`) | the RLS middleware |
  | `storageif.IdempotencyStore` | the Idempotency-Key middleware |

  The values are required graph inputs whose value may be nil, not `optional:"true"` tags: a missing provider fails
  `TestAppGraph` instead of quietly running without the lock. Test graphs supply nil values (`noDatabase()` in
  `internal/di`); test routers take them from what the test wired.
- **The assertions left are pinned.** `TestArch_StorageTypeAssertionsRatchet` (`internal/archtest`) counts, per file
  outside the storage packages, the type assertions whose value or asserted type is a store. `internal/api` no longer
  imports the database package (`TestArch_APIDoesNotImportStorageDatabase` allows nothing).

## Consequences

A new store method is implemented in Postgres and the fake, with a contract scenario; a test double that embeds the
fake gets it for free, one that overrides a read must override the method the service now calls. A new database-only
capability is a field of `StorageResult` and a parameter of its consumer.

The assertions left after the first pass are gone too (Phase 3 wave 2), and the ratchet allows none:

- The organisation service's store is a slice of `StorageBackend`: `OrgStore.MutateOrg` and the new role
  `OrgInviteStore`, held to Postgres's semantics by the contract (`contract/lifecycle.go`). `internal/app` gives the
  service the backend when the deployment has a server store (`capability.ServerStore`), the in-memory store
  otherwise.
- The keystore's capabilities are fx values of `provideKeyStore` (`KeyStoreResult`): the keys, the organisation
  credential store (`service.OrgKeyStore`, whose key families and restorer are part of it, since only the encrypted
  database keystore holds organisation credentials) and the personal key-family lister
  (`storageif.SecretFamilyLister`). The last two are nil with the OS keyring, which cannot enumerate its entries; the
  Azure services test them for nil (no orphan sweep, unconditional deletes on the desktop) instead of asserting.

## Alternatives considered

- **fx `optional:"true"` dependencies.** Shorter test graphs, but a graph that forgets the provider builds and runs
  without the lock or the RLS transaction.
- **One bundle value of every database capability.** Fewer graph inputs, but every consumer would depend on all of
  them, and nothing would show which one it uses.
- **Keeping the assertions behind helpers.** The fallback branches would stay, and so would the code only tests run.
