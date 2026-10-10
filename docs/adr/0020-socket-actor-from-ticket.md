# 20. A socket acts as the credential its ticket was issued from

Date: 2026-10-10. Status: accepted.

## Context

R7 of the refactoring proposal. A browser opens the collaboration socket in two steps: `POST /api/ws-ticket` mints a
30-second, single-use ticket, and `GET /ws?flowId=&ticket=` redeems it. The ticket named the user and the source
credential's jti and expiry, but not the credential's token scopes. The socket's actor was rebuilt from the user id
alone, so `AuthzService.Can` saw an unscoped user.

The ticket route needs `read`, so a read-scoped personal access token could get a ticket. If its owner held editor
rank on a flow, the socket granted the write rank, and the token could relay `block.update`, `block.create` and
`block.delete` frames to every member of the room and over the Redis backplane. The HTTP write routes refuse that
token. A read-scoped token of a viewer was already refused; the bypass was the scope, not the role.

The per-message check itself already existed: `handleIncoming` refused `block.*` unless the connection could edit,
and that flag came from `Can(FlowEdit)` at connect and every 2 minutes. What was missing was the scopes reaching the
actor.

## Decision

1. **The ticket carries the scopes.** `IssueWSTicket` takes `auth.WSTicketSource{UserID, Email, Role, Scopes,
   AccessJTI, AccessExp}` and copies the source credential's scopes into the ticket's `scopes` claim. A session or
   an unscoped token gives no scopes, which means unscoped, as everywhere else. The ticket also carries a format
   version claim, `wsv` = `auth.WSTicketVersion` (1).
2. **A versionless ticket is refused.** It comes from a server that predates ticket scopes, and its missing scopes
   cannot be read as "unscoped". `VerifyWSTicket` and `ConsumeWSTicket` refuse it, so `/ws` answers 401. The client
   reconnects with a fresh ticket from the new server. This costs at most one reconnect within 30 seconds of a
   deploy.
3. **The socket's actor comes from the ticket.** `auth.ActorFromWSTicket` is `ActorFromClaims` read through the
   source credential's jti (`src_jti`) instead of the ticket's own. A PAT's ticket is that token, an `ActorToken`
   with its id and scopes; a session's is the `ActorUser`. The access log records this actor.
   - `internal/websocket` takes it in `Identity{Actor, DisplayName, AccessJTI, AccessExp}`. The package imports
     `internal/auth`, which imports no package that imports `internal/websocket`, so there is no cycle.
   - The `FlowAccessChecker` methods take the actor. The `internal/di` adapter passes it straight to
     `AuthzService.Can` (for the read, `AuthorizeFlow`: the same check, returning the header it read), which refuses
     `FlowEdit` to a token without `write` before any read.
   - Without a checker (no storage), the write rank is `actor.Allows(write)` instead of a literal `true`. That is
     the same for every real deployment, because API tokens need storage, and it fails closed if that changes.
4. **The per-message check is a declared table.** `inboundNeeds` in `client.go` maps each client-sendable event type
   to what sending it needs: `needJoined` (`FlowRead`, checked at the upgrade, on an access change (8) and every 2
   minutes) for `ping` and `presence.update`, and `needEdit` (`FlowEdit`: editor rank and the write scope) for
   `block.*`. `serverOnly` lists the rest. `TestInboundNeeds_CoverEveryEventType` reads the package's event type
   constants from source and fails on one that is in neither list, so a new client-sendable type cannot ship
   undeclared. This is the small stand-in for the WS dispatch registry the proposal cut.
5. **A refused frame is dropped, counted and answered, and the socket stays open.**
   - The counter is `pad_ws_inbound_refused_total{type,reason}`. The reason is `edit`, `unknown` or `invalid`. The
     type label is a declared event type or `other`, never the client's text.
   - `permission_denied` goes to the sender at most once per 5 seconds per connection. `unknown_event` and
     `invalid_message` still answer every frame; the inbound rate limiter bounds them.
   - The shipped client reconnects after every close, so closing on a refusal would turn it into a reconnect loop.
6. **Codes and the route are unchanged.** The refusal keeps the `permission_denied` code; its message names the
   write scope when the actor is a scoped token. A write-only token still cannot get a ticket, because the route
   needs `read` and route scopes are all-of.
7. **The ticket response gains `expiresAt`** (RFC 3339). This is additive; the frontend type already declared it.
8. **An access change re-checks the open sockets it can reach at once** (added with the Phase 4c open items; it
   was "not now" before). The 2-minute pass stays, as the backstop for what no event reaches.
   - *Triggers* (`websocket.SubscribeAccessRechecks`, subscriber `ws-access-recheck`): `FlowChanged` of kind
     `sharing` or `deleted` reaches the flow's room; `OrgMemberChanged` `removed` or `role` reaches that user's
     sockets, `org-deleted` the sockets on the organisation's flows; `AccountErased` reaches the account's sockets
     and those on the flows it owned (erased with it); the new `AccessRevoked{UserID, JTI}` reaches the sockets that
     user opened with that credential. A membership added only widens access; a sharing change is rechecked either
     way, as the event does not say which.
   - *Finding the sockets without storage*: the hub has rooms by flow id and each socket's user. A socket also keeps
     two things fixed at connect: its flow's owner and organisation, which `FlowAccessChecker.CheckAccess` now
     returns (`FlowOwner`; the di adapter reads them through `AuthorizeFlow`, the header it already loads), and the
     jti of its credential. Updates never change a flow's owner or organisation; deleting the organisation detaches
     the flow, which is the change the org id is kept for. A membership change reaches all of the user's sockets
     here (at most 10), not only those on the organisation's flows: narrowing would save a few lookups and would
     trust the org recorded at connect.
   - *Tier*: async, so a recheck runs only after the change committed (before, it would read the old grants and keep
     the socket) and never for a rolled-back one. With `InlineWhenSaturated`: flagging is in memory and quick, so a
     saturated bus runs it in the committing request's post-commit hook instead of dropping it.
   - *The recheck*: the subscriber only flags sockets, through a one-slot channel per socket, so it never waits and a
     burst of changes leaves one pass pending, not one per change. Each socket's own re-authz loop runs the pass the
     2-minute tick runs (the blacklist, then `CanEdit`, then `CheckAccess`): it drops the write rank, or disconnects
     once read is gone. Event passes hold one of 4 hub-wide slots (`maxConcurrentRechecks`), so an organisation
     deleted under hundreds of sockets runs 4 authz lookups at a time; a socket waiting for a slot still gets its
     periodic pass when that falls due.
   - *Admission race*: a change that commits after a socket's connect check read the grants, but before the socket
     joined its room, finds no socket to flag. The hub counts the rechecks it starts (`accessChanges`, bumped before
     it looks); a socket that sees the count moved since its connect check re-checks once after joining.
   - *Revocation*: `AuthHandler` publishes `AccessRevoked` at logout and when an API token is deleted, right after
     the blacklist write, which the recheck then finds. The handler publishes because the revocation is that write
     in `auth.Manager`, which cannot reach the bus (`eventbus` imports `auth`). The user must match as well as the
     jti: the token delete route blacklists any id it is given, and an announcement naming another user's token id
     reaches none of that user's sockets. Revoking a session (the sessions page, a password change or reset)
     revokes refresh tokens, which no access token names: the socket keeps its expiry timer, so it ends when its
     access token expires (at most 15 minutes), when the HTTP API stops accepting that token too.
   The Redis backplane message format and the presence records are unchanged; they belong to Phase 5b.

## Consequences

- A read-scoped token's socket can join and send presence but not mutations, matching the HTTP routes. A token's
  scopes cannot change (tokens are created and deleted, never edited), and deleting one blacklists `pat:<id>`, which
  the recheck turns into a disconnect (8). So the ticket needs no scope revalidation and no role snapshot: role and
  sharing are read from storage at connect, on an access change and every 2 minutes.
- Old client, new server: nothing visible. The shipped frontend sends only `presence.update` and `ping`, and the
  ticket response only gained a field. Rollback: a new server's ticket on an old server is accepted, its scopes and
  `wsv` ignored, which is the behaviour before this change.
- During a Phase 5b mixed-version rollout an old replica could admit a read token's mutation, and new replicas would
  relay it, because peer frames are delivered without a check. Today this is moot: one replica, Redis off. It
  belongs in 5b's failure-mode table; the backplane message gains no sender authority here.
- `internal/api` has its first end-to-end socket test through the real router (`ws_e2e_test.go`), and the authz
  matrix gains `POST /api/ws-ticket`.
- A removed collaborator or member, a deleted flow or organisation, an erased account, a logged-out session or a
  deleted API token loses its open sockets on the replica that made the change as soon as the change commits (tens
  of milliseconds in `ws_recheck_e2e_test.go`), and a demoted editor loses the write rank as fast. The client sees
  the close the periodic pass gave it, only sooner.
- Cost: an event scans this replica's sockets in memory under the hub's read lock, then one authz pass (one or two
  point lookups) per flagged socket, at most 4 at a time and coalesced per socket. A share-link change also
  rechecks its flow's room.
- Under load nothing is dropped silently: the delivery runs inline when the bus is saturated, flagging never
  waits, and a socket queued for a slot still gets its periodic pass. What is lost is what the bus loses (a crash
  between the commit and the delivery, ADR 0009) and the other replicas. A flood of sharing changes on one flow
  costs its room one pass per socket at a time, and can hold the 4 slots, which delays other sockets' event passes
  but never their periodic pass.
- Other replicas: events are in process, so a peer replica's sockets of the same flow, user or organisation learn
  of the change at their periodic pass (up to 2 minutes). Today there is one replica (Phase 5b gates `maxReplicas`),
  and during a rolling deploy the old replica closes its sockets at shutdown. A cross-replica recheck over the
  backplane was considered and not built: a `ws:recheck` pub/sub channel beside the room channels would be cheap and
  safe with mixed versions (an old replica does not subscribe to it), but it defines a Redis message that Phase 5b's
  `{v, kind}` envelope, previous-release fixtures and failure-mode table are meant to own, and it protects nothing
  while there is one replica. Revocations already cross replicas in the token blacklist (`baki:auth:revoked`), so a
  peer's next pass finds them; only the prompt nudge stays local.
- `FlowAccessChecker.CheckAccess` returns `(FlowOwner, error)`. Rollback of (8): no schema and no wire change; the
  previous release has the periodic pass only.

## Open

- Cross-replica rechecks, with the backplane envelope (Phase 5b). For revocations, a listener on the blacklist's
  peer channel could nudge the peers' sockets without a new message.
- A global role change (`UserAdminService.ChangeRole`) announces nothing; it matters only for ownerless legacy
  flows, which instance admins may read. The periodic pass catches it.
- A revoked session reaches an open socket at its access token's expiry, as it reaches the HTTP API; access tokens
  name no session.
- Fixed since: an SSE stream of a revoked token ends at once on the replica that revoked it
  (`EventManager.SubscribeRevocations`, the same `AccessRevoked` event), its 2-minute blacklist check covering the
  others; and the token delete route revokes, announces and audits only a token the caller owns (it blacklisted any
  id it was given, so another user's token, if its id was known, was cut off from sockets for 10 minutes).
