# AI roadmap: Replicas and Redis

Items of category `replicas` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-replicas-2

**Resume, begin and the active pointer serve a dead replica's live snapshot for up to 12 minutes** · tier 1

**Problem.** `ResumeStream`, `OwnerOf` and `activeStreamOf` answer from the Redis snapshot without checking the owner's liveness key. `ownerGone` exists, but its only caller is `callOwner`, and `dropDeadSnapshot` runs only from the owner-gone branch of `CancelStreamAs`. After a replica crashes mid-turn, a reconnecting client keeps getting a non-final snapshot until the key's 12-minute TTL (`maxChatStreamDuration + resumeRetention`) runs out. That covers both a resume by offset after an SSE reconnect and a reload that re-attaches through `baki:chat:active`. The exception is when someone presses stop: the stop goes through `CancelStreamAs` → `callOwner` → `ownerGone` → `dropDeadSnapshot` (`chat_commands.go:429-434`), which removes the snapshot. Without a stop, the client's stall probe reports a generic "stalled", with no code, after three unchanged probes. Every reload re-attaches to the same hung stream. The consequences section of ADR 0033 records this as still open.

**Evidence.**
- `internal/service/chat_resume.go:558` — `ResumeStream` falls back to `resumeBackplane().Load` with no liveness check and returns the snapshot as-is (`:559`)
- `internal/service/chat_resume.go:448` — `OwnerOf` also answers from `Load`
- `internal/service/chat_commands.go:894` — `activeStreamOf` hands the snapshot behind the pointer to `ReadConversation` (`:895-898`) with no liveness check, so a reload re-attaches
- `internal/service/chat_replicas.go:195` — `ownerGone` (live flag + `steadyView` + `ReplicaAlive`) exists; its only caller is `callOwner` (`chat_commands.go:370`)
- `internal/service/chat_commands.go:433` — `dropDeadSnapshot` (defined at `chat_replicas.go:224`) is called only from the `errOwnerGone` branch of `CancelStreamAs`
- `frontend/src/hooks/useStreamingMessage.ts:217` — after `STALL_PROBE_LIMIT` (3, line 29) unchanged probes the client reports `chat:errors.stalled` with no code (`:218`)
- `docs/adr/0033-stream-commands-across-replicas.md:22` — "Still open: a resume (not a stop) of a dead replica's stream serves the stale snapshot until it expires or a stop removes it"; the key TTL is `maxChatStreamDuration + resumeRetention` (`chat_resume.go:31`)

**Change.** Add a liveness check to the read path (`ResumeStream`, `OwnerOf`, `activeStreamOf`). When the snapshot is non-final, names another replica and says `live:true`, ask `ownerGone`. If the owner is gone, compare-delete the snapshot (`dropDeadSnapshot`) and answer with a synthesized final `ResumeResult` whose `ErrorCode` is `STREAM_INTERRUPTED`, so the client shows the partial text as interrupted at once. The other option is `STREAM_NOT_FOUND` on the first resume, as the stop path does. Pick one and pin it in ADR 0033. Keep the `steadyView` rule so a Redis restart never fabricates a death. This covers clients that never press stop; today only a stop removes the dead snapshot.

**Touches.** `internal/service/chat_resume.go`, `internal/service/chat_commands.go`, `internal/service/chat_replicas.go`, `internal/service/chat_replicas_test.go`, `docs/adr/0033-stream-commands-across-replicas.md`

**Done when.**
- miniredis, two services: A publishes a live snapshot and its liveness key, the key expires, and B's view is steady. B's `ResumeStream` returns a final result with `ErrorCode` `STREAM_INTERRUPTED` (or `STREAM_NOT_FOUND`), and the snapshot key is deleted.
- The same scenario within `steadyView`'s 15 s: B still returns the live snapshot (no false death).
- After the first such resume, `ReadConversation` on B no longer reports `activeStreamId` for that conversation.

**ADR.** Specified as open in ADR-0033 (close-out consequences), not implemented.

### R-replicas-3

**A transient Redis read error on /resume or /begin is reported as STREAM_NOT_FOUND instead of a retryable 503** · tier 1

**Problem.** `redisResumeStore.Load` discards the error from `Lookup` ("any transport error → treat as not found"). `OwnerOf` then returns `""`, and `ownsStream` answers `/resume` and `/begin` with the 403 `STREAM_NOT_FOUND` refusal. `handleResumeStream` also renders every `ResumeStream` error as 404 `STREAM_NOT_FOUND`. The command routes already tell an unreadable snapshot from a missing one through `OwnerOfChecked`/`lookupSnapshot`, and answer 503 `REPLICA_UNREACHABLE` with `Retry-After`. After an SSE reconnect the client resumes every open stream by offset. `reportLost` maps a 403/404/410 on a live subscription to a `STREAM_INTERRUPTED` bubble. One Redis blip therefore turns healthy cross-replica streams into failed turns while their owners keep streaming and billing. The owner still stores its full answer once (`chat_conversation.go:259-267`). The error bubble exists only in the local store: `events.ts:107-123` appends it, and `appendMessage` in `stores/chat/messages.ts:19-32` does not persist it. A reload therefore shows the stored answer in place of the interrupted bubble. A duplicate, billed twice, appears only if the user re-sends the interrupted turn.

**Evidence.**
- `internal/service/chat_resume.go:154` — `Load`: "redis.Nil (miss) or any transport error → treat as not found" (`:155-157`); `Lookup` (`:162`) tells them apart, but `ResumeStream` (`:558`) and `OwnerOf` (`:448`) call `Load`
- `internal/api/handlers_chat.go:287` — `ownsStream`: the `owner == ""` check is at `:289` and the `STREAM_NOT_FOUND` refusal is rendered at `:292`; used by `/resume` (`:264`) and `/begin` (`:127`)
- `internal/api/handlers_chat.go:267` — `handleResumeStream` renders every `ResumeStream` error as 404 `STREAM_NOT_FOUND` (`:269`)
- `internal/service/chat_commands.go:563` — `OwnerOfChecked` tells an unreadable snapshot (`ErrReplicaUnreachable`, `:571-573`) from a miss; its only caller is `ownsStreamForCommand` (`handlers_chat.go:305`)
- `frontend/src/hooks/useStreamingMessage.ts:271` — after a reconnect, `resumeAll` resumes every stream (`:272-274`) with `onGone` → `reportLost` (`:255-259`), which emits `CHAT_ERROR_CODES.interrupted` when `isStreamGone` is true
- `frontend/src/lib/chatErrors.ts:37` — `isStreamGone` matches 403/404/410 only

**Change.** Route `/resume` and `/begin` through `OwnerOfChecked`/`lookupSnapshot`. On a backplane error `ResumeStream` returns `ErrReplicaUnreachable`, and the handler answers 503 `REPLICA_UNREACHABLE` with `Retry-After: 2`, as the commands do. A true miss keeps `STREAM_NOT_FOUND`. On the client, a 503 `REPLICA_UNREACHABLE` on resume is neither "gone" nor a stall-probe miss: the catch in `resumeInto` keeps the slot and schedules one retry after `retryAfterSeconds` before calling `onGone`. Widen ADR 0033 point 6 from the commands to the read path.

**Touches.** `internal/api/handlers_chat.go`, `internal/service/chat_resume.go`, `frontend/src/hooks/useStreamingMessage.ts`, `frontend/src/lib/chatErrors.ts`, `internal/api/handlers_chat_commands_test.go`, `docs/adr/0033-stream-commands-across-replicas.md`

**Done when.**
- Handler test: `/resume` for an id not in the local maps while the Redis client errors (not `redis.Nil`) answers 503 with code `REPLICA_UNREACHABLE` and `Retry-After: 2`. `/resume` for an unknown id while Redis answers is unchanged: 403/404 `STREAM_NOT_FOUND`.
- Service test: a backplane `Lookup` error makes `ResumeStream` return `ErrReplicaUnreachable`.
- `useStreamingMessage` test: after a reconnect, a resume rejected with `REPLICA_UNREACHABLE` emits no `STREAM_INTERRUPTED` and re-issues the resume after `retryAfterSeconds`.

**ADR.** ADR-0033 point 6 scopes `REPLICA_UNREACHABLE` to the commands; the read path is not covered.

### R-replicas-4

**Legacy two-POST /api/chat/begin is not relayed to the replica that owns the stream** · tier 1

**Problem.** A start without `clientStreamId` gets a server-generated id, and its output is held until `BeginStream` closes `ctl.started`. The start path closes it itself only when the client provided the id. `BeginStream` closes it only when the stream is in this replica's `activeStreams`. For a stream another replica runs, it falls through to `ResumeStream` and returns nil unless the snapshot is already final, and the handler renders that nil as `{status:ok}`. The command bus knows only `cancel` and `decide`, so nothing reaches the owner. Meanwhile the owner's provider callback blocks in `awaitStart` on the first chunk. No further `touch()` happens, and the idle watchdog cancels the stream after the idle limit. Because the callback blocks after the first chunk, the provider stream is no longer read. The bill is the input plus whatever output the provider produced before the context was cancelled, not necessarily a full completion. The bundled frontend always sets `clientStreamId`. The clients hit are API-token and third-party clients that follow the two-POST handshake documented on `ChatRequest.ClientStreamID` (`core/models/chat.go:102-105`). Whether a request fails depends on which replica the second POST lands on. The begin Swagger text (`handlers_chat.go:111-119`) does not describe the handshake.

**Evidence.**
- `internal/service/chat.go:729` — `ctl.started` is closed at start only when `clientProvided` (`:730-732`); "legacy leaves it to BeginStream"
- `internal/service/chat.go:782` — `awaitStart` blocks on `ctl.started` or `ctx.Done`; called at `:950` before any chunk is forwarded
- `internal/service/chat_resume.go:317` — `BeginStream`: a local stream closes `started` and replays the journal (`:318-322`); otherwise `ResumeStream`, returning nil unless the snapshot is final (`:331-334`)
- `internal/api/handlers_chat.go:132` — a nil from `BeginStream` is answered `{status:ok}` (`:136`)
- `internal/service/chat_commands.go:126` — the `chat.cmd` ops are only `cancel` and `decide`
- `internal/service/chat_resume.go:230` — the idle watchdog cancels after `idleLimitFor` with no activity; the blocked callback never calls `touch`
- `core/models/chat.go:105` — "Absent ⇒ the backend generates the ID (legacy two-POST handshake)"

**Change.** Choose one of two fixes. The first adds a third op, `begin`, to `chat.cmd`: the owner closes `ctl.started` and replays the journal to its own scope. The origin relays the begin when the snapshot names another live replica, and answers 503 `REPLICA_UNREACHABLE` when the relay fails, as the other commands do. The second refuses a missing `clientStreamId` with 400 when `MultiReplica` is on, and documents the two-POST handshake as single-replica only. Pin the choice in ADR 0033, the begin Swagger text and the `ClientStreamID` doc comment in `core/models/chat.go`.

**Touches.** `internal/service/chat_commands.go`, `internal/service/chat_resume.go`, `internal/api/handlers_chat.go`, `core/models/chat.go`, `docs/adr/0033-stream-commands-across-replicas.md`, `internal/service/chat_commands_test.go`

**Done when.**
- Two `ChatService`s on one miniredis: a stream started on A without `clientStreamId` and begun on B emits its first chunk on A's notifier within the test's timeout (or the start on B is refused with the documented 400).
- A begin relayed while the owner's bus is down answers 503 `REPLICA_UNREACHABLE`, not `{status:ok}`.
- The begin Swagger text states the multi-replica behaviour.

**ADR.** ADR 0033 point 3 lists only cancel and decide; begin is unspecified.

### R-replicas-6

**Liveness, the id claim, the caps and the ledger are gated on each process's own PAD_MAX_REPLICAS: a single replica with Redis, or a peer with a different value, writes live=false records that nobody can declare dead and nothing detects** · tier 1

**Problem.** Two groups of mechanisms switch on under different conditions. The resume store, command bus, approval records and conversation pointers attach whenever `PAD_REDIS_URL` is set. The liveness key, the id claim, the cross-replica caps and the shared ledger attach only when `multiReplicaClient` says `MaxReplicas > 1`, and each process decides that for itself. A single replica with Redis therefore writes snapshots and approval records with `live=false`. After a crash (OOM, SIGKILL) these records stay behind, naming a boot origin nobody listens on. `BootOrigin` gives every boot a new origin, so the "restart under the same name" branch of `CancelStreamAs` (`chat_commands.go:419-420`) never fires. What happens next:
- A stop goes to `callOwner` and gets no receiver. Because `live` is false, `ownerGone` never reports the owner gone, so the answer is 503 `REPLICA_UNREACHABLE` until the snapshot's 12-minute TTL runs out.
- A resume serves the stale live snapshot until the stall probes give up.
- A decision answers 503 until the approval key expires (`FixDecisionTimeout` 60 s + 10 s).
- When Redis hangs, each command on an unknown stream also waits out its lookup timeout: up to 2 s for `Lookup`, 1.5 s for `loadApproval`. A refused connection fails fast.

The same records appear during a rollout that raises `PAD_MAX_REPLICAS`, through overlapping revisions or a compose scale-up that leaves the env of the old container unchanged. The old replica's streams are in no shared ledger, cap or claim. The budget check cannot see its spend, the same `clientStreamId` can run twice, and a stop for one of its streams after it dies is 503 until expiry. The runbook's "one release" rule warns about release skew only. Nothing stamps or checks the configuration a peer runs with.

**Evidence.**
- `internal/di/services.go:269` — `multiReplicaClient` returns nil when `cfg.Storage.MaxReplicas <= 1`, so each process decides the ledger, caps, claim and liveness for itself (`:265-268`)
- `internal/di/services.go:594` — `Resume: redisClient` whenever Redis is configured; `MultiReplica` is `multiReplicaClient(...) != nil` (`:602`)
- `internal/service/chat.go:406` — `attachCluster` (resume store + command bus) for any `opts.Resume`; `shareAcrossReplicas` (liveness, claim, cap) only if `opts.MultiReplica` (`:408-410`)
- `internal/service/chat_replicas.go:195` — `ownerGone` returns false when `!live` (`:196`); `rs.live` is set true only in `shareAcrossReplicas` (`:140-143`); approval records carry `Live: cl.live != nil` (`chat_commands.go:593`) and snapshots carry `r.live` (`chat_resume.go:139`)
- `internal/service/chat_commands.go:419` — the "restart under the same name" branch (`:419-420`) never fires: `BootOrigin` gives every boot a new origin (`internal/redisx/origin.go:14-15`)
- `internal/service/chat_commands.go:429` — `CancelStreamAs` relays a snapshot naming another origin; it gets `errOwnerGone` only via `ownerGone`, else `ErrReplicaUnreachable` (`:430-437`)
- `internal/service/chat_resume.go:136` — `resumeStoreOpTimeout` (2 s) bounds `Lookup` (`:162-165`); `clusterOpTimeout` (1.5 s, `chat_commands.go:114`) bounds `loadApproval` (`chat_commands.go:617-620`)
- `internal/service/chat_commands.go:76` — the approval key's TTL is `ai.FixDecisionTimeout + approvalKeySlack`: 60 s (`internal/ai/tools.go:32`) + 10 s (`chat_commands.go:110`)
- `docs/architecture/operations.md:124` — the "one release" rule covers a release without the mechanisms, not a replica configured without them; `:108` says "A single replica keeps its in-process ledger even with `PAD_REDIS_URL` set"
- `docs/adr/0033-stream-commands-across-replicas.md:18` — point 8 scopes liveness and `owner_gone` to `PAD_MAX_REPLICAS > 1`

**Change.** Fix the single-replica case first, in one of two ways. Either write the liveness key on every Redis-backed replica (one SET per 5 s), so single-replica records carry `live=true` and the existing `owner_gone` path answers within 15 s. Or, when `!MultiReplica`, treat a snapshot or approval that names another origin as dead by configuration: compare-delete the snapshot, answer a stop with ok and a decision with 404. In both cases, make `Lookup`, `loadApproval` and `activeStreamOf` return at once while `Health` reports degraded. Document single-replica-with-Redis and configuration skew next to release skew in `operations.md`. An optional follow-up handles skew:
- Stamp the configured `MaxReplicas` (or a mechanisms bitmask) into the liveness record and the resume/approval envelopes.
- When a peer's record says 1 while this replica's value is above 1, log once and increment `pad_replica_config_skew_total`.
- Have the deploy workflow refuse a revision whose `PAD_MAX_REPLICAS` differs from the running one unless replicas are at 1. Optionally, derive "shared" from the count of live replica keys rather than from config alone.

**Touches.** `internal/service/chat.go`, `internal/service/chat_commands.go`, `internal/service/chat_replicas.go`, `internal/service/chat_resume.go`, `internal/di/services.go`, `internal/redisx/liveness.go`, `docs/architecture/operations.md`, `internal/config/deploy_replicas_test.go`

**Done when.**
- Single-replica test with miniredis: a snapshot written by a previous boot origin (`live=false`) exists. After a restart, a stop answers ok within 15 s and the snapshot is removed, a resume answers `STREAM_NOT_FOUND`, and an approval decision answers 404 within 15 s instead of 503 for 70 s.
- Single-replica test with Redis marked degraded: cancel, fix-decision, resume and begin for an unknown stream answer within 100 ms.
- Two-replica test with one replica built `MultiReplica=false`: the other replica logs the skew once and the metric increments. `operations.md` names configuration skew and the rollout order that avoids it.

**ADR.** ADR 0033 close-out and the ADR 0028 addendum gate liveness, owner_gone and the ledger on `PAD_MAX_REPLICAS > 1`; neither covers a single replica with Redis or mixed values.

### R-replicas-8

**Every provider memo and the circuit breaker are per-process, and the anonymous-struct memos escape the state registry** · tier 3

**Problem.** `TestArch_PerProcessStateDeclared` cannot see the anonymous-struct shape the AI package uses for its memos. The scanner's `TypeSpec` branch inspects only named struct types (`state_test.go:239-251`). Its `ValueSpec` branch counts a hit only in three cases: a collection type, a `make`/`new` of a collection, or an empty composite literal whose own type is a collection (`state_test.go:260-270`). So `var x = struct{ sync.Mutex; m map… }{m: …}` is never reported. Eight memos have this shape and no `stateRegistry` entry:
- `learnedRepairs`, `streamRefusals` and `embedDimsRefusals` (`openai_compat.go:341`, `:833`, `:951`)
- `responsesToolRoutes` (`responses.go:736`)
- `azureModelsCache` (`azure_foundry.go:534`)
- `agentKinds` (`azure_agents.go:522`)
- `entraSources` and `entraCLISources` (`azure_entra.go:317`, `:414`)

The `internal/ai` block of the registry lists only named state (`state_test.go:65-75`). ADR 0032 requires the registry to find "every keyed collection beside a lock" (item 7). That rule is therefore not enforced for exactly the shape the AI package uses. `tzLocCache` (`internal/api/dashboard_params.go:75`) and `proxySetCache` (`internal/api/middleware/ip.go:63`) have the same shape and are registered only by hand (`state_test.go:87-88`), so the shape is caught only when an author remembers it.

Separately, these memos and the circuit breaker are all per process. With `PAD_MAX_REPLICAS=N`, each replica pays once for the same lessons: a parameter rejection, a stream refusal, a Responses reroute decision, a served-model observation, a context-window rejection, and an Azure project listing (bounded at 10 s by `azureProjectExtrasTimeout`, `azure_foundry.go:527`, and cached 5 min, `:515`). That is a deliberate choice, not a defect. The registry accepts per-replica relearning for the equivalent named memos (`state_test.go:65-72`), and `operations.md:126` lists provider circuit breakers as per replica on purpose.

**Evidence.**
- `internal/archtest/state_test.go:239` — the `TypeSpec` branch inspects named struct types only (239-251)
- `internal/archtest/state_test.go:260` — a `ValueSpec` is a hit only for a collection type (260), `make`/`new` of a collection (263-266), or an empty composite literal whose `Type` is a collection (267-270)
- `internal/ai/openai_compat.go:341` — `learnedRepairs` is `var … = struct{ sync.Mutex; m map… }{…}`; `streamRefusals` (833) and `embedDimsRefusals` (951) have the same shape
- `internal/ai/responses.go:736` — `responsesToolRoutes`; `azure_foundry.go:534` `azureModelsCache`; `azure_agents.go:522` `agentKinds`; `azure_entra.go:317`/`:414` `entraSources`/`entraCLISources`: the same shape; `internal/archtest` names none of the eight
- `internal/archtest/state_test.go:65` — the `internal/ai` block (65-75): `estimateRequestTokensMemo` (65), `breakerRegistry` (67), `CopilotAuth` (68), `learnedWindows` (69), `modelListCache` (70), `servedMemo`/`servedByResource` (71-72), all replica-local with relearn-per-replica reasons
- `internal/archtest/state_test.go:87` — `tzLocCache` and `proxySetCache`, the same anonymous-struct shape, registered by hand (87-88)
- `internal/ai/learned_window.go:21` — the `learnedWindows` process map; `served_model.go:36` `servedMemo` and `servedByResource`
- `docs/adr/0032-cache-invalidation-fanout-and-state-registry.md:17` — item 7: the registry finds "every keyed collection beside a lock"
- `docs/architecture/operations.md:126` — "Per replica on purpose: … provider circuit breakers"

**Change.** Scope the work to the archtest scanner. Treat an anonymous struct type in a `var` `ValueSpec` the same way as a named struct: a lock field plus a collection field is a hit. Then register every anonymous-struct memo the scanner finds, across all packages, with a locality and a reason. Redis sharing of the memos and publishing breaker transitions are dropped. Both are per replica on purpose (`state_test.go:67-72`, `operations.md:126`).

**Touches.** `internal/archtest/state_test.go`

**Done when.**
- `TestArch_PerProcessStateDeclared` fails when a new `var x = struct{ sync.Mutex; m map[string]T }{…}` is added without a `stateRegistry` entry, and the eight existing memos have entries.

**ADR.** ADR-0032 §7 requires the declaration (not satisfied for the eight memos); sharing the memos or the breaker is not specified by any ADR

### R-replicas-11

**A conversation GET that follows the remote active pointer can wait ~2.4 s on a slow Redis despite the stated 400 ms bound** · tier 3

**Problem.** `activeStreamOf` bounds the pointer GET to `activeReadTimeout`: 400 ms, because "a slow Redis must not slow the conversation load" (`chat_commands.go:115-116`). It then calls `lookupSnapshot` on the request context (`:894`). `Lookup` bounds that read separately, at `resumeStoreOpTimeout`, 2 s (`chat_resume.go:136`, `:163`). `ReadConversation` resolves the remote stream before the history read (`chat_conversation_version.go:265-268`). The second round trip happens only when a pointer exists and names another replica (`chat_commands.go:887-893`). A conversation with no pointer gets `redis.Nil` on the first GET and returns after that one GET. A fully hung Redis is still capped at 400 ms, because the snapshot read is never reached. The ~2.4 s worst case needs three things at once: the pointer GET answers within 400 ms, the snapshot GET then stalls, and the reload lands while another replica runs the stream. That is a narrow window. In it, the page load stalls for the sum of both timeouts, which breaks the 400 ms bound per conversation read that ADR 0033 states (`docs/adr/0033-stream-commands-across-replicas.md:22`).

**Evidence.**
- `internal/service/chat_commands.go:116` — `activeReadTimeout = 400 ms`, "a slow Redis must not slow the conversation load" (115)
- `internal/service/chat_commands.go:884` — the pointer GET runs under the 400 ms context (884-886), then `lookupSnapshot(ctx, …)` runs on the request context (894)
- `internal/service/chat_commands.go:887` — no pointer, or a pointer to this replica, returns before the snapshot read (887-893)
- `internal/service/chat_resume.go:162` — `Lookup` applies its own 2 s `resumeStoreOpTimeout` (136, 163) on `context.WithoutCancel(ctx)`
- `internal/service/chat_conversation_version.go:262` — `ReadConversation` resolves the remote stream (266-267) before the history read
- `docs/adr/0033-stream-commands-across-replicas.md:22` — "one GET per conversation read (400 ms bound)"

**Change.** Bound both reads by one 400 ms deadline. Keep `rctx` open across the pointer read and the snapshot read, and pass it into `lookupSnapshot`. Skip the snapshot lookup when `cl.health.Degraded()` (`chat_commands.go:203`). `Lookup` derives its context from `context.WithoutCancel(ctx)` (`chat_resume.go:163`), which drops the caller's deadline along with its cancellation. So `Lookup` must also honour the caller's deadline (or take one), or `rctx` will not bound it. A pipeline cannot do both reads in one round trip, because the snapshot key comes from the pointer's value. Only a Lua script could, and that is not worth it here.

**Touches.** `internal/service/chat_commands.go`, `internal/service/chat_resume.go`, `internal/service/chat_commands_test.go`

**Done when.**
- With a Redis GET that hangs for 3 s on the snapshot key, `ReadConversation` returns within 500 ms with no `activeStreamId`.

### R-replicas-13

**Two-replica CI smoke cannot drive a chat stream: add a stub provider to the compose overlay** · tier 3

**Problem.** The gate for raising `maxReplicas` never streams a chat turn. `scripts/ci/two-replica-smoke.sh` says it does not cover four things, because the stack has no AI provider: stopping a stream from the other replica, deciding an approval from the other replica, the shared AI budget and the knowledge lease (`two-replica-smoke.sh:41-43`). Its conversation scenario only calls `/api/chat/get` and `/api/chat/save` (`:254`, `:259`). The cross-replica command bus, the Redis in-flight ledger and the liveness fallback are proven only in process, with miniredis (`internal/di/two_replicas_test.go:270-271`). The code can already point a provider at any base URL at package init, through `OPENAI_API_URL` and the other `*_API_URL` variables (`providerconfig.go:5-17`). It can also lift the private-egress guard with `PAD_AI_ALLOW_PRIVATE_EGRESS` (`loader.go:426`), which is applied to the fixed-host client (`services.go:157-160`, `internal/ai/spec.go:364-370`). A compose-internal SSE stub can therefore be wired in without changing the providers.

**Evidence.**
- `scripts/ci/two-replica-smoke.sh:41` — "Not covered here, because they need an AI provider the stack does not have: stopping a chat stream and deciding an approval from the other replica …, the shared AI budget, and the knowledge-base lease"
- `scripts/ci/two-replica-smoke.sh:254` — only `/api/chat/get` (254) and `/api/chat/save` (259) are exercised
- `internal/di/two_replicas_test.go:270` — `scenarioStreamCommands` and `scenarioSharedBudget` run in process only (270-271)
- `internal/ai/providerconfig.go:12` — `providerURL` reads `*_API_URL` at init, so a stub endpoint can replace a fixed host
- `internal/config/loader.go:426` — `PAD_AI_ALLOW_PRIVATE_EGRESS` admits a private-network provider host
- `internal/di/services.go:157` — `ConfigureProviders` receives the egress policy and `AllowPrivateEgress` (160); `internal/ai/spec.go:364-370` applies it to the shared fixed-host client
- `internal/api/handlers_chat.go:37` — a chat budget refusal is an error event on the stream with code `AI_BUDGET_EXCEEDED`, not an HTTP status (the check runs in the stream worker, `internal/service/chat.go:854`)

**Change.** This is tier 3: extra coverage for a multi-replica mode that is not yet enabled, whose logic the in-process miniredis scenarios already cover. Add a small stub service to `docker-compose.replicas.yml`. It is an OpenAI-wire SSE server that answers `/v1/chat/completions` with a scripted, pace-controlled stream and a `tool_call` turn. On both replicas, set `OPENAI_API_URL` to the stub and `PAD_AI_ALLOW_PRIVATE_EGRESS=true`. Seed an openai key through `/api/keys/save`. Add three smoke scenarios:
- (9) Start a stream on A, confirm the resume snapshot on B, stop the stream from B, and expect `STREAM_STOPPED` on A's SSE.
- (10) A stream with an approval proposal decided from B.
- (11) With a $0.01 daily budget and one running stream on A, a second stream started on B fails with `AI_BUDGET_EXCEEDED`.

Document the stub in `safety-nets.md` and `operations.md`.

**Touches.** `docker-compose.replicas.yml`, `scripts/ci/two-replica-smoke.sh`, `scripts/ci/ai-stub/` (new), `.github/workflows/ci.yml`, `docs/architecture/safety-nets.md`, `docs/architecture/operations.md`

**Done when.**
- The two-replica job prints PASS for a stream started on A and stopped from B, and `pad_chat_commands_total{op="cancel",side="origin",outcome="ok"}` increments on B.
- A decision posted to B for a proposal raised on A results in a `fix_decision` event on A's SSE.
- With a $0.01 daily budget and one running stream on A, a second stream on B is refused with `AI_BUDGET_EXCEEDED`, naming the in-flight spend.

### R-replicas-c23

**No org-level or replica-level limit on concurrent AI streams; the only concurrency cap is a compile-time 3 per member** · tier 3

**Problem.** `tryStartStream` (`chat.go:216-224`) and the cross-replica `SlotCap` (`chat_replicas.go:59`, acquired with `ctl.ownerID` at `:94`) count streams only per `ownerID`. Nothing limits an organisation's concurrent streams or a replica's total. An org with N members can run 3N concurrent streams (`chat.go:39`). Each lasts up to 10 minutes (`chat.go:36`) with up to 6 tool iterations (`chat.go:42`). All of them run on the org's shared credential when members have no key of their own (member-first fallback, `chat.go:503`, `factory.go:161`). By design, provider 429s on that shared key do not trip the circuit breaker (`circuitbreaker.go:327-331`). Each call makes up to 3 attempts in total, 1 initial plus 2 retries (`retry.go:12-13`). The count is configurable through `RetryMaxAttempts` (`config.go:385`, default 3 at `:440`). Each call also honours Retry-After up to 20 s (`retry.go:72`). So contention on the shared key turns into retry traffic against it, and every waiting stream keeps its in-flight reservation against the org budget. Org spend is still bounded by the org `DailyBudget` and `PerUserDailyBudget` (`core/models/settings.go:100-103`). The issue is concurrency and contention on the shared key, not unbounded spend. The per-member cap is a const that operators cannot tune, and `operations.md` documents only the per-user cap ("3 chat streams per user", `operations.md:126`).

**Evidence.**
- `internal/service/chat.go:39` — `const maxConcurrentStreamsPerScope = 3` ("bounding one user's spend")
- `internal/service/chat.go:216` — `tryStartStream` counts only entries whose `ownerID == scope` (216-224); no org or global count
- `internal/service/chat_replicas.go:59` — `SlotCap` over `chatStreamSlotsKey`, acquired with `ctl.ownerID` (94): per member only
- `internal/service/chat.go:503` — `turnProvider` uses `factory.ForOrg` with the org credential as member-first fallback
- `internal/ai/factory.go:161` — `memberFirst`: the caller's own key, otherwise the organisation's shared one
- `internal/ai/circuitbreaker.go:329` — `ErrRateLimited` never counts toward opening the breaker (327-331)
- `internal/ai/retry.go:12` — `retryMaxAttempts = 3` is the total number of tries (12-13); `maxRetryAfterWait = 20 s` (72)
- `internal/config/config.go:385` — `RetryMaxAttempts`, default 3 (440)
- `core/models/settings.go:100` — `DailyBudget` and `PerUserDailyBudget` (100-103)
- `docs/architecture/operations.md:126` — "3 chat streams per user" is the only documented chat stream cap

**Change.** Add an org slot cap: a `redisx.SlotCap` on a declared key such as `baki:chat:org-streams:{org}`, failing open when Redis is down, like the member cap. Add a per-replica ceiling. Make the member, org and replica caps configurable through `internal/config`, with bounds (for example `PAD_CHAT_MAX_STREAMS_PER_USER`, `_PER_ORG`, `_PER_REPLICA`). Refuse with `CHAT_CAPACITY_REACHED` and a `reason` field (`member`, `org` or `server`) so the client can explain which limit was hit. Count refusals per reason in a metric. Update `redis_keys.golden`, `operations.md` and `.env.example`.

**Touches.** `internal/service/chat.go`, `internal/service/chat_replicas.go`, `internal/redisx/`, `internal/config/config.go`, `internal/config/loader.go`, `internal/di/testdata/` (redis keys golden), `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts`, `docs/architecture/operations.md`

**Done when.**
- Test: with an org cap of 2, a third concurrent stream on that org's flows from any member is refused with `CHAT_CAPACITY_REACHED` and reason `org`. Personal flows are unaffected.
- Two-replica test: the org cap holds across replicas while Redis is up. When Redis is down it degrades open and records a `pad_redis_caps_total` outcome.
- The config loader rejects zero or negative caps, and `operations.md` and `.env.example` document the settings.
