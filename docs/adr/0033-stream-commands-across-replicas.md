# 33. Stream commands across replicas

Date: 2026-10-13. Status: accepted, implemented (Phase 5b, wave 2: the chat half of the cross-replica commands; the SSE
half is ADR 0030).

## Context

A chat stream is a goroutine with a cancel function, a buffer and a set of pending approvals, all in the memory of the
replica that started it (`activeStreams`, `finishedStreams`, `pendingFixDecisions`). The requests that act on a running
stream arrive on any replica behind a load balancer with no stickiness:

- **Stop** (`POST /api/chat/cancel`) found no stream on the wrong replica, did nothing and answered `ok`. The stream
  kept running and spending until its idle limit or the ten-minute cap.
- **An approval decision** (`POST /api/chat/fix-decision`) on the wrong replica answered 404 ("no pending fix
  proposal"). The card was dead, and the stream waited out the 60-second approval timeout.
- **A reload** (`POST /api/chat/get`, `activeStreamId`) on the wrong replica reported no stream, so the client did not
  re-attach and the answer was lost until a second reload.

The ownership check already worked across replicas (`OwnerOf` falls back to the resume snapshot in Redis), but the
snapshot did not say which replica owned the stream, so there was nowhere to send a command. The decisions taken for
the phase: a per-instance command channel, the owner replica re-checks everything, and Redis being down answers 503
`REPLICA_UNREACHABLE`, never a false `ok` (ADR 0029's failure-mode vocabulary).

## Decision

1. **The snapshot names its owner.** `resumeSnapshot` gains `replica` (the instance ID, `PAD_INSTANCE_ID`) and the
   envelope (`v:1, kind:"chat.resume"`), all additive. The store stamps them in `Save`, so no caller changes. A release
   before this reads the snapshot as it did (the fixture `0043/chat_resume_snapshot.json` is read by this release,
   and a test decodes what this release writes with a copy of 0043's struct). A snapshot without `replica` (written by
   that release) cannot be commanded; see Consequences.
2. **A command bus, `redisx.CommandBus`.** Two declared channels per instance: `baki:chat:cmd:<instance>` carries
   commands to the replica, `baki:chat:reply:<instance>` carries the answers back to the asker. Each replica
   subscribes to both from start (`ChatService.StartCommands`, an fx start hook) to the end of `Shutdown`
   (`StopCommands`, so a relayed stop is still answered while the streams wind down), and re-subscribes with a
   backoff after any break. The bus is the generic request and reply part: it reads two fields of a payload,
   `requestId` and `replyTo`, and leaves the rest to its user.
   - `Call(target, requestID, payload, wait)` publishes and waits at most `chatCommandWait` (2 s). It fails at once
     with `ErrNoReceiver` when `PUBLISH` reports no subscriber on the target's channel (the owner is down, restarting
     or not subscribed): that is the same answer as a timeout, only quick. It fails with `ErrNotReady` when this
     replica's own reply subscription is not up (an answer could not arrive), `ErrNoReply` after the wait, or the
     transport error. Pub/sub queues nothing, so a command sent into a re-subscription gap is lost, and the sender
     hears a failure.
   - The handler runs on its own goroutine, bounded at 64 in flight per replica; a burst beyond it is dropped (its
     sender times out). Payloads over 16 KiB are ignored. `redisx.PubSub` counts every message by its declared kind and
     drops an unknown version or kind before it reaches a handler (ADR 0029).
3. **The command, `chat.cmd` v1:** `{v, kind, op, streamId, scope, requestId, replyTo}` plus, for a decision,
   `proposalId, approved, excluded, tokenScopes`. `op` is `cancel` or `decide`. `scope` is the CALLER's stream scope
   (the key scope of the request, `""` without accounts). The reply is `{v, kind:"chat.reply", requestId, status}` with
   status `ok`, `gone` (the stream has ended), `no_pending`, `forbidden`, `scope_denied` or `unsupported` (an op this
   release does not know: a newer replica during a rolling deploy).
   - **The owner decides, again.** For `cancel` it finds the stream in its own `activeStreams` and compares
     `ctl.ownerID` to `scope` (`forbidden` otherwise). For `decide` it checks the same ownership and then calls the
     same `ResolveApproval` the local path calls, with an actor rebuilt from `tokenScopes`, so the stream match, the
     required-scope check and the first-decision-wins channel all run again. The asking replica's checks (it did the
     ownership check on the snapshot, and the scope check against the pending key) are an early refusal, not the
     authority.
4. **Stop.** `ChatService.CancelStreamAs(ctx, scope, id)`: a stream on this replica is cancelled as before (no Redis);
   a finished one is a no-op; otherwise the shared snapshot says whether it is over (nothing to do), whose it is
   (`errStreamNotOwned`, answered as an unknown stream), and which replica runs it (the command). Without Redis
   there is no other replica: the answer is the one it always was.
5. **Approvals.** `requestApproval` writes `baki:chat:approval:<proposalID>` = `{stream, replica, scope}` (TTL: the
   wait plus 10 s) before the card is shown, and deletes it when the approval resolves (the decision claim stays until its TTL: it holds the outcome, addendum).
   `ChatService.DecideApproval(ctx, scope, stream, proposal, approved, excluded)` first tries `ResolveApproval` on
   this replica (no Redis). On `ErrNoPendingFix` with a cluster, it reads the pending key: missing, another stream's,
   or this replica's own means 404 as before; a token without the key's `scope` is refused here
   (`ErrScopeDenied`, nothing sent). It then takes `SET NX baki:chat:decision:<proposalID>` (TTL 60 s): **the first
   decision from any replica wins the claim and is sent; a decision that loses it is not answered `ok` on that
   account: it reports the winner's outcome (see the addendum below)**. A command known not to have been delivered gives the claim back (compare-and-delete), so
   the retry can send; one that may have been delivered marks it lost (addendum). The owner's channel buffer stays the authoritative first-wins (it also covers a decision that
   arrives on the owner itself, which does not take the claim: a local decision touches no Redis).
   - **A failed write of the pending key** is logged and nothing else: decisions that reach the owner replica work,
     decisions from other replicas answer 404 until the next approval.
   - The proposal id is client-supplied; it becomes part of a key name only after the approval key read, and ids over
     128 bytes are refused first.
6. **Reload.** `baki:chat:active:<digest of scope, flow, conversation>` = `{stream, replica}` (TTL as the snapshot),
   written by the owner's mirror the first tick after the turn's question is stored (`userTurnStored`), cleared
   when the answer has settled (or after 15 s). `ReadConversation` consults it when the local maps have no stream, loads
   the snapshot, checks `snapshot.Owner == scope`, and drops a stream whose finished answer the stored history
   already ends with (the local `answerStored`, on the snapshot's text). Redis trouble is "no stream", as before.
7. **Redis down.** `OwnerOfChecked` is `OwnerOf` that tells an unreadable snapshot (Redis error) from an unknown stream,
   and the two command handlers use it: a stream this replica does not run and whose snapshot cannot be read, an
   owner that does not answer, and a failed publish are all **503 `REPLICA_UNREACHABLE`** with `Retry-After: 2` and
   `details.retryAfterSeconds`. The code is added to the catalogue (api-conventions.md), the two routes' annotations
   and the frontend. A command is safe to send twice (stop is idempotent; a decision is guarded by the claim, which a
   failure releases), so "retry" is the whole recovery. A stream on the replica that got the request is untouched by
   an outage (stop, decision and the approval itself use no Redis). `resumeStore` itself is unchanged (fail open);
   `Lookup` is the optional extension that reports errors, which only the commands ask for.
8. **Frontend.** `chatApi.cancelStream` re-sends a stop that was answered `REPLICA_UNREACHABLE` after the server's
   wait, twice; a stop that still fails raises a toast that says the response keeps running there, with a "Try
   again" action that repeats until it gets through. A decision answered `REPLICA_UNREACHABLE` puts the card back to
   pending (it had gone to "applying") and raises a toast with a retry action.
9. **Not done, on purpose.** `begin` is not a command: the legacy handshake (a stream the server named, unblocked by
   `/api/chat/begin`) only matters to clients that predate `clientStreamId`, and the shipped client always sends one.
   A legacy-handshake stream stays tied to the replica its `/begin` reaches, as before. The `clientStreamId`
   duplicate check (`tryStartStream`) and the per-scope stream cap stay per replica, so a user can run 3 streams on
   each replica: the shipped client re-sends `/stream` only after a 503 `SERVER_RESTARTING`, which starts nothing, so
   the survey found no two-replica failure that a Redis claim would fix, and the budget ledger bounds the cost. See
   Open.

## Consequences

- A stop and an approval decision work from any replica; a reload re-attaches from any replica. With one replica and
  with no Redis the code paths are the ones that existed (the new service methods return before touching Redis when
  the stream or the approval is local or `cluster` is nil).
- A stop for a stream whose owner is gone is 503 until the snapshot expires (up to the stream cap plus two minutes),
  because nothing says "this replica is dead" more precisely than "nobody listens on its channel". The stream is dead
  too, so it costs nothing, and the client's own stall probe ends the bubble. The same holds for a stream mirrored by
  a release before this one (no `replica` in the snapshot): during the first rolling deploy to this release a stop
  for such a stream is 403 `STREAM_NOT_FOUND` (addendum below; first written as a 503), and the stream runs on until it
  ends, which is why the first 5b deploy stays at one replica.
- Instance IDs need not be unique any more: the name replicas go by in Redis carries a boot suffix (see the addendum
  below). Before that, two replicas that shared one would both receive each other's commands, and the one that does
  not run the stream would answer `gone`, which the asker takes as "already over".
- Cost: one extra GET per conversation read on a deployment with Redis (the pointer; 400 ms bound), two GETs and a
  publish per relayed stop, three round trips and a claim per relayed decision. Nothing on the streaming path
  except the pointer write once per turn.
- Metrics: `pad_chat_commands_total{op,side,outcome}` (origin and owner side, so a rate of `unreachable` at the
  origin without an owner count is a replica that never got the command) and `pad_chat_command_seconds{op,answered}`.
  `pad_redis_errors_total{mechanism="chat_cmd"|"chat_reply"|...}` already carries the Redis side.

## Rollback

The previous release does not subscribe to `baki:chat:cmd:*`, so nothing it does changes. It reads the new snapshot
unchanged (additive fields), ignores the approval, decision and active keys (they expire on their own: TTLs of at
most twelve minutes), and answers a stop and a decision from its own memory, as before. This release against a
previous one during a rolling deploy: a stop for a stream owned by the previous release is 503 (its snapshot has no
`replica`), a decision for its approval is 404 (it writes no pending key), a reload finds no pointer and reports no
stream; each is the behaviour of the release before, or a retry. Reverting the binary needs no cleanup.

## Open

- **The per-scope stream cap and `clientStreamId` uniqueness across replicas.** A Redis `SET NX` claim of the id and
  an `INCR` counter per scope would close both; neither failure is reachable with the shipped client (above), and
  the cap multiplies a limit rather than breaking a guarantee. Revisit if a client retries `/stream`.
- **A liveness signal for the owner.** The snapshot has no "last written" time, so a client that resumes a stream
  whose replica died sees a stale non-final snapshot until it expires. A heartbeat field would let the resume
  endpoint answer "interrupted" at once.
- **`begin` as a command** if the legacy handshake must survive a multi-replica deployment.
- **Approvals across a restart.** A pending approval still dies with its process (nothing restarts a tool loop).

## Addendum: authenticated commands, a decision that is not guessed, replica identity (Phase 5b review round)

**Commands are signed (REVIEW_REDIS M2).** Before, any process that could publish to `baki:chat:cmd:<replica>` could
stop a stream or, knowing the ids Redis itself exposes, approve an AI-proposed `apply_fix` as the victim. When the
replicas share a secret, every command and every reply travels in a signed frame (`internal/redisx/command_auth.go`):
`{"v":1,"kind":<channel name>,"dir":"cmd"|"reply","to":<receiving replica>,"ts":<unix ms>,"n":<nonce>,"body":<the
payload as text>,"mac":<hex HMAC-SHA256>}`. The key is HKDF-SHA256 of the deployment's shared secret
(`PAD_ENCRYPTION_KEY`, else `PAD_AUTH_SECRET`; a per-process minted secret never qualifies,
`AuthConfig.SharedSecret`), with a purpose label, so the secret signs nothing itself. The MAC covers the direction, the
receiver, the timestamp, the nonce and the body; a frame is accepted only if it verifies, is addressed to this replica
on the right channel, its timestamp is within 30 seconds of the receiver's clock and its nonce has not been seen in
that window (a bounded cache that refuses rather than forgets when full). Rejected frames are dropped before any
handler and counted in `pad_redis_bus_rejected_total{reason}` (unsigned, malformed, oversize, bad_mac, stale, replay,
target, flood). Commands are new in this phase, so there is no older sender to stay compatible with. Consequences to
plan for: replicas must hold the same `PAD_ENCRYPTION_KEY`/`PAD_AUTH_SECRET` (a rolling key change leaves commands
between the two keys unanswered, which is 503 `REPLICA_UNREACHABLE` and a retry, for the length of the overlap) and
their clocks within the 30-second window. With no shared secret the bus runs unsigned and the boot says so; Redis on a
private network with a password and TLS is then the only defence (ADR 0030 addendum).

**The loser of the decision claim reports the winner's outcome (REVIEW_REDIS M1, L10).** The first version answered
`ok` to a decision that lost the `SET NX` claim, on the theory that the winner's was in flight. If the winner then
failed (the owner restarting, its replica dying between claim and call) the user saw a success for a decision nobody
received. The claim now has a state: `sending` (a replica won and is delivering), `done` (the owner answered; the
status is recorded), `lost` (sent, no answer came; the decision is recorded). A replica that loses the claim waits,
bounded by the winner's own call timeout plus two seconds, for `done` and returns the owner's answer as its own; if the
claim is released (the winner could not deliver) it claims again; if a `sending` claim is older than the call timeout
plus two seconds its winner is presumed dead and the claim is taken over with a compare-and-set; if nothing settles in
time it answers 503 `REPLICA_UNREACHABLE`. A delivery known not to have happened (nobody listens, the bus is down)
releases the claim; one that may have happened (no reply, a publish timeout) marks it `lost` with the decision, and
the retry of the **same** decision then answers `ok` once the approval is gone (it was applied), while the opposite
decision answers `no_pending` (too late), instead of a 404 for a decision that was applied. The claim is no longer
deleted when the approval resolves; it expires on its own TTL, because it holds the outcome.

**Identity.** The `replica` in snapshots and approval records, and the command and reply channels, use the boot origin
(`instance#suffix`, ADR 0030 addendum), so replicas with the same `PAD_INSTANCE_ID` no longer answer each other's
commands (the "instance ids must be unique" consequence above no longer holds). A stop for a stream whose process has
been replaced is "nobody listens", 503, until the snapshot expires, exactly as for a replica that is gone for good.

**A stream of the release before the commands** (a snapshot without `replica`) cannot be commanded. The answer is now
the one that release gave for a stream its replica did not run, 403 `STREAM_NOT_FOUND`, which the client takes as
"gone" with no retry loop and no toast; the 503 it replaces promised a retry that could only fail for as long as the
stream lived (REVIEW_COMPAT M6). A decision for such an approval stays 404 (it wrote no pending key).

**Smaller.** A peer-chosen reply status is mapped to the known set before it labels a metric (`other` otherwise,
`pad_chat_commands_total`). `clearActive` is a `WATCH`/`MULTI` compare-and-delete, so a newer turn's pointer is never the
one removed. Log lines of this path are rate limited (`redisx.LimitedWarn`). The stream start's budget-ledger write
runs outside the stream-cap lock (ai-providers.md, ADR 0028 addendum), so a slow Redis never queues stream starts
or holds up `Shutdown`.

**Operator statement for evictions** is in ADR 0029's addendum: an evicted claim is harmless, an evicted pending
approval makes decisions from other replicas answer 404 until it times out, and an evicted snapshot makes a relayed
stop answer 403 `STREAM_NOT_FOUND` (never an ok that stopped nothing).

**Rollback.** Revert the build: the commands vanish with it (the channels are pub/sub), the previous release never
subscribed to them, and the claim and approval keys expire within a minute. No migration.
