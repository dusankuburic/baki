# 11. A data-lifecycle registry with a schema coverage test

Date: 2026-10-08. Status: accepted.

## Context

Four operations decide what happens to stored data: the data-subject export, account erasure, organisation
deletion and the retention sweep. Each was a central list in `postgres_retention.go` (the export struct assembled
twice, once buffered and once streamed by hand; the erasure's statement list; the sweep's table list), and
organisation deletion was a single `DELETE FROM organisations`. Nothing tied the lists to the schema, so a new table
was silently left out of all four, and several already were: erasure left the conversations of the user's own flows
behind (no foreign key), and the user's id, name and email on other people's rows (comments, assignments, share
links, invitations, teammates' notifications, organisation ownership); organisation deletion left the knowledge base,
channels, custom rules and policies behind (no foreign key); the export included flows shared with the user although
its contract is "own data only", and ignored a settings read error. Erasure also ran inside the request's
row-level-security transaction, so its statements could not reach the user's rows on flows the user could no longer
open.

## Decision

- **Each domain declares its tables** in a `lifecycle_<domain>.go` file of `internal/storage/database`, registered
  from `init` like the analyzer's rules: a `tableClaim` per table (which export section carries the user's rows or why
  none does; whether erasure reaches the rows by a step, by an ON DELETE CASCADE foreign key, or the table holds no
  user identity and why; what organisation deletion does with them, including "kept as history" with the reason; why
  the sweep leaves the table alone when no purge step covers it; and the legacy migrator step that fills it), and the
  steps that act on them: export sections, erase steps, org-delete steps and purge steps.
- **Order is explicit.** Erase steps run by phase (`eraseDetached`, `eraseOwned`, `eraseIdentity`, `erasePersonal`,
  `eraseAccount`), org-delete steps by phase (`orgContent`, `orgDetach`, `orgRow`), purge steps single statements
  first, then the batched tables, then the windowed ones; within a phase by table name, never by file. Export
  sections follow the field order of `interfaces.UserDataExport`, and both export paths run the same sections, so
  the streamed bundle is the buffered one byte for byte.
- **Coverage is tested.** `TestPostgres_Lifecycle_EveryTableClaimed` requires every table of the migrated schema to be
  claimed exactly once, every cascade claim to be backed by an ON DELETE CASCADE foreign key to a table the operation
  removes, and the organisation classification to match the table's `org_id` column. `TestLifecycle_*` (no database)
  check the claims against the registered steps, the sections against the bundle and the purge steps against
  `PurgeResult`. `TestPostgres_Lifecycle_ErasureSweep` erases a user seeded into every table that can name them and
  searches every text, uuid and JSON column for the id and the email; `TestPostgres_Lifecycle_DeleteOrgSweep` does the
  same for an organisation's id.
- **Lifecycle statements run without the row-level-security user** (`asSystem`), inside the caller's transaction:
  they are keyed by one user's or organisation's id, the caller is authorised for it at the service, and they must
  reach that id's rows whoever's flow they hang off. Ordinary reads and writes keep their scope.
- **The registry stays in the package; there is no fx group.** Every step is SQL inside the erasure's, the
  deletion's or the sweep's transaction, and every table is created by this package's migrations; contributions from
  outside would need the transaction and the step types exported for no current user. The service-level part of
  erasure (releasing credentials, disabling the organisation credentials the user lent, `AccountErased`) stays in
  `AccountLifecycleService`. The legacy migrator's step list (`internal/migration`) is referenced by name.
- **Erasure keeps shared content and drops identity.** Comments, triage decisions, versions, share links and
  invitations the user made on other people's flows and organisations stay with their author, assignee, creator or
  inviter cleared; teammates' notifications naming the user say "a teammate"; notifications addressed to the user go.
  An organisation the user owns stays with its members and content, without an owner; one left without an admin is
  named in a warning log line. The audit trail keeps the rows about the user (as a target) and other actors' records
  naming the user, as before.
- **Organisation deletion** deletes the configuration and content the organisation owns, detaches its flows to their
  owners (`org_id = ''`), and keeps the id on usage, alerts and the audit trail as history.

## Consequences

A new table fails the coverage test until its domain says what the four operations do with it, which is the point:
the failure message names the file to edit. A new export section is a field of `UserDataExport` plus one `section`
declaration. Erasure statements bypass the policies by design, so a step must be keyed by the user's own id or email
and nothing broader. Erasure is still one transaction; its statement count grew by the new identity steps. The open
product decisions (an organisation left without an admin, the user's flows inside an organisation being deleted with
the account, other actors' audit records naming the user) are recorded in the code and the architecture notes, not
decided here.
