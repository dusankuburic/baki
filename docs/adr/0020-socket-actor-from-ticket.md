# 20. A socket acts as the credential its ticket was issued from

Date: 2026-10-10. Status: accepted.

## Context

The collaboration socket's ticket named the user and the source credential's jti/expiry but not its token scopes, and the socket's actor was rebuilt from the user id alone — so a read-scoped personal token whose owner held editor rank could relay `block.update`/`create`/`delete` frames that the HTTP routes refuse it.

## Decision

1. The ticket carries the scopes: `IssueWSTicket` takes `auth.WSTicketSource{UserID, Email, Role, Scopes, AccessJTI, AccessExp}` and copies the source credential's scopes into the ticket's `scopes` claim, plus a format version claim `wsv` = `auth.WSTicketVersion` (1). A session or unscoped token gives no scopes (meaning unscoped).
2. A versionless ticket is refused (401): its missing scopes cannot be read as "unscoped". Costs at most one reconnect within 30 s of a deploy.
3. The socket's actor comes from the ticket: `auth.ActorFromWSTicket` is `ActorFromClaims` read through the source credential's jti (`src_jti`) — a PAT's ticket is that token (an `ActorToken`), a session's the `ActorUser`. `FlowAccessChecker` methods take the actor; without a checker the write rank is `actor.Allows(write)` (fails closed).
4. The per-message check is a declared table: `inboundNeeds` maps each client-sendable event type to `needJoined` (`FlowRead`) or `needEdit` (`FlowEdit`: editor rank and the write scope, for `block.*`); `TestInboundNeeds_CoverEveryEventType` fails on an undeclared type.
5. A refused frame is dropped, counted (`pad_ws_inbound_refused_total{type,reason}`; the type label is a declared event type, never client text) and answered; the socket stays open. `permission_denied` goes to the sender at most once per 5 s per connection. The ticket response gains `expiresAt` (RFC 3339, additive).
6. **An access change re-checks the open sockets it can reach.** Triggers (`websocket.SubscribeAccessRechecks`, subscriber `ws-access-recheck`): `FlowChanged` of kind `sharing`/`deleted` (the flow's room), `OrgMemberChanged` `removed`/`role`/`org-deleted` (the user's sockets), `AccountErased`, and `AccessRevoked{UserID, JTI}` (sockets opened with that credential; published by `AuthHandler` at logout and token delete). Async tier (after commit) with `InlineWhenSaturated`. The subscriber only flags sockets through a one-slot channel per socket; each socket's re-authz loop runs the pass (blacklist, `CanEdit`, `CheckAccess`), dropping the write rank or disconnecting when read is gone; event passes hold one of 4 hub-wide slots. A change committing between a socket's connect check and its room join is caught by a recheck counter (a socket seeing the count move re-checks once after joining). Revoking a session ends its sockets at the access token's expiry (≤15 min) — access tokens name no session.

## Consequences

A read-scoped token's socket can join and send presence but not mutations, matching the HTTP routes; token scopes cannot change and deleting a token blacklists `pat:<id>`, which the recheck turns into a disconnect. A removed collaborator or member, deleted flow/org, erased account or logged-out session loses its sockets on the acting replica as soon as the change commits; peer replicas learn at their periodic pass (≤2 minutes; cross-replica recheck deferred to Phase 5b — revocations already cross via the `baki:auth:revoked` blacklist). Rollback: a new server's ticket on an old server is accepted, scopes and `wsv` ignored. The token delete route revokes, announces and audits only a token the caller owns.
