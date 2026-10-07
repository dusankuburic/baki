# 9. Domain events on an in-process bus, in a sync and an async tier

Date: 2026-10-07. Status: accepted.

## Context

A change in one service had to reach whatever another service derived from it: the dashboard cache of a flow's
owner, the chat's scrubbed context of the flow, a provider's cached model listing, the realtime clients. Each link
was a setter wired at boot (`SetCloudFlowMutationHook`, `OnInvalidateFlow`, `OnApiKeyChanged`, `OnUserDeleted`)
or a direct call into the other service, so every new consumer meant editing the producer and the DI graph. Some
invalidations ran before the transaction committed: a reader could refill the cache from the old row in between,
and the stale entry then lived until its TTL. The SSE `Emit` reached every connected client of every tenant unless
the caller remembered `EmitTo`.

## Decision

- **Event types** live in `internal/events`, which imports nothing of the module: `FlowChanged`,
  `CredentialChanged`, `OrgMemberChanged`, `SettingsChanged`, `KnowledgeDocChanged`, `FindingStatusChanged`,
  `AccountErased`. An event is a snapshot that names what a subscriber needs (ids, owner, organisation, kind) and
  nothing secret; fields are only added.
- **The bus** (`internal/platform/eventbus`) depends on no domain package (`internal/archtest`). Services publish
  with `bus.Publish(ctx, e)`; subscribers register with `eventbus.Subscribe[E](bus, tier, name, fn)` at boot, from an
  `fx.Invoke`, never from the publisher.
- **Sync tier: invalidation.** The subscriber runs inside `Publish`; when `Publish` ran inside an open transaction,
  it runs again after the commit (`eventbus.Committed`) or the rollback (`eventbus.RolledBack`), through
  `txhook.Register` and the new `txhook.OnRollback`. Sync subscribers are quick and never fail the publisher (a
  panic is recovered and counted). Fan-out to other replicas (Phase 5b) is published from the committed pass only.
- **Fills check a generation** (`eventbus.Gen`): a reader captures the key's generation before reading the source
  and stores what it read only if `FillOK`: no invalidation since, and its own transaction has published nothing
  (`txhook.Published`: it may be reading its own uncommitted writes). A read-only request inside its RLS
  transaction still fills. Generations are striped (256 counters per cache), so they cost no memory per key; a
  collision only skips a fill.
- **Async tier: side effects** (email, notifications, realtime, disabling lent credentials). The subscriber runs
  only after the commit, never for a rolled-back write, on the bus's `lifecycle.Bounded` runner ("events", 64
  concurrent, drained at shutdown), each delivery with a 30 s timeout and its own panic recovery. Its context has
  the publisher's actor, claims and request info but not its cancellation or its row-level-security scope: a
  subscriber that reads or writes the database chooses its scope explicitly. A saturated runner drops the delivery
  and counts it, except for a subscriber registered with `eventbus.InlineWhenSaturated()`, which then runs inline in
  the committing request's post-commit hook: work that must not be lost and is quick (revoking a departed lender's
  shared credential; handing a comment's or an assignment's bell rows to their own runner).
- **Metrics:** `pad_events_published_total{event}`, `pad_event_subscriber_failed_total{event,subscriber,reason}`
  (panic, timeout, dropped), `pad_event_subscriber_duration_seconds{event,subscriber,tier}`.
- **Tests** use `eventbus.NewRecorder(bus)` to assert what was published, and `eventbus.New(nil, …)` to run async
  deliveries inline.
- **SSE audience is mandatory.** `service.EventNotifier` has no bare broadcast: `EmitTo(user)`, `EmitToOrg(ctx, org)`
  (the roster is read where the event is delivered) or the explicit `EmitToAll`. An empty user is `LocalUser`,
  the identity-less clients of a deployment without accounts, never every client.
- **WebSocket `flow.changed`** goes out from an async subscriber of `FlowChanged` (content updates only), after the
  commit, with a millisecond stamp rather than the stored version: a viewer may already record the new version over
  content its parallel reload read just before the commit, and would skip a notification carrying that version.

## Consequences

A new consumer of a change is one subscriber in its own module; the producer does not change. Delivery is in
process and at most once: a crash between the commit and an async delivery loses that delivery, which is acceptable
for caches (they also expire) and for best-effort notifications. A durable outbox is deferred until a consumer
needs guaranteed delivery. Sync subscribers run on the publisher's goroutine, so a slow one slows every write that
publishes its event; the duration histogram shows it.
