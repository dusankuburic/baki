# 32. Cache invalidation fan-out, access rechecks across replicas, and the per-process state registry

Date: 2026-10-13. Status: accepted, implemented (Phase 5b, wave 2).

## Context

Cache invalidation is an in-process event: a service publishes a domain event (ADR 0009) and the subscribers that
hold something derived from it drop it. With two replicas behind a load balancer the second replica never hears
of it. Wave 1 (`eventbus.Replicated`, `Bus.OnRemote`, `Bus.DeliverRemote`, `subscribers.golden`) classified every
subscriber and pinned a seam; nothing crossed a process yet. What a second replica saw:

- **Stale configuration for the TTL.** An admin lowers the daily AI budget or disables a provider on A: B enforces
  the old settings for up to 30 s. A rule or membership change leaves B analysing with the old org profile for 30 s,
  a credential change leaves B's model listing for 10 minutes, an upload leaves B's corpus count for 30 s, and "my
  upload is not on Home" is the dashboard's 30 s.
- **A 2-minute window on revocations.** A logout, a changed sharing, a removed member or a deleted API token closes
  the sockets and streams of the replica that processed it at once. The other replicas' sockets re-checked at their
  periodic pass (2 minutes) and their SSE streams at the blacklist check (2 minutes). ADR 0020 accepted this and
  deferred a `recheck` message to the realtime envelope.
- **Two pieces of state that are not memos.** `flow.upload-ids` mints a new flow id on a miss, so an identical
  re-upload through the other replica created a second flow. The WebSocket ticket replay guard was a per-process
  map (the Postgres blacklist is the authority, but the guard in front of it was local).
- **No list of what else is per process.** `undeclaredMemos` named eight memos; nothing forced a new map, ring or
  registry onto any list, and the surveys of Phase 5b found about fifty.

## Decision

### 1. One channel, keys only

`internal/invalidation` implements `eventbus.Replicas` over Redis pub/sub. The channel is `baki:inval`, declared
with `redisx.Declare` (name `inval.fanout`, version 1, on-down open, kinds `inval`, `recheck`, `hb`, `resync`). A
message is the envelope of ADR 0029 plus the key, its scope, the sender's epoch and the sender's origin (its instance id
and a random suffix per boot, so two replicas that share an instance id still tell their own messages from the other's,
and a restarted replica numbers from one):

```json
{"v":1,"kind":"inval","key":"rules.profiles","scope":{"org":"o1","user":"u1"},"epoch":17,"o":"app-a#3f9c0a11"}
```

- **Keys, never events.** What travels is an invalidation key (a declared cache name, or a `recheck.*` nudge) and
  the tenant parts of what changed (ids only; no free text, nothing secret). The replicated subscribers write
  the scope; the cache owner's applier reads it. The scope vocabulary per key is in
  [safety-nets.md](../architecture/safety-nets.md#event-subscribers-internalditestdatasubscribersgolden).
- **Published after the commit only.** The bus hands each distinct `(key, scope)` of an event to the hook through
  `txhook`: after the publisher's transaction committed (at once where there is none), never for a rollback, after
  this replica's own Committed pass. The hook enqueues; a goroutine does the `PUBLISH`. A writer never waits on
  Redis, and a failure never fails a commit.
- **Applied by the owner's own drop.** `Bus.OnRemote(key, apply)` is registered next to the subscriber that
  publishes the key; the receiver calls `DeliverRemote(key, scope)`, which runs the same generation bump and delete
  the local subscriber performs, so a fill in flight is refused. It never publishes, so a message cannot loop. A
  replica drops its own origin's messages. An empty scope means the whole cache (the caches that lacked a
  whole-cache drop gained one: `ProviderService.InvalidateAllModels`, `KnowledgeService.InvalidateAllCorpora`,
  `cache.Typed.Clear`).
- **Order is kept.** One FIFO queue, one publishing goroutine, one subscription goroutine: the settings cache is
  dropped before the rule profiles resolved from it. Identical messages still waiting are merged.

### 2. Bounding what a lost message costs

Fan-out is best effort. Staleness is bounded three ways, in order of strength:

1. **Epoch gap check.** A replica numbers its messages 1, 2, 3, ...; a message it could not send (Redis degraded,
   the publish failed) still takes its number. A receiver tracks the last epoch per origin: a skipped number, or a
   heartbeat (every 10 s, carrying the last epoch sent) ahead of the last message seen, is a gap and triggers a
   resync. An epoch that goes back is an origin that restarted; the first sight of an origin is its baseline. The
   queue is bounded (512): a burst past it collapses into one `resync` message, which reaches the peers after every
   write it replaces committed.
2. **Resync.** On a gap, on every (re)subscription once the server confirmed it, on Redis recovering
   (`redisx.Health` down to up) and on a peer's `resync` message, the replica calls `DeliverRemote(key, nil)` for
   every replicated key: every shared-class cache is dropped (the generation is bumped, so fills in flight are
   refused) and every open socket re-checks its access. Resyncs run at most once per 5 s and coalesce.
3. **The TTLs.** Unchanged: 30 s for settings, rule profiles, corpus counts and the dashboard views, 10 min for model
   listings. They are all there is while Redis is down, and all an older release that does not subscribe has.

### 3. `cache.Shared("redis-inval")`

The six caches the survey named are declared `cache.Shared(cache.MechanismInvalidation, reason)` instead of
`ReplicaLocal`: `settings.deployment`, `rules.deployment-profile`, `rules.profiles`, `providers.model-listings`,
`rag.corpus-counts`, `dashboard.home` and `dashboard.analytics` (seven specs). `Shared` now takes a mechanism and a
reason (the golden prints `shared: redis-inval: <reason>`). The storage stays in each replica's memory, so `cache.New`
builds it; it refuses any other `Shared` mechanism, because `Typed` cannot share entries. This is static, not "when a
replicator is installed": without Redis the cache behaves as a replica-local one and the fan-out is simply absent,
which a single replica and the desktop need. `TestSubscribers_ReplicatedCachesAreSharedAndSharedCachesAreReplicated`
ties the caches golden to the subscribers golden in both directions. The caches that stay `ReplicaLocal` are the ones
the survey classified as needing no signal (keyed by version, revalidated on every read, pure functions).

### 4. Rechecks on the same channel

`recheck.ws-access` and `recheck.sse-revocation` (the replicated subscribers wave 1 classified) go out as kind
`recheck`. The receiver's applier flags this replica's sockets exactly as the local change would
(`Hub.RecheckFlow`, `RecheckUser`, `RecheckOrg`, `RecheckOwner`, `RecheckCredential`) or ends the streams of the
revoked token (`EventManager.Revoke` through `ApplyRemoteRevocation`). The origin has already done its own. A
credential recheck keeps the user match of ADR 0020: a message naming another user's credential reaches none of
theirs. This closes the 2-minute window to a message's latency. A resync is the empty scope: every socket
re-checks (bounded by the hub's existing concurrency cap); the SSE streams keep their periodic blacklist check as
the backstop for a revocation whose message was lost, because a resync names no token.

### 5. The drops that were not events

Three direct drops reached only the replica that ran them: `DashboardService.RecordAnalysis` (an analysis is not a
`FlowChanged`), the `ProviderService` methods that save, replace or revoke a key without a `CredentialChanged` (the
GitHub and Copilot sign-ins, the local and Azure configurations, an org's Azure credentials), and
`DisableOrgAzureCredentialsSavedBy`. They announce themselves with `Bus.Invalidate(ctx, key, scope)`: after the
commit, never for a rollback, only a key some replicated subscriber declares (so the subscribers golden stays the
complete list of what crosses replicas). The drops that an event already covers (`PrepareOrgDeletion`, the account
erasure release) stay local: `OrgDeleted` and `AccountErased` carry them.

### 6. Two more pieces of state on Redis, with a local fallback

- **`flow.upload-id`** (`baki:flow:upload-id:{user}:{digest}`, 24 h): the flow id an upload minted, claimed with
  `SET NX` so the first of two replicas parsing the same bytes wins and the other adopts its id; an id whose flow was
  deleted is replaced, except within 15 s of the claim, when the claiming replica is still saving the flow. Every
  error falls back to the replica's memo, which is also all there is without Redis. A miss is harmless: a second
  flow, as after the memo's TTL.
- **`ws.ticket`** (`baki:ws:ticket:{jti}`, up to 2 min): `SET NX EX` by the ticket's jti in front of the in-memory
  guard. The Postgres-backed blacklist stays the authority across replicas and fails closed; this guard fails open.

### 7. The state registry

`TestArch_PerProcessStateDeclared` (`internal/archtest/state_test.go`) finds every keyed collection beside a lock
(a struct with a mutex or `sync.Map` and a map, `list.List` or LRU field) and every package-level map, `sync.Map`
or cache made empty and filled later (about sixty), and requires each to be an entry of `stateRegistry`. An entry
has a locality and a reason: replica-local (why that is acceptable), shared (its mechanism and a `Test` function
that proves it, which must exist), database, request-scoped, config, desktop-only, test-only, or composite (a type
that mixes, with `Type.field` entries). It absorbs `undeclaredMemos`. A reason that begins "known gap" names a
cross-replica defect that is not fixed (the scanner's dedup memory, the analysis report pair and record claims).
Entries for mechanisms other agents of the same phase build (the SSE hub, the chat stream registry, the budget
ledger) say replica-local now and are flipped to shared when those land.

## Failure-mode table (Redis down) additions

| Mechanism (declared name) | On down | What degrades | Recovery |
|---|---|---|---|
| Cache invalidation and rechecks (`inval.fanout`) | open: a writer is never blocked; a message not sent is counted (`pad_inval_dropped_total`) and takes its epoch | other replicas serve the old value for the cache's TTL (30 s; model listings 10 min) and keep a revoked socket until its periodic pass (2 min) | the subscription re-establishes itself; each replica resyncs on resubscribe, on Redis recovering and on any gap |
| Upload ids (`flow.upload-id`) | open: the replica's own memo | an identical upload through another replica creates a second flow | the next claim |
| WS ticket guard (`ws.ticket`) | open: the in-memory guard and the Postgres blacklist | none for correctness (the blacklist is the authority) | none needed |

## Consequences

- A rolled-back write publishes nothing; a committed one reaches a peer in a message's latency, instead of a TTL.
- Metrics, all owned by `internal/invalidation`: `pad_inval_published_total{kind}`, `pad_inval_received_total{kind}`,
  `pad_inval_dropped_total{reason}`, `pad_inval_ignored_total{reason}`, `pad_inval_gap_total`,
  `pad_inval_resync_total{reason}`. What the decoder made of each payload is `pad_redis_msgs_total{kind}` of ADR 0029.
  A sustained `pad_inval_gap_total` rate means the channel loses messages; a rising `unknown_key` during a rolling
  deploy is the other release's newer caches.
- A resync after every resubscription costs a cold cache and a recheck of every socket (each bounded by the hub's
  slots); a flapping Redis is rate-limited to one per 5 s.
- A new per-process map, list or cache fails `TestArch_PerProcessStateDeclared` until its author has decided what a
  second replica sees of it. The agents building the SSE hub, the stream registry and the ledger in the same phase
  must add their types to the registry when they merge.
- The channel is a trust boundary of the deployment, like `baki:auth:revoked`: whoever can publish to it can drop
  caches, make sockets re-check, and end the streams of a token whose jti they know. It cannot read data, grant
  access, or change a value.

## Rollback

Additive. An older replica does not subscribe to `baki:inval` and ignores nothing it needs: it keeps the TTL
windows, as before. A new replica receiving nothing from an older one falls back to its TTLs and the resync on
resubscribe. The wire shape is versioned (`v:1`, unknown versions and kinds are counted and ignored), so a later
release can add a field or a kind. Reverting the binary leaves `baki:inval` unused and `baki:flow:upload-id:*` and
`baki:ws:ticket:*` to expire on their TTLs; no schema or storage change. The first 5b deploy stays at one replica
(`maxReplicas` 1), so the mixed-version window is a rolling deploy of one replica's successor.

## Open

- The two SSE-side limits remain: a resync does not end SSE streams (it names no token), and an SSE revocation lost
  by the channel waits for the periodic blacklist check (2 minutes).
- `dashboard.*` drops the owner's and the flow's organisation's views; other collaborators' views of a shared flow
  still wait out the TTL (which users those are is not known without a read), as before.
- `ChatService`, `EventManager` and `memLedger` are registered replica-local until the command channel, the SSE
  fan-out and the Redis ledger of the same phase merge (ADR 0030).
- The scanner's dedup memory and the analysis report pair are registered as known gaps (SURVEY_STATE rows 5 and 8);
  moving them to the database is not part of this phase.
- Alerts on `pad_inval_gap_total` and `pad_inval_dropped_total{reason="redis_down"}` belong with the infrastructure
  wave's other Redis alerts.
