# Data lifecycle

What the data-subject export, account erasure, organisation deletion and the retention sweep do with each table
(ADR 0011). The declarations are in `internal/storage/database/lifecycle_<domain>.go`; the runners in
`lifecycle_export.go`, `lifecycle_erase.go` and `lifecycle_purge.go`.

## Adding a table

`TestPostgres_Lifecycle_EveryTableClaimed` fails until the table is claimed. Add a `tableClaim` to the domain's
`lifecycle_<domain>.go` (or a new file registering a new domain from `init`):

| Field | Say |
|---|---|
| `export` / `noExport` | the `UserDataExport` key carrying the user's rows, or why the bundle has none |
| `erase` (+ `eraseWhy`) | `erasedByStep` (add an erase step), `erasedByCascade` (an ON DELETE CASCADE foreign key to `users` or to something erasure deletes), or `notPersonal` with the reason |
| `org` (+ `orgWhy`) | `orgNotKeyed` (no `org_id`), `orgByStep` (add an org-delete step), `orgByCascade`, or `orgKept` with the reason |
| `noPurge` | why the retention sweep leaves it alone; empty when you add a purge step (and a `PurgeResult` field) |
| `legacy` | the legacy migrator step that fills it, if any |

A table holding a user's id or email without a cascading foreign key needs an erase step: delete the user's own rows
(`erasePersonal`), or clear the identity on rows kept for others (`eraseIdentity`). `TestPostgres_Lifecycle_ErasureSweep`
seeds a user into every table that can name them and searches every column afterwards; seed the new table there too.

The test fake (`internal/testutil`) mirrors erasure, organisation deletion, the export and the sweep for the rows it
keeps, and the storage contract (`internal/storage/contract/lifecycle.go`) holds the fake and Postgres to the same
outcomes: a changed step changes the fake and the scenario with it.

## What each operation does

- **Export** (`ExportUserData`, streamed by `ExportUserDataTo`): the account, the flows the user owns with content,
  settings, the uploaded avatar image (base64, `avatar` is null without one), the whole audit history, API tokens, chat on the owned flows, the comments the user wrote (on any flow,
  including one they can no longer open) and their triage decisions on the owned flows, their organisations with
  their own membership, active sessions and the personal Azure registry (no secrets). A flow shared with the user and
  the other members of an organisation are other people's data and stay out. Any read error fails the export.
- **Account erasure** (`DeleteUser`, after `AccountLifecycleService`'s prepare and before its finalize): the
  conversations of the owned flows, the owned flows (with everything that cascades), the per-user rows, then the
  account. On rows kept for others the identity is cleared: comment author and name, triage updater and assignee,
  version and baseline creator, share-link creator, invitation inviter, organisation owner, the minter of an
  organisation CI credential (the credential stays the organisation's), the editor of an organisation credential lent
  by someone else; a teammate's comment or assignment notification that names the user
  exactly ("New comment from <name>", "Assigned to you by <name>", display name or email), on a flow the user reached
  (a grant or membership, or a comment or triage decision of theirs, so a flow they have since lost access to too),
  says "a teammate".
  The audit trail keeps its rows: the user's own actions lose email, IP, token and user agent, rows about the user
  lose the user agent, and a failed login for the user's address loses the address too. The rows the deletion
  request records afterwards (the deletion itself, the credentials it disabled) carry no identity of the actor
  (`audit.WithErasedActor`). The organisation credentials the user lent are disabled by finalize, after the statements.
- **Organisation deletion** (`DeleteOrg`): the knowledge base, policies, channels and custom rules, then the row
  (members, invitations, settings, credentials and the Azure registry cascade). Its flows become their owners' own.
  Usage, alerts and the audit trail keep the id.
- **Retention sweep** (`PurgeExpiredData`; on the server store with accounts: cloud, and local mode with
  `PAD_AUTH_ENABLED`): expired sessions, tokens, invitations and share links; organisation CI credentials 30 days
  after they expired (listed until then, so an admin sees why a runner fails); the token blacklist, idempotency
  keys and the CI webhook's delivery records older than an hour in batches; flow undo snapshots older than 24 hours
  (batched; reads hide them at once); with an audit window, the audit trail,
  analysis history, usage and alerts older than it.

The erasure and deletion statements, and the export's comment read, run without the row-level-security user
(`asSystem`): they are keyed by one id the caller is authorised for, and must reach its rows on any flow.

## Undo ring (`flow_snapshots`, migration 0045)

The pre-mutation source that a fix, a batch fix, a source save or a restore replaced, so "that fix made it worse" can
be undone. Before Phase 5b it lived in a per-process ring (`service/flow_snapshots.go`), which a deployment of
several replicas cannot keep: an undo taken on a replica that did not apply the fix found nothing ("snapshot not
found"), and a restart or deploy dropped the history. Decision (Phase 5b, recorded here, not in an ADR):

- **A table, not Redis.** A snapshot is a whole source (tens of KB to MB), and the Redis of this deployment evicts
  under memory pressure (`volatile-lru`) with the budget ledger and presence in the same keyspace. The row is
  TOAST-compressed (lz4 where the server has it) like `flows.content`.
- **Cloud only.** The service puts the ring of a stored flow in the table where `capability.Of(cfg)` has
  `server-store` (`di.snapshotStoreOptions` wires `service.WithSnapshotStore`); the desktop has no store and keeps the
  in-memory ring, which also holds the snapshots of a desktop file or folder in the other profiles (they restore to a
  path, and a path is no row of `flows`). A service built without a store keeps the memory ring for stored flows too.
- **Bounds** replace the memory ring's global byte budget: the newest 20 per flow (was 10), at most 32 MiB of stored
  text per flow (member names count), a snapshot over that bound is not kept and evicts nothing, and a push applies
  both in the statement that follows its insert. Reads hide a row older than 24 hours (`FlowSnapshotTTL`) at once;
  the retention sweep deletes it (`PurgeResult.FlowSnapshots`, batched by `ctid` through `created_at`).
- **Same transaction, never fatal.** The push joins the request's transaction under a savepoint: a request that rolls
  back takes its snapshot with it, and a push that fails (the flow vanished, the text holds a NUL byte, a database
  error) leaves the transaction and the mutation intact. The service logs it and goes on; listing reports a store
  failure instead of an empty ring.
- **Tenancy.** `flow_snapshots` has the policy of the other flow-child tables (migration 0034): a row is reachable
  when its flow is, so the owner, a collaborator and an organisation member see it and nobody else does. A restore
  loads by `(flow_id, id)`, so an id taken from another flow restores nothing (404).
- **Lifecycle.** `ON DELETE CASCADE` takes the ring with its flow (account erasure, a
  plain delete). No user id is stored. Not in the export bundle: it is a day-old copy of the exported flows' source.

Rollback: the table is new and nothing else changed shape. The release before keeps its memory ring and never reads or
writes the table, so an image rollback needs no schema rollback (`expand`); `bakicli migrate down 44` drops it.
During a rolling deploy a replica of the old release answers its own undo from memory and one of the new release from
the table; each sees only what it recorded until the old ones are gone.

## Open

- An organisation whose last admin erases their account keeps its members and content and cannot be administered;
  erasure logs `account erasure left organisations without an admin`. Refusing the erasure, handing the organisation
  over or deleting it is a product decision.
- The user's flows inside an organisation are deleted with the account, as before; transferring them is a product
  decision.
- Other actors' audit records naming the user (a share's `targetUser`, an invitation's email) and the user's pre-auth
  rows (`resource_id`, IP) are kept as recorded.
- Not in the bundle yet: SSO identity links, AI usage, pending invitations to the user's email, the share links the
  user created, the version history of the owned flows, triage decisions on other people's flows.
- An anonymised comment shows no author in the UI (`authorName || authorId` are both empty).
- Flows detached from a deleted organisation keep the old organisation in a process's cached document until their
  next save (the cache is keyed by version, and detaching does not change it).
