# 9. Domain events on an in-process bus, in a sync and an async tier

Date: 2026-10-07. Status: accepted.

## Context

Cross-service reactions were setter hooks wired at boot (`SetCloudFlowMutationHook`, `OnInvalidateFlow`, `OnApiKeyChanged`, `OnUserDeleted`) or direct calls; every new consumer edited the producer and the DI graph, and some invalidations ran before the commit, letting a reader refill a cache from the old row.

## Decision

- Event types live in `internal/events` (`FlowChanged`, `CredentialChanged`, `OrgMemberChanged`, `SettingsChanged`, `KnowledgeDocChanged`, `FindingStatusChanged`, `AccountErased`): snapshots naming ids/owner/org/kind and nothing secret; fields are only added. The bus (`internal/platform/eventbus`) depends on no domain package; subscribers register with `eventbus.Subscribe[E](bus, tier, name, fn)` at boot from an `fx.Invoke`, never from the publisher.
- **Sync tier (invalidation):** runs inside `Publish`, and when `Publish` ran inside a transaction it runs again after the commit (`eventbus.Committed`) or rollback (`eventbus.RolledBack`) through `txhook`. Sync subscribers are quick and never fail the publisher (panics recovered and counted). Cache fills check a generation (`eventbus.Gen`, `FillOK`, `txhook.Published`): a fill stores only if no invalidation happened since.
- **Async tier (side effects: email, notifications, realtime):** runs only after commit, on a `lifecycle.Bounded` runner (64 concurrent, drained at shutdown), 30 s timeout per delivery with panic recovery; its context keeps the publisher's actor and claims but not its cancellation or RLS scope. A saturated runner drops and counts, except a subscriber registered `eventbus.InlineWhenSaturated()` (must-not-lose, quick work).
- **SSE audience is mandatory:** `EmitTo(user)`, `EmitToOrg(ctx, org)` or explicit `EmitToAll`; an empty user is `LocalUser`, never every client. WebSocket `flow.changed` goes out from an async subscriber of `FlowChanged` after commit, with a millisecond stamp rather than the stored version.
- Metrics: `pad_events_published_total{event}`, `pad_event_subscriber_failed_total{event,subscriber,reason}`, `pad_event_subscriber_duration_seconds{event,subscriber,tier}`. Tests use `eventbus.NewRecorder(bus)` and `eventbus.New(nil, …)` (inline async).

## Consequences

A new consumer is one subscriber in its own module. Delivery is in process and at most once: a crash between commit and an async delivery loses it (acceptable for caches and best-effort notifications; a durable outbox is deferred). Sync subscribers run on the publisher's goroutine — a slow one slows every write that publishes its event.
