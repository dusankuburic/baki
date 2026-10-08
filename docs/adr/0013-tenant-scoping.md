# 13. Row-level security scopes visibility; the service authorizes roles

Date: 2026-10-08. Status: accepted.

## Context

A request that runs in the request transaction sets `app.current_user_id`, and the row-level policies then show
only the rows of flows (or organisations) the caller can see: as owner, collaborator or org member. Without a user
context the policies allow every row. That covers the background jobs, the routes outside the request transaction
(`route.NoTx`, public routes) and the desktop. Roles (viewer, editor, admin) are decided by `AuthzService.Can`.

Phase 3 looked for storage methods that let a caller reach another tenant's rows just because the table had no
policy:

- `flow_analysis` (the dashboard summary) and `share_tokens` had no row-level security. `share_tokens` was left
  without it on the belief that the anonymous `/api/shared` lookup needs that. It does not: the lookup runs without a
  user context.
- `flow_collaborators` cannot have a policy. Every flow policy reads it, so a policy on it would recurse.
- `DeleteFindingComment` with an empty author deleted any comment.
- The governance scanner's batch reads and `FlowFilter.AllFlows` read across tenants by design, with nothing marking
  them as such.
- The triage and baseline policies let any viewer of a flow write, while the service requires `FlowEdit`.

## Decision

1. **Row-level security scopes visibility, not roles.** Every flow-child table carries the visibility policy that
   delegates to `flows` (v27). Migration v34 adds it to `flow_analysis` and `share_tokens`. The viewer, editor and
   admin checks stay in `AuthzService.Can`, which the services call before the writes. There is no edit-level policy,
   for three reasons:
   - `AuthzService` is the single source of truth. A SQL copy of `FlowEdit` (owner, org admin or member, editor or
     admin collaborator, never an ownerless flow) would drift from it, and a policy stricter than the service fails
     a legitimate write with a 500.
   - Account erasure runs in the departing user's own scope. It rewrites their identity on flows they may only view,
     such as `finding_status.updated_by` after a demotion. An edit-level policy would skip those rows without an
     error.
   - The batch triage upsert would evaluate a role subquery for every row.

   `TestFlowRecordWrites_ViewersRefusedAtTheService` proves that every viewer kind gets 403 on every triage,
   baseline, comment and share-link write. `TestRLS_ScopedMethods_*` proves that a stranger reaches none of these
   rows inside their own scope.
2. **A table without row-level security is listed with its reason** in `TestRLS_TablesWithoutRowSecurityAreListed`,
   so a new table cannot ship unscoped by omission.
   - The grant tables (`org_members`, `flow_collaborators`) stay without a policy. The collaborator statements reach
     their grants through `flows` instead (`EXISTS (SELECT 1 FROM flows ...)`). Inside a scope, a flow the caller
     cannot see has no grants to list, and a write on it answers `ErrNotFound`.
   - `organisations` stays without a policy too. The authorization checks read it (an unknown organisation answers
     differently from a refusal), invite acceptance reads it before the invitee is a member, and so does the
     realtime roster at delivery. Every caller is authorized at the service.
3. **Comment deletion names its author.** `DeleteFindingComment` deletes only the author's own comment, and an empty
   author matches no one (`ErrNotCommentAuthor`). Moderation is a separate method,
   `DeleteFindingCommentAsModerator`, which the handler calls only after a `FlowShare` check.
4. **System reads are pinned.** `FlowFilter.AllFlows` and `GetFlowBaselinesBatch` reach every tenant when they run
   without a scope. Only the files listed in `TestArch_SystemReadsPinned` may use them: the governance scanner and
   the legacy migrator.

## Consequences

- The policies still do nothing where no user context is set. Routes outside the request transaction rest on the
  service's authorization and the explicit predicates of their statements. For the transactional routes, the
  policies are defense in depth.
- v34 is `expand`. The previous release reads `flow_analysis` and `share_tokens` only for flows the caller can see,
  or without a scope, so the policy hides nothing it reads.
- Inside a user's scope, a share token of a flow they cannot see does not resolve. No route looks one up that way:
  the share-link routes are public.
- Statements that rewrite a departing user's identity run in that user's scope. They reach only the flows the user
  can still see, so rows on flows they have lost access to keep their id. This predates v34, and the data-lifecycle
  work has to run those statements where the policies do not filter.
- `CommentStore` gains `DeleteFindingCommentAsModerator`.
