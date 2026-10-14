# 29. Redis payload envelope, key registry and health policy

Date: 2026-10-13. Status: accepted, implemented (Phase 5b, wave 0: the foundations every later Redis mechanism builds on).

## Context

Redis is optional: unset, every consumer keeps its in-process implementation; set, five things use it. The rate
limiter keeps its token buckets there, the WebSocket hub fans rooms out and shares presence through it, the chat
service mirrors stream snapshots so a client can resume on another replica, and the token blacklist fans
revocations out over it. Phase 5b adds more (SSE fan-out and a replay log, a command channel, the AI budget ledger,
cache invalidation), and raises `maxReplicas` above 1 only when they are in.

Before this ADR:

- **No payload carried a version.** `{o, e}` (room), the presence record, the resume snapshot and the revocation
  message were bare objects. A rolling deploy puts two releases on the same keys and channels for minutes, so a new
  field or a renamed one reached an old reader unannounced.
- **One reader deleted what it could not read.** `decodePresence` removed a presence record that failed to
  unmarshal: a record written by the other release, in a shape this one did not know, was erased from under its live
  owner.
- **The keys were scattered literals** in five packages under four naming schemes (`ws:`, `chat:`, `ratelimit:`,
  `baki:auth:`), with no list of what exists, which of them expire, or what each does when Redis is away. The
  server cannot run an eviction policy that only evicts keys with a TTL (`volatile-lru`) without knowing that.
- **Redis was a hard dependency twice.** `redisx.Provide` refused to boot when the server did not answer, and
  `/readyz` answered 503 after three failed probes because a ping failed. Every consumer is written to fail open,
  so a Redis outage on a fleet took every replica out of rotation (and a restart during it could not boot) while
  each of them could have served.
- **`peruser` was a bare literal** beside the rate-limit group constants, and no test froze the group names, which
  are the metric labels and the middle segment of the Redis keys.
- **Nothing refused `PAD_MAX_REPLICAS` above 1 without Redis**, which silently multiplies every rate limit and
  splits every collaboration room.

## Decision

1. **A payload envelope**, `redisx.Envelope{V int, Kind string}`: `{"v":1,"kind":"ws.room",...}`, the two fields
   beside the payload's own in the same object.
   - *Additive only.* A field is never renamed or retyped; a new one is added. Missing `v` is version 0, the shape
     of every release before the envelope, and is always accepted.
   - *Tolerant decode.* `redisx.Decode(kind, maxVersion, payload, dst)` returns `ok`, `unknown` (a version above the
     reader's, or another kind) or `bad` (not decodable), counts it in `pad_redis_msgs_total{kind,result}` and
     leaves `dst` untouched unless `ok`. An unknown payload is ignored: never a panic, never an error to the sender.
     The `kind` label is the declared kind the reader expects, never the sender's text.
   - *Never delete on a decode failure.* A record is pruned by its own expiry only. Every reader of a stored record
     skips what it cannot decode and leaves it in place.
   - *Applied now* to the room message (`v:1, kind:ws.room`, beside `o` and `e`), the presence record (`v:1,
     kind:ws.presence`) and the resume snapshot read (accepted at its declared version 0; a snapshot of an unknown
     version is a miss and stays in place). The revocation message stays version 0 on purpose: it is a security
     message, an old replica must act on it, and the 30 s reload from Postgres is the backstop for one a peer
     cannot read. `redisx.PubSub` counts every message by its channel's declared kind and drops one of an unknown
     version or kind before the subscriber sees it.
2. **A key registry**, `redisx.Declare(Spec{Name, Kind, Pattern, TTL, Version, OnDown, Owner})`, called at package
   level by the package that owns the key. `Declare` panics at init on a malformed name or pattern, a duplicate,
   a missing owner, a missing on-down behaviour, a key without a TTL (every kind but pub/sub needs one: the server
   will run `volatile-lru`), and a channel with one. The owner builds names with the declaration (`Key(parts...)`,
   `Glob()` for a pattern subscription, `Prefix()`), so the literal exists once, in the declaration.
   - `internal/di/testdata/redis_keys.golden` lists every family (`go test ./internal/di/ -run TestRedisKeys_Golden
     -update-redis-keys`), and `TestRedisKeys_CoversEveryDeclaration` fails when a declaring package is not linked
     into that test binary.
   - Two archtest ratchets: the declaring files are pinned (a new Redis user is a conscious, reviewed edit), and no
     other file spells the start of a declared pattern as a string literal. The prefixes are read from the
     declarations, so a family declared later is covered. The one literal that cannot move is
     `auth.RevocationChannel`: `internal/auth` stays free of the Redis dependency and defines the publisher and
     subscriber interfaces `redisx` implements, so the channel is declared in `redisx/keys.go` and
     `TestRedisKeys_AuthConstantMatches` holds the two spellings together.
   - *Shipped names are never renamed.* The existing keys keep their names (`ws:room:`, `ws:presence:`,
     `chat:resume:`, `ratelimit:`, `baki:auth:revoked`); new families go under `baki:`.
3. **Previous-release fixtures.** `internal/redisx/testdata/redis/prev/<release>/` holds payloads captured from the
   tree of a release, before any payload change: `0043` has a room message, a presence record, a resume snapshot, a
   revocation message and a token-bucket hash. Each package that decodes one has a test that feeds the fixture to
   the current code and asserts the result, and the same packages assert that what they now write is still read by
   the previous release's shape. `TestPrevFixtures_EveryFileIsDecodedByATest` fails on a fixture no test names. The
   release process appends a directory per release.
4. **Health: degraded, not unready.** `redisx.Health` watches the shared client: a ping every 5 seconds and the
   outcome of every operation (a go-redis hook, so no call site reports anything).
   - One failed ping degrades at once; three operations in a row failing, with no success between, degrade;
     any success recovers (a miss is an answer). A caller that gave up (`context.Canceled`) is not counted.
   - `Degraded()` reads state and never pings. `pad_redis_up` (gauge, absent without Redis, so an alert on `== 0`
     cannot fire for a deployment that does not use it) and `pad_redis_errors_total{mechanism}` (the declared family
     the failing operation touched, `ping`, or `other`). `OnChange(func(up bool))` lets a later mechanism
     resynchronise when Redis returns.
   - **`/readyz` stays 200** and adds `"degraded":["redis"]` to its body while Redis is down; `status` and the other
     keys are unchanged, so an old probe sees nothing new. The database and blob checks still gate it. The admin
     health page keeps its live ping and `overall: "degraded"`. The signal to alert on is `pad_redis_up == 0` for a
     few minutes, not readiness.
   - **Boot tolerates an unreachable Redis**: a loud WARN, the app starts degraded, the loop notices the recovery. A
     URL that does not parse still refuses to boot (a typo is not an outage).
5. **`PAD_MAX_REPLICAS` above 1 requires `PAD_REDIS_URL`**, refused by config validation in every mode and from
   either config source. A single replica needs no Redis; several share rate limits, rooms, presence and resume
   only through it.
6. **Rate-limit groups are frozen.** `route.RatePerUser = "peruser"` sits with the other group constants,
   `middleware_chain.go` builds its limiters and dispatch map from the constants, and the key shapes are built by
   one declaration (`ratelimit:{group}:{addr}`, with the per-user group's two shapes as its `addr`).
   `internal/api/testdata/ratelimit_groups.golden` records each group's metric label (observed by refusing a
   request) and key shape; a test fails on a `Rate*` constant missing from the list and on a group literal in the
   chain. Renaming a group in a two-release fleet splits its buckets (the limit doubles) and orphans its series.
7. **A harness for two-replica tests**, `internal/testutil/redisreplica`: N replicas on one miniredis, each with its
   own instance ID, client (its own pool) and `redisx.Health`, `Kill` and `Restore` for an outage, `PrevFixture`,
   and `Eventually`. Later waves write their mechanism's two-replica and Redis-down scenarios on it.

## Failure-mode table (Redis down) for the mechanisms that exist

| Mechanism (declared name) | On down | What degrades | Recovery |
|---|---|---|---|
| Rate limit (`ratelimit.bucket`) | degrade: a per-replica in-memory bucket with the same rate and burst, 75 ms call timeout, 5 s breaker | the limit is N times the rate for the outage; it is never lifted | the breaker closes after 5 s |
| WS room fan-out (`ws.room`) | open: publish logs a warning, local delivery is unaffected | collaborators on other replicas see no edits or selection changes; messages sent in the gap are lost | the subscription re-establishes itself; clients heal through `flow.changed` and the version check on save |
| WS presence (`ws.presence`) | open: writes are logged, a failed read returns the local roster | the roster shows this replica's members only | the next heartbeat (54 s) rewrites; records expire on their own |
| Chat resume snapshot (`chat.resume`) | open: `Save` reports false and is retried at the next tick, `Load` is a miss | resuming a stream on another replica answers `STREAM_NOT_FOUND`; the same replica still resumes from memory | the next successful save |
| Revocation fan-out (`auth.revoked`) | closed: a miss that cannot be answered from the local set goes to Postgres and an error there means revoked | peers learn of a revocation at their 30 s reload instead of at once | resubscribe plus a catch-up load |
| `/readyz` | stays 200 | the body lists `"degraded":["redis"]` | none needed |
| Boot | starts degraded (WARN) | as above | the ping loop |

The mechanisms Phase 5b adds (SSE fan-out and log, subscriber presence, the command channel, the budget ledger,
cache invalidation) add their rows here with the same vocabulary: every one fails open except the budget ledger,
which fails closed while a daily budget is set.

## Consequences

- A rolling deploy is safe for these payloads in both directions: an old replica ignores the two extra fields, a new
  one accepts both shapes and skips, without deleting, what it does not know. The fixtures prove the first
  direction against the real previous payloads.
- A Redis outage no longer removes replicas from rotation and no longer blocks a restart. The cost: a deployment
  that relied on readiness to notice a missing Redis now needs the `pad_redis_up` alert (the operations runbook names
  it; provisioning the alert is the multi-replica infrastructure wave's), and `deploy.yml`'s smoke test no longer fails a deploy whose Redis is unreachable.
- Every Redis key has a declared TTL, so the managed Redis can move from `allkeys-lru` to `volatile-lru` (the
  infrastructure change belongs to the multi-replica wave).
- A new Redis user must edit two pinned lists (the declaring-files ratchet and the golden), which is the review
  point for a new key family.
- `pad_redis_msgs_total{result="unknown"}` rising during a rolling deploy is the other release's newer payloads, and
  expected; `bad` rising is a producer bug.
- go-redis v9.21 waits for 36 bytes of a (P)SUBSCRIBE confirmation before it parses it, so a channel name under 8
  bytes never reports ready. `Declare` refuses such a channel.

## Rollback

Everything here is additive. The previous release reads the new room and presence payloads (the extra fields are
ignored; the fixtures' shapes are the previous release's own) and keeps writing its own, which this release reads as
version 0. Reverting this release leaves nothing behind in Redis that the previous one cannot read, no key was
renamed and no migration ran. An operator can restore the old boot behaviour only by reverting the build; there is
no flag.

## Open

- Alerts on `pad_redis_up` and the Bicep/compose pieces (Redis service, the `volatile-lru` policy, a scale rule) are
  the multi-replica infrastructure wave's; this ADR only provides the signal.
- The pub/sub subscriptions (`ws.room`, `auth.revoked`) are not seen by the operation hook; a subscription that
  cannot reconnect is noticed by the ping loop, not by a counter of its own.
- The resume snapshot and the revocation message stay version 0. The command-channel wave bumps the snapshot when it
  adds the owning replica; the revocation message moves only if it ever needs a field an old replica must not
  ignore, which the additive rule is meant to avoid.

## Addendum: memory pressure and what an evicted key does (Phase 5b review round)

Every key in the registry has a TTL, which is what lets the server run `volatile-lru`: under memory pressure it evicts
the key with a TTL that was used least recently, and since all of them have one, **any** key can be the victim, with
no error to the app. The review asked what that does to the keys whose loss matters, and for a signal. The statement:

| Key (declared name) | If the server evicts it | What the app does about it |
|---|---|---|
| In-flight budget ledger (`ai.inflight-user`, `ai.inflight-org`) | the budget under-counts the streams of the replicas whose fields were in the hash, until their owners rewrite them (every second) | a held stream whose record has to be created again is recognised on its next write; the next budget check on that replica **fails closed once** (`AI_BUDGET_UNAVAILABLE`, `pad_ai_ledger_errors_total{op="lost"}`) and the record is whole again for the check after |
| Decision claim (`chat.decision`) | a second decision for the same proposal may be sent too | harmless: the owner's `ResolveApproval` is first-decision-wins, so the second answers `no_pending`; a loser waiting on the claim sees it vanish and reads the approval key instead |
| Pending approval (`chat.approval`) | a decision from another replica answers 404 (`ErrNoPendingFix`) until the approval times out | decisions that reach the owner's replica work; no false ok |
| Resume snapshot (`chat.resume`) | resuming on another replica answers `STREAM_NOT_FOUND`; a stop from another replica answers 403 `STREAM_NOT_FOUND` (the owner lookup finds nothing) | never an ok that stopped nothing |
| SSE log (`sse.log`) | a reconnect cannot be replayed whole | the client is told to refetch (`replay:gap`, ADR 0030 addendum) |
| WS ticket (`ws.ticket`), upload id memo, rate-limit buckets, presence | the extra guard or the memo is gone | each falls back to what it protects (the Postgres blacklist, a fresh lookup, a fresh bucket) |

Nothing in this table is made silent: `redisx.Health` polls the server's `INFO` with its 5-second ping (every 30
seconds, best effort; a server that refuses `INFO` is asked once) and exports `evicted_keys` as
**`pad_redis_evicted_keys_total`** (deltas since the process first looked), with one rate-limited WARN naming it. The
first read also checks `maxmemory_policy` and warns once if it is `allkeys-*`. **Operator statement:** run
`volatile-lru` (or `noeviction` on a small dedicated instance), size `maxmemory` with headroom (the SSE logs are the
large tenant: up to 200 frames of up to 32 KiB per active user for 5 minutes), and alert on any increase of
`pad_redis_evicted_keys_total` as well as on `pad_redis_up == 0`.

Two more changes of the round. `HSET`+`EXPIRE` and `XADD`+`EXPIRE` pairs (the budget ledger, the SSE log and subscriber
records, the WebSocket presence hash) go out in one `MULTI`/`EXEC`, so a connection cut between the two cannot leave a
key without a TTL, which `volatile-lru` could never evict. Log lines that carry a Redis failure
(`redisx.LimitedWarn`) are rate limited per call site (one line per 30 seconds, with the number suppressed) and carry
the error's class, never its text, which includes the server's address.
