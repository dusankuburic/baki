# 30. Cross-replica commands and SSE replay

Date: 2026-10-13. Status: accepted, implemented for the SSE half (Phase 5b, wave 2: fan-out, event ids with
`Last-Event-ID` replay, subscriber presence). The stream command channel (cancel, approvals, `begin`) is decided in
the second half of this ADR, which the command-channel work adds; this half does not depend on it.

## Context

`api.EventManager` was an in-process hub. `EmitTo`, `EmitToOrg`, `EmitToAll` and `HasSubscriber` saw only the clients
connected to this replica, so with a second replica:

- an event emitted on A (a chat chunk, an analysis result, `settings:changed`, a scanner's `governance:alert`)
  never reached a client connected to B;
- `watchStream`, the chat stream's watchdog, asks `HasSubscriber(owner)` every 15 seconds and cancels the stream after
  two misses, so a stream on A whose owner's tab sat on B was cancelled after about 30 seconds ("you were
  disconnected");
- an SSE stream carried no event ids and the server ignored `Last-Event-ID`, although the swagger text said a
  reconnect resumes: what was emitted while the client was disconnected was lost, and the client refetched.

## Decision

All of it sits behind `api.EventFanout`, an interface with the `*redisx.SSEBackplane` as its one implementation. A
deployment without the shared Redis (capability `shared-state`) builds the manager without one and is byte for byte
what it was: no ids, no replay, local clients only.

### 1. Fan-out

- One channel, `baki:sse:ev` (declared `sse.emit`, envelope `{"v":1,"kind":"sse.emit",...}`), and an origin filter:
  every message carries the publishing replica's instance id and a replica drops its own.
- `EmitTo`, `EmitToOrg` and `EmitToAll` deliver to this replica's clients first, then queue the message. The queue is
  bounded by count (1024) and bytes (16 MiB), has one sender (an origin's events keep their order), and never blocks
  the emitter. A full queue drops the message (`pad_sse_fanout_total{publish,dropped}`).
- The message carries the encoded event body (the JSON of the `data:` line, after the `lightweightEvent` swap, so the
  256 KiB frame bound holds everywhere) and the audience: `all`, or the list of users with the event id of each
  copy. `EmitToOrg` resolves the roster at the origin (one read) and publishes the user list.
- A receiver validates (a version or kind it does not know, a body that is empty, over 256 KiB or holds a line break,
  an id that is not a stream id, no audience: counted `ignored` and dropped), then delivers to its own clients
  through the same `offer` path and **never publishes**. The id is written into the frame only after
  `ValidStreamID`, so a hostile message cannot inject a line.
- **Chat chunks and heartbeats, and progress ticks (`analysis:progress`, `flow:parse-progress`) are publish-only
  and ephemeral**: they get no id, are not logged, and are published only for the users whom subscriber presence says
  another replica holds (`PublishHeld`). A replica streaming an answer to its own client puts nothing on the channel.
  The check is `PeerHint`, a cache read that never waits on Redis for a user it has seen (it refreshes in the
  background once the answer is a second old); only a user it has never seen waits for one bounded read (200 ms).
  Unknown counts as not held: a chunk missed in the second between a tab connecting to another replica and the
  first presence write is recovered by the chat client's resume by offset.
- Everything else is **durable** and published unconditionally, to every audience member whether or not anyone
  listens: the id and the log (below) exist for the one who is not listening yet.

### 2. Event ids and replay

- A durable event is appended, before it is delivered anywhere, to `baki:sse:log:<user>` (a Redis Stream, `MAXLEN ~
  200`, the key expiring 5 minutes after the last write; declared `sse.log`). The entry id returned by `XADD`
  (`<ms>-<seq>`, monotonic and independent of the replica) is the frame's `id:` line on every replica that
  delivers it. The write is one pipelined round trip bounded to 250 ms and skipped while `redisx.Health` says Redis
  is degraded, so a dead Redis costs the emitter nothing.
- An org event is copied into each member's log (at most 256 members per event; the rest of a larger audience
  receives it without an id). `EmitToAll` has no roster to copy into: it is delivered live and carries no id;
  nothing in the code uses it today, and nothing tenant-scoped may.
- A frame over 32 KiB is not logged (the log would hold up to 200 of them per user for 5 minutes): it is delivered
  whole and without an id, as it is today.
- `HandleEvents` reads `Last-Event-ID`. A value that is not a stream id is ignored and counted. Otherwise, after the
  stream is registered (so nothing emitted from then on is missed) and before the live loop, it reads `(id, +]` from
  the user's log: at most 200 entries, none older than 5 minutes whatever id was sent, oldest first, written with their
  ids. A live frame that was queued meanwhile and is also in the replay is dropped by comparing its id with the last
  replayed one, so nothing is delivered twice. A Redis failure continues without a replay (`failed`), never refuses
  the stream.
- The log key is the authenticated user's, never derived from the header, so a client cannot read another user's
  frames by sending that user's ids.
- A gap is detected and signalled, not skipped: a reconnect the log cannot replay whole gets a `replay:gap` event instead
  of a partial replay (see the addendum below).
- **Client** (`frontend/src/api/client.ts`): remembers the last `id:` line, applied when the event's `data:` line
  arrives, as the SSE specification says, and sends `Last-Event-ID` on a reconnect. It sends the header only after a
  server has sent an id, so an older server (no ids) never receives one; the id is forgotten on teardown and
  sign-out. A cross-origin client (the Tauri webview, a dev server) makes `Last-Event-ID` a non-safelisted request
  header, so the server's CORS allow list gains it (`router.go`); without that the preflight of the first
  reconnect would fail.

### 3. Subscriber presence

- `baki:sse:subs:<user>` (declared `sse.subs`) is a hash: field = instance id, value `{"v":1,"kind":"sse.subs",
  "n":<local streams>,"exp":<ms>}`. A replica rewrites a user's record soon after their local stream count changes
  (the count is read when written, so writes cannot be reordered into a stale state) and every user's every 15
  seconds. Each record is valid for 45 seconds and the hash expires 60 seconds after its last write; a clean stop
  removes the replica's fields.
- `HasSubscriber(user)`: a local stream answers true with no Redis call. Without a fan-out, false. Otherwise one
  `HGETALL` bounded to 200 ms, cached for 5 seconds per user (the watchdog calls every 15 seconds per stream), counting
  a record of another instance with `n > 0` and an unexpired `exp`. A reader skips an expired, unreadable or
  newer-version record and **never deletes** it.
- **A Redis error, or Redis marked degraded, returns true** (decision 3 of the Phase 5b brief): a stream is never
  cancelled because presence is unreadable. A stream whose owner really left is still ended by the idle limit, the
  10-minute cap and the budget.
- `chat_resume.go` is not touched: the watchdog already calls `HasSubscriber`.

### Failure modes

| Mechanism | Redis down | Recovery |
|---|---|---|
| Fan-out (`sse.emit`) | open: local delivery continues; publishes fail fast once Health is degraded (`skipped`); clients on other replicas miss events | the subscription retries with a 0.5-10 s backoff and reports `Subscribed()`; events published meanwhile are not replayed from the channel, a reconnecting client replays from its log |
| Event log (`sse.log`) | open: frames are delivered without an id; no replay after a reconnect | the next successful append; the log survives a short outage in Redis memory |
| Presence (`sse.subs`) | open: unreadable means "has a subscriber"; writes are retried at the next tick | records expire on their own |

### Mixed versions

A replica of the release before neither publishes to nor subscribes to `baki:sse:ev`, writes no log and no presence.
Beside a new replica it is an island: its clients get its own events, theirs get the new replica's, and its
`HasSubscriber` is local. Its streams carry no ids and its server ignores `Last-Event-ID`; a new client works against
it (it never sends the header until a server sent an id). **That is why the first Phase 5b deploy stays at one
replica** and `maxReplicas` is raised only when the whole fleet runs this release.

## Consequences

- With the shared Redis, an event reaches a client on any replica, a stream is no longer cancelled for a tab on
  another replica, and a reconnect replays up to 200 durable frames from the last 5 minutes.
- The emit path of a durable event gains one pipelined Redis round trip (about 1 ms, 250 ms at worst, none while
  Redis is degraded). Chat chunks gain a cache read.
- Redis holds up to about 200 frames of at most 32 KiB per user for 5 minutes, bounded and volatile (every key has a
  TTL).
- Per-replica caps stay per replica (10 SSE streams per user per replica, so 10 x N).
- `pad_sse_fanout_total{direction,outcome}`, `pad_sse_log_total{outcome}`, `pad_sse_replay_total{outcome}`,
  `pad_sse_replay_frames_total` and `pad_sse_presence_reads_total{outcome}` say which part is degraded.

## Rollback

Everything is additive: the previous release ignores the three key families (they carry TTLs) and the channel. A
client of the previous release ignores `id:` lines (its parser reads `data:` only) and sends no header. Reverting
the build leaves only expiring keys behind. No migration.

## Open

- `EmitToAll` frames carry no id and are not replayed. If a deployment-wide event ever needs replay it needs a
  roster (or a shared "all" log with a per-user cursor).
- The channel is one for all users; every replica receives every message and drops those it has no clients for.
  Sharding to `baki:sse:ev:<0-63>` with refcounted subscriptions is the upgrade if chunk volume warrants it.

## Addendum: replay is complete or says so; replica identity; what crosses Redis (Phase 5b review round)

**Replay completeness (REVIEW_REDIS H2).** The first version returned the oldest 200 entries after the client's
`Last-Event-ID` and then went on with live frames, so a client that had missed more lost the middle without a sign.
A replay is now complete or it says it is not. `Replay` reads one more than the cap and the log's first entry in the
same round trip, and returns a `*ReplayGap` instead of a partial result when:

- more than 200 frames follow the cursor;
- the cursor is older than the log's first entry (its entry was trimmed or lost with a Redis restart, so what came
  between is unknowable), or the log is empty although the cursor is inside the 5-minute window;
- the cursor is older than the 5-minute window.

On a gap, and also when the replay cannot be read (Redis down), `HandleEvents` writes one `replay:gap` event
(`{"name":"replay:gap","data":{}}`) instead of frames. When the log exists the frame carries the id of the log's newest
entry, so the client's cursor leaves the gap behind and the next reconnect does not replay it again; a replay whose
write budget (10 s) runs out ends the same way, with the id of the last entry it fetched. The frame is an ordinary
`{name, data}` event: an older client has no listener for the name and ignores it, and an older server never sends it.
The client (`frontend/src/api/client.ts`) does not hand the event to its listeners; it calls the gap subscribers
(`subscribeEventGap`), which refetch what a reconnect refetches: running chat streams resume by offset
(`useStreamingMessage`), and the governance badge and settings reload (`useAppEvents`). A client idle for more than
five minutes, and any reconnect during a Redis outage, therefore refetches once; that is the price of never being
silently wrong. `pad_sse_replay_total{outcome}` gains `gap` and `cut`.

**Replica identity (REVIEW_REDIS M3).** The name a replica goes by in Redis is its `PAD_INSTANCE_ID` plus a boot
suffix, `pod-7#3fa91c0e` (`redisx.BootOrigin`; the invalidation fan-out already did this). It is the SSE origin of
published events, the field of the replica's subscriber record, the owner `replica` in resume snapshots and approval
records, and the address of the command channels (ADR 0033). Two replicas given the same `PAD_INSTANCE_ID` (a copied
setting, a fixed `hostname:`, host networking) are therefore still two replicas: before, each took the other's events
for its own, overwrote its subscriber record and answered its commands. The human-facing id in logs, traces and the
admin page is unchanged. A restarted replica is a new origin, so its old subscriber record is simply left to expire
(at most 45 seconds of "someone may be listening", the safe direction).

**What crosses Redis, and who may read it (REVIEW_REDIS M6).** Event bodies cross Redis in plaintext: `baki:sse:ev`
carries every event of every tenant to every replica, and `baki:sse:log:<user>` holds up to 200 durable frames (each at
most 32 KiB) for each active user for 5 minutes; chat answers, flow names and finding comments are in them. Key names
contain user ids (`sse.log`, `sse.subs`, `ai.inflight-user`, `flow.upload-id`). Anyone who can read the server
(`MONITOR`, `SLOWLOG`, a snapshot, a managed service's support access) sees them. A multi-replica deployment must
therefore keep Redis on a private network, require AUTH (a password or an ACL user), use TLS (`rediss://`), and not
enable RDB/AOF persistence on this instance (it is a volatile backplane). The app warns once at boot, with
`PAD_MAX_REPLICAS > 1`, when `PAD_REDIS_URL` is `redis://` (not TLS) or carries no password. The size of the logs is
bounded by count and by bytes per frame; lowering `SSELogMaxLen` or `SSEMaxLoggedBytes` trades replay depth for Redis
memory and exposure, and is a code constant today (see Open).

**Rollback / mixed versions.** The `replay:gap` event and the boot suffix are additive: the previous release neither
sends nor understands either, and ignores unknown names. A replica of this release and one of the previous release
share no SSE channel, as before. The first 5b deploy stays at one replica.

**Open (added).** The log's per-frame bound (32 KiB) and length (200) are the 5b defaults; a deployment with chatty
org events and many users may want them smaller, which needs a setting. Expiry of subscriber records and leases is
judged with each replica's own clock (REVIEW_REDIS L9); a skew approaching the lease makes live records look expired.
