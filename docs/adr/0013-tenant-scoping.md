# 13. Row-level security scopes visibility; the service authorizes roles

Date: 2026-10-08. Status: accepted.

## Context

A request transaction sets `app.current_user_id` and the policies show only visible rows; without a user context the policies allow every row (background jobs, `route.NoTx` routes, the desktop). `flow_analysis` and `share_tokens` had no policy, `DeleteFindingComment` with an empty author deleted any comment, and cross-tenant batch reads were unmarked.

## Decision

1. RLS scopes visibility, not roles. Every flow-child table carries the visibility policy delegating to `flows` (v27); v34 adds `flow_analysis` and `share_tokens`. Viewer/editor/admin checks stay in `AuthzService.Can`, called by the services before writes. No edit-level policy: a SQL copy of `FlowEdit` would drift from `AuthzService` (and a stricter policy fails legitimate writes with a 500), account erasure runs in the departing user's own scope and must reach rows they may only view, and the batch triage upsert would evaluate a role subquery per row. `TestFlowRecordWrites_ViewersRefusedAtTheService` and `TestRLS_ScopedMethods_*` pin both halves.
2. A table without RLS is listed with its reason in `TestRLS_TablesWithoutRowSecurityAreListed`, so nothing ships unscoped by omission: the grant tables (`org_members`, `flow_collaborators` — every flow policy reads it, a policy would recurse; collaborator statements reach grants through `flows`) and `organisations` (authorization checks, invite acceptance and the realtime roster read it before membership; every caller is authorized at the service).
3. `DeleteFindingComment` deletes only the author's own comment; an empty author matches no one (`ErrNotCommentAuthor`). Moderation is `DeleteFindingCommentAsModerator`, called only after a `FlowShare` check.
4. System reads are pinned: `FlowFilter.AllFlows` and `GetFlowBaselinesBatch` reach every tenant when run without a scope, and only the files listed in `TestArch_SystemReadsPinned` may use them (the governance scanner, the legacy migrator).

## Consequences

v34 is `expand` (the previous release reads `flow_analysis` and `share_tokens` only for flows the caller can see, or without a scope, so the policy hides nothing it reads). The policies do nothing where no user context is set; transactional routes get them as defense in depth. Statements rewriting a departing user's identity run in that user's scope and miss rows on flows the user lost access to — the data-lifecycle work (ADR 0011) runs them where the policies do not filter.
