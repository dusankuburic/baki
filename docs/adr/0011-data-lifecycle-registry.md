# 11. A data-lifecycle registry with a schema coverage test

Date: 2026-10-08. Status: accepted.

## Context

Export, account erasure, organisation deletion and the retention sweep were central lists nothing tied to the schema; a new table was silently left out of all four, and several already were (erasure left the user's identity on other people's rows; org deletion left the knowledge base, channels, custom rules and policies behind).

## Decision

- Each domain declares its tables in a `lifecycle_<domain>.go` file of `internal/storage/database`, registered from `init`: a `tableClaim` per table (export section or why none; how erasure reaches the rows — step, ON DELETE CASCADE, or no user identity and why; what org deletion does, including "kept as history"; why the sweep leaves it alone) plus the export/erase/org-delete/purge steps.
- Order is explicit: erase phases `eraseDetached`, `eraseOwned`, `eraseIdentity`, `erasePersonal`, `eraseAccount`; org-delete phases `orgContent`, `orgDetach`, `orgRow`; purge steps single statements first, then batched, then windowed; within a phase by table name. Both export paths run the same sections byte for byte.
- Coverage is tested: `TestPostgres_Lifecycle_EveryTableClaimed` requires every migrated table claimed exactly once and every cascade claim backed by a real foreign key; `TestPostgres_Lifecycle_ErasureSweep` / `DeleteOrgSweep` erase/delete a seeded user or org and search every text, uuid and JSON column for the id and email.
- Lifecycle statements run without the row-level-security user (`asSystem`), inside the caller's transaction: they are keyed by one user's or organisation's id, and a step must be keyed by that id and nothing broader.
- Erasure keeps shared content and drops identity: comments, triage decisions, share links and invitations the user made on others' flows stay with author/assignee/creator cleared; an org the user owns survives without an owner (logged if left without an admin). Organisation deletion deletes org-owned content, detaches its flows to their owners (`org_id = ''`), and keeps the id on usage, alerts and the audit trail as history.
- The registry stays in the package; there is no fx group. The service-level part of erasure (releasing credentials, `AccountErased`) stays in `AccountLifecycleService`.

## Consequences

A new table fails the coverage test until its domain claims it — the failure message names the file to edit. Erasure is still one transaction; its statements bypass the policies by design.
