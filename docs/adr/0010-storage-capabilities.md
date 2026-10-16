# 10. Storage capabilities: role interfaces for the domain, fx values for the database

Date: 2026-10-08. Status: accepted.

## Context

About 24 call sites type-asserted the storage backend to learn its capabilities; fallback branches ran only in tests (tests passed on code production never ran), and database-only capabilities (the advisory lock, the pool, the keystore, the RLS transaction) forced `internal/api` to import the database package.

## Decision

- Domain capabilities are methods of the role interfaces `StorageBackend` embeds: `TriageStore.GetFindingStatus`, `APITokenStore.GetAPITokenWithOwnerByHash`, `OrgStore.OrgMemberRole` and `ListOrgMembershipsForUser`, `FlowStore.ListFlowsPage`, `AuditStore.SaveAuditEvents`, `UserStore.ExportUserDataTo`, and the role interfaces `RefreshTokenStore` and `IdentityLinkStore`. Every backend implements them (Postgres and the fake, held by contract scenarios); the fallback branches are gone.
- Database-only capabilities are fx values of `provideStorageBackend` (`StorageResult`, nil without a database; the request transaction and Idempotency-Key store also nil without RLS): `lock.GlobalLocker` (`internal/platform/lock`, declared once), `storagedb.SQLProvider`, `storagedb.KeyStoreProvider`, `storagedb.FieldEncryptor`, `audit.OrgBackfiller`, `service.BlobHealthChecker`, `storagedb.AzureRegistryProvider`, `reqtx.Opener` (`internal/platform/reqtx`), `storageif.IdempotencyStore`. They are required graph inputs whose value may be nil, not `optional:"true"` tags — a missing provider fails `TestAppGraph` instead of quietly running without the lock.
- The keystore's capabilities are fx values of `provideKeyStore` (`KeyStoreResult`): the org credential store (`service.OrgKeyStore`) and the personal key-family lister (`storageif.SecretFamilyLister`) are nil with the OS keyring (it cannot enumerate entries); the Azure services test them for nil instead of asserting.
- `TestArch_StorageTypeAssertionsRatchet` pins type assertions to none outside the storage packages; `internal/api` does not import the database package (`TestArch_APIDoesNotImportStorageDatabase`).

## Consequences

A new store method is implemented in Postgres and the fake with a contract scenario; a new database-only capability is a field of `StorageResult` and a parameter of its consumer.
