# AI roadmap: Streaming

Items of category `streaming` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-streaming-1

**parseGeminiSSE emits no keepalives, drops reported usage on a filtered finish, cannot classify a mid-stream error event and carries no served model** · tier 1

**Problem.** The Gemini SSE parser decodes only `candidates` and `usageMetadata`. It produces a chunk only for a text part or a `finishReason`; function calls are collected and sent on the Done chunk. This has three consequences, each the opposite of what the Claude, OpenAI and Responses parsers do.

(1) An event that delivers nothing emits no chunk: a usage-only event, a functionCall-only part, or a candidate with only safety ratings. Once the first text has cleared `awaitingOutput`, nothing feeds the chat idle watchdog, and a silent phase over 90 s is cancelled as "the AI provider stopped responding". In practice this is narrower than it sounds. Thought parts are never requested (`geminiPart` has no thought field), so a Gemini thinking phase usually sends no events at all. The phase before first output is already covered by the hosted limit that `awaitingOutput` applies. The realistic cases are events after first output that carry only a functionCall, usage or safety data.

(2) A SAFETY/RECITATION/BLOCKLIST/PROHIBITED_CONTENT finish computes `tokensIn`/`tokensOut` from `usageMetadata`, then emits an error chunk without them. The audited wrapper therefore bills an estimate for a run Gemini priced exactly.

(3) A `data: {"error":{…}}` event after the 200 decodes as an empty event: `json.Unmarshal` ignores the unknown field, so `parseErrors` is not incremented. The stream normally ends as `errStreamTruncated` wrapping `io.ErrUnexpectedEOF`; `errStreamMalformed` appears only if other lines failed to decode. The server's code, status and message are lost, the error is not retryable, and the breaker never counts the outage.

Finally, no chunk ever carries `ServedModel` (`modelVersion` is ignored), so usage rows and the served-model memo never learn which model answered.

**Evidence.**
- `internal/ai/stream.go:828` — the decoded struct (`828-842`) has `Candidates` and `UsageMetadata` only, with no `error` object and no `modelVersion`; an unknown `error` field decodes silently, and only an undecodable line increments `parseErrors` (`844-847`)
- `internal/ai/stream.go:849` — the scan loop (`849-891`) emits only for `part.Text` (`852`) and `cand.FinishReason` (`868-889`); `part.FunctionCall` is collected (`854-866`) and sent on the Done chunk (`886`); there is no `!emitted → onChunk(Chunk{})` branch and no `ServedModel` on any chunk
- `internal/ai/stream.go:879` — the filtered-finish chunk is `Chunk{Error: ...}` without `TokensIn`/`TokensOut`, although `871-875` computed both from `usageMetadata`; a stream that ends without a finish returns `errStreamMalformed`/`errStreamTruncated` at `898-903`, and the latter wraps `io.ErrUnexpectedEOF` (`47-48`)
- `internal/ai/stream.go:664` — contrast: `parseOpenAISSE` decodes `error` events into a classified chunk (`664-676`), emits a keepalive per empty event (`723-728`; Claude at `507-511`) and carries usage on the content-filter chunk (`587-596`; the Claude refusal at `445`)
- `internal/ai/gemini.go:241` — `geminiErrorResp` (code/message/status) exists, but the stream parser never consults it; `isRetryable` (`retry.go:49-51`) matches only `ErrRateLimited`, `ErrProviderDown` and net transport errors, so the Gemini failure is never retried
- `internal/ai/gemini.go:179` — `geminiPart` (`179-183`) has no thought field, so thought parts are never requested
- `internal/ai/audited.go:285` — a stream without Done and without reported tokens is billed from `estOut.total()`/`estimateRequestTokens` (`285-291`); on the watchdog side, `sawOutput` clears `awaitingOutput` on first text (`chat_stream_control.go:249-253`) and the 90 s limit applies (`chat_resume.go:50`, `198-210`, `230-232`)

**Change.** Four changes in `parseGeminiSSE`:
- Track `emitted` per decoded data line, and emit `Chunk{ServedModel: servedModel}` when a line produced nothing, as the Claude and OpenAI parsers do.
- Put `TokensIn`/`TokensOut` from `usageMetadata` on the filtered-finish error chunk.
- Add an optional `error{code,status,message}` field to the decoded struct and map it through a `classifyGeminiStreamError`: 429/`RESOURCE_EXHAUSTED` → `ErrRateLimited`, 5xx/`UNAVAILABLE`/`INTERNAL` → `ErrProviderDown`, anything else stays a plain error. Emit the result as an error chunk carrying the usage seen so far, and end the stream with nil.
- Read `modelVersion` into `servedModel` and carry it on text, keepalive, Done and error chunks.

Pin all four in `stream_test.go` and the spend-integrity test.

**Touches.** `internal/ai/stream.go`, `internal/ai/gemini.go`, `internal/ai/stream_test.go`, `internal/ai/gemini_errors_test.go`, `internal/ai/spend_integrity_test.go`

**Done when.**
- A Gemini SSE fixture of three usage-only/empty-parts events followed by a text event invokes `onChunk` three times with field-less keepalives before the text chunk.
- A SAFETY finish with `usageMetadata` {prompt 100, candidates 20, thoughts 30} yields an error chunk with `TokensIn` 100 and `TokensOut` 50, and the audited wrapper records those exact counts, not an estimate.
- A data line `{"error":{"code":503,"status":"UNAVAILABLE","message":"x"}}` ends the stream with an error chunk satisfying `errors.Is(ErrProviderDown)` and a nil return. `RetryingProvider` retries it when no content was delivered, and the `modelVersion` from the stream appears as `Chunk.ServedModel` on the Done chunk.

### R-streaming-3

**A panic inside the emit path leaks the coalescer's emitMu and hangs the stream's cleanup forever** · tier 1

**Problem.** `chunkCoalescer.wrap` and `add` take `emitMu` without `defer` and call the base emit inside the critical section. That base emit does a lot of work:
- it claims the terminal event
- it runs `storeAnswer`, a Postgres write with CAS rounds bounded by `persistAnswerTimeout` (15 s)
- it journals and stamps the conversation version
- it delivers to every SSE client and to the Redis fan-out

The worker's `recover` exists for a panic in exactly this code, but Go runs deferred functions LIFO. The cleanup defer, registered after the recover, runs first. Its `ensureTerminal` emit("error") and then `coalesce.flush()` both try to take the still-locked `emitMu` and block forever. The batch-timer flush and a heartbeat emit already in flight also block forever on the leaked `emitMu`. The watchdog does not: it exits when the 10-minute ctx ends. The panic never reaches the recover, and nothing after the blocked cleanup runs:
- `workers.Done` never runs, so `Shutdown`'s Wait times out on every deploy.
- The mirror worker waiting on `mirrorFinished` leaks with it.
- `activeStreams` keeps the entry, so one of the caller's 3 slots is used up for the life of the process ("too many chat responses running").
- The budget reservation and the share hold are never released.

All of this happens only if something in the emit path panics. No concrete panic source is evident in `EmitTo` or `deliverTo`. This is a latent defect that defeats the existing recover, not one seen in practice.

**Evidence.**
- `internal/service/chat_scrubber.go:92` — `wrap`: `emitMu.Lock(); flushLocked(); c.emit(...); emitMu.Unlock()` with no defer (`92-95`)
- `internal/service/chat_scrubber.go:119` — `add`: the same pattern around the first-chunk emit (`119-121`); only `flush` (`133-137`) defers the unlock; the batch timer calls `flush` (`126`)
- `internal/service/chat.go:753` — the base emit runs under `emitMu`: `claimTerminalEvent` (`756`), `storeAnswer` (`759-764`, a DB write; `persistAnswerTimeout` = 15 s at `chat_conversation.go:197`), `stampConversationVersion`, `recordJournal`, `notifier.EmitTo` (`766-775`)
- `internal/service/chat.go:800` — the recover defer (`800-804`) is registered before the cleanup defer (`810`), so on a panic the cleanup runs first, and its emit("error") at `816` and `coalesce.flush()` at `820` both block on the leaked `emitMu`
- `internal/service/chat.go:797` — `workers.Done` is the outermost defer; `close(mirrorFinished)` (`823`, awaited by the mirror worker started at `745`), `activeStreams.Delete` (`827`), the budget release (`828`) and `share.finish` (`829`) never run
- `internal/service/chat.go:39` — `maxConcurrentStreamsPerScope = 3`: the leaked entry permanently takes one of the caller's slots
- `internal/service/chat_stream_control.go:216` — the heartbeat emit (goroutine started at `chat.go:844`) goes through the same `emitMu`; the watchdog exits with the 10-minute ctx (`chat.go:704`)
- `internal/api/events.go:243` — `EmitTo` (`243-245`) and `deliverTo` (`385-401`) show no concrete panic source

**Change.** The essential fix: defer `emitMu.Unlock` in `wrap` and `add`, and make the cleanup's `ensureTerminal` emit and flush best-effort with their own recover. Beyond that, recover inside the coalescer's emit, so a panicking delivery cannot poison later emits, and register the recover defer last, so it runs before the cleanup. Moving `storeAnswer` out of the coalescer's critical section must keep the ADR 0031 rule that the answer is stored before "done" leaves. Do it by persisting in the worker before it calls emit("done"), not by reordering inside the coalescer. Add a test with a notifier whose `EmitTo` panics once.

**Touches.** `internal/service/chat_scrubber.go`, `internal/service/chat.go`, `internal/service/chat_shutdown_test.go`

**Done when.**
- Test: a notifier that panics on the first chunk leaves the stream recorded as `STREAM_INTERRUPTED`; the stream settles, leaves `activeStreams`, releases its ledger entry (`others() == 0`), and `workers.Wait` returns.
- A race-enabled run of the chat service package stays green.
- A heartbeat emit is never blocked for longer than one `persistAnswerTimeout`.

### R-streaming-6

**SSE reconnect give-up is sticky for the session: the header shows 'ready' and in-flight streams never resolve** · tier 1

**Problem.** After `SSE_MAX_RECONNECT_ATTEMPTS` (10) failed reconnects, about 181 s of backoff (1+2+4+8+16+5×30 s), the client sets the connection to "idle". From then on it waits only for a once-only window "online" event. The app-level `useAppEvents` subscription keeps `streamActive` true for the tab's lifetime, so `subscribeToEvents` never calls `connectEvents` again. A backend outage or deploy longer than three minutes therefore leaves chat dead for the rest of the tab's life, even though the server is back and every other API call succeeds. Every send then waits `OPEN_WAIT_TIMEOUT_MS` (15 s) for "open", fails with the `cannotConnect` bubble, and posts a cancel for a stream that was never created. A stream that was in flight when the attempts ran out is never probed, because `probeStall` re-arms without probing while the state is not "open". It sits on screen forever.

**Evidence.**
- `frontend/src/api/client.ts:594` — after `SSE_MAX_RECONNECT_ATTEMPTS` (10, line 533): `sseGivenUp`, state "idle", and only a once-only window "online" handler restarts (`594-611`)
- `frontend/src/api/client.ts:613` — delay = min(30 s, 2^n s): ten attempts total about 181 s
- `frontend/src/api/client.ts:748` — `subscribeToEvents` calls `connectEvents` only when `!streamActive` (`748-756`)
- `frontend/src/hooks/useAppEvents.ts:46` — the app-level hook's `subscribeToEvents` listener keeps `streamActive` true for its lifetime
- `frontend/src/hooks/useStreamingMessage.ts:366` — a new send waits for "open" up to `OPEN_WAIT_TIMEOUT_MS` (15 s, line 26), then tears down and rejects with `cannotConnect`
- `frontend/src/hooks/useStreamingMessage.ts:210` — `probeStall`: if `getEventConnectionState() !== 'open'`, it re-arms and returns without POSTing the resume (`210-213`)
- `frontend/src/components/chat/ChatHeader.tsx:55` — `toDisplayState` maps every state other than "reconnecting" and "open" to "ready" (`55-59`)

**Change.** While listeners exist, drop the hard cap on attempts and keep retrying at the 30 s ceiling. Reconnect at once on "online", on a `visibilitychange` to visible, and on any successful API response while the state is not "open" (hook into `noteServerVersion`). Make `registerStream` start `connectEvents` when the state is "idle" instead of only waiting. Let `probeStall` run its `/api/chat/resume` probe even while SSE is down, since it is plain HTTP, so a finished stream commits. Add a "disconnected" display state to `ChatHeader` with a title other than "ready".

**Touches.** `frontend/src/api/client.ts`, `frontend/src/hooks/useStreamingMessage.ts`, `frontend/src/components/chat/ChatHeader.tsx`, `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts`, `frontend/src/i18n/en.ts` (chat connection keys)

**Done when.**
- `client.sse.test`: after 10 failures, a successful request to any API path (or a `visibilitychange` to visible) triggers a new `/api/events` fetch within 1 s, and the state never reports "idle" while a listener exists.
- `ChatHeader` test: the "reconnecting" and "disconnected" states never render the success dot, and their title is not "ready".
- `useStreamingMessage` test: with the connection "reconnecting", a stream whose `/resume` answers done commits via `onDone` within one probe interval.

### R-streaming-c8

**On Windows the sidecar is always hard-killed after about one second, so the ADR 0024 stop (ChatService.Shutdown, finishing-answer persist, stop hooks) never runs and nothing guard…** · tier 1

**Problem.** On non-Unix systems, `term_signal` is a no-op and `process_alive` always returns false. `graceful_terminate` therefore sleeps one second, sees "not alive", breaks out of the grace loop and calls `child.kill()` (TerminateProcess). Windows is PAD's main platform, so on Windows a desktop close or restart never reaches fx's stop. `registerDrain`/`ChatService.Shutdown` never refuses new streams, ends running ones or waits for workers. A just-finished answer's `persistAssistantTurn` (up to 15 s) is cut off. The question is persisted at stream start and the answer only at the end, so a close mid-turn leaves an unanswered question stored. An approved `apply_fix` that is interrupted after its file write leaves the user's flow changed while the stored conversation holds only the unanswered question.

**Evidence.**
- `src-tauri/src/lib.rs:121` — `#[cfg(not(unix))] fn term_signal(_pid: u32) {}`: no graceful request on Windows
- `src-tauri/src/lib.rs:137` — the non-Unix `process_alive` returns false, so the grace loop (`147-152`, `161-166`) breaks after its first 1 s sleep
- `src-tauri/src/lib.rs:412` — `ExitRequested` goes straight to `graceful_terminate_blocking`, with no check with the UI about in-flight work
- `desktop_boot_test.go:130` — "killed instead, as the Tauri shell stops the backend on Windows"
- `internal/app/server.go:168` — `registerDrain` → `chatSvc.Shutdown` is reached only through fx stop
- `internal/service/chat_resume.go:381` — `Shutdown`: refuse new streams, cancel with `SERVER_RESTARTING`, wait for workers to finish storing answers

**Change.** Add a graceful stop that works on Windows. Either expose a stop trigger that exists only in the desktop profile, listens only on loopback and is authenticated with the session key the shell already holds (for example `POST /api/internal/shutdown` calling `fx.Shutdowner`), or have the desktop backend treat EOF on stdin as a stop request. In `lib.rs`, call it, then wait up to `BACKEND_SHUTDOWN_GRACE_SECS` before the hard kill. Wait on the plugin's `CommandEvent::Terminated`, or on a real Windows liveness check (`OpenProcess`/`GetExitCodeProcess`). Add a close-requested hook (`TauriAdapter.ts`) that asks for confirmation while a chat stream or a pending approval is live.

**Touches.** `src-tauri/src/lib.rs`, `internal/app/server.go`, `internal/api/router.go`, `internal/service/conversation_store.go`, `desktop_boot_test.go`, `frontend/src/platform/adapters/TauriAdapter.ts`

**Done when.**
- `desktop_boot_test` on Windows stops the backend through the new trigger and asserts exit code 0 (the "killed instead" branch is gone).
- Closing the app on Windows right after an answer has finished leaves that answer in the conversation file.
- Closing the app while a chat stream or a pending approval is live shows a confirmation.

### R-streaming-c20

**A Stop or approval decision refused by the general per-IP bucket or the per-user write cap is silently dropped, so the billed stream keeps running** · tier 1

**Problem.** `POST /api/chat/cancel` is subject to two shared limits:
- The general per-address bucket: 1 rps with burst 20, shared with every unclassified route of every client behind that address.
- The per-user write cap: 10/s with burst 20, shared with every POST, including the chat UI's own POST reads (get, resume stall probes, preview-context, save).

When cancel gets a 429, the client does not retry, because it retries only `REPLICA_UNREACHABLE`. It logs a warning and tears down the stream subscription anyway, and `closeThread` swallows the error entirely. The server watchdog cancels a stream only when the user has no SSE subscriber at all. The user's app-wide SSE connection is still open, so the stream keeps running and billing. Approval decisions share the same buckets (`fix-decision` is `ratelimit=general` too), but they fail visibly. A refused decision switches the card to the `decisionFailed` error state, with no retry on 429. An undelivered decision leaves the stream parked until its approval timeout instead of billing. The Stop path is the main problem; fix-decision is a secondary UX issue.

**Evidence.**
- `internal/api/testdata/route_policy.golden:395` — `POST /api/chat/cancel ... ratelimit=general`
- `internal/api/testdata/route_policy.golden:405` — `POST /api/chat/fix-decision ... ratelimit=general`
- `internal/api/route_policy.go:263` — an undeclared `RateLimit` resolves to `route.RateGeneral` (`263-269`); cancel declares none (`handlers_chat.go:557`)
- `internal/config/config.go:416` — the general bucket defaults to 1.0 rps / burst 20 (`416-417`); per-user is 10 / 20 (`436-437`)
- `internal/api/per_user_ratelimit.go:68` — `perUserKey` charges every POST/PUT/PATCH/DELETE to the user's write bucket (`68-74`; `isWriteMethod` at `83-89`), cancel included
- `frontend/src/api/chat.ts:57` — `cancelWithRetry` retries only on `isReplicaUnreachable` (`57-68`); a 429 is thrown
- `frontend/src/hooks/useStreamingMessage.ts:196` — on a failed cancel the hook logs and calls `teardownStream` anyway (`196-200`)
- `frontend/src/stores/chat/threads.ts:132` — `closeThread` calls `cancelStream(...).catch(() => {})`
- `internal/service/chat_resume.go:243` — the watchdog resets its miss count while `HasSubscriber(scope)` is true
- `frontend/src/components/chat/hooks/stream-engine/fixes.ts:149` — a failed decision sets the card to the `decisionFailed` error state (`149-150`)

**Change.** Treat stream-control commands as spend-reducing. Declare `/api/chat/cancel` with `route.RateLimit(route.RateNone)`, and exempt it in `perUserKey`. Do that either through a new route policy flag (for example `route.Control()`) or by giving cancel and fix-decision their own generous per-user "control" bucket. Mirror the change in the legacy classifier `rateLimitGroup` (`middleware_chain.go:179`). On the client, handle a 429 on cancel like `REPLICA_UNREACHABLE`:
- retry after the `Retry-After` hint
- keep the stream marked as stopping and show the stop-retry toast
- keep the subscription until the server confirms the stop or the stream ends

Give a 429 on fix-decision a retry instead of the terminal `decisionFailed` state.

**Touches.** `internal/api/handlers_chat.go`, `internal/api/per_user_ratelimit.go`, `internal/api/middleware_chain.go`, `internal/api/route/`, `internal/api/testdata/route_policy.golden`, `frontend/src/api/chat.ts`, `frontend/src/hooks/useStreamingMessage.ts`, `frontend/src/stores/chat/threads.ts`

**Done when.**
- In cloud mode with the general and per-user buckets drained (RPS 0.001, burst 1), `POST /api/chat/cancel` for the caller's own stream reaches the handler and stops the stream in every route-policy mode.
- `route_policy.golden` shows cancel with `ratelimit=none` (or the new control group), and a `perUserKey` unit test pins the exemption.
- Frontend test: a 429 from cancel is retried per `Retry-After`, the stop-retry toast is shown, and the subscription stays until the server confirms the stop or the stream ends.

### R-streaming-4

**Cross-replica reattach commits a truncated answer: chunks carry no offset and the finishing resume races the owner's final snapshot** · tier 2

**Problem.** Take a client attached to replica B for a stream running on replica A, for example after a reload mid-turn or an SSE reconnect to another replica. The client marks its chunk count invalid on every reconnect, so the done event triggers a full resume with finish. `ResumeStream` on B is not local, so it answers from the Redis snapshot. The owner rewrites that snapshot at most once per second. It finalises it only when the worker's deferred cleanup closes `mirrorFinished`, which happens after the done event has already been sent (`markDone`, then `emit("done")`) and fanned out. `resumeInto` replaces the accumulated text with whatever the snapshot holds and calls `onDone` whenever finish is set, even when the snapshot says `done=false`. The committed message can therefore miss the last second of text.

Under normal conditions A's final Save usually wins the race, because B's path needs a pub/sub hop, a browser round trip and two Redis GETs (`OwnerOf` at `chat_resume.go:448`, then `:558`). The sharper failure is a slow or failed final write. The final Save at `chat_resume.go:274` is never retried and its result is ignored, and Save fails open with a 2 s bound (`chat_resume.go:136-151`). After a Redis blip at the end of a stream, the last tick's `done=false` snapshot stays for the key's full TTL, and every finishing resume on another replica commits that stale text.

The turn is server-owned, so the client never corrects it. The client adopts the server's version from the done meta (`events.ts:52`). A later compaction (`replaceThreadConversation`, `conversationSync.ts:106-116`) can then write the truncated answer over the stored one. Chunk events carry no byte offset either, so a client that resumed from a snapshot and then receives chunks already in it cannot drop them.

Frequency is low: the failure needs a cross-replica client plus a reconnect, reload or dropped chunk during the turn. Tier 2 fits better than tier 1.

**Evidence.**
- `frontend/src/hooks/useStreamingMessage.ts:324` — a done with `countsInvalid` or a chunk-count mismatch calls `resumeInto` in mode `full` with finish (324-328)
- `frontend/src/hooks/useStreamingMessage.ts:273` — `resumeAll` sets `countsInvalid = true` for every live stream on reconnect or replay:gap (271-275)
- `frontend/src/hooks/useStreamingMessage.ts:138` — `resumeInto` calls `onReplace(res.text)` at 153, then `onDone` when `res.done || finish` (161-167), regardless of `res.done`
- `internal/service/chat_resume.go:558` — `ResumeStream` answers from the backplane snapshot for a stream not on this replica (558-559)
- `internal/service/chat_resume.go:266` — `mirrorStream` writes at most once per `resumeMirrorInterval` (1 s, line 39), and writes the final snapshot only on `<-finished` (272-274)
- `internal/service/chat.go:980` — `markDone`, then `emit("done")` at 981 (tool loop: 1150-1152); `mirrorFinished` is closed only in the deferred cleanup at 823

**Change.** Make done authoritative and ordered:

- **Server:** when the backplane is enabled, write the final snapshot between `markDone` and `emit("done")`, with a bound, and check its result instead of ignoring it. Stamp done (and error) with the final text's byte length.
- **Client:** a finishing resume whose done is false, or whose text is shorter than the stamped length, is not final. Keep the longer accumulated text and re-resume with a short backoff until the lengths match.
- **Protocol:** carry a byte offset on chunk events, so a reattached client drops chunks at or below its offset.

Document both the done stamp and the chunk offset in ADR 0030.

**Touches.** `internal/service/chat.go`, `internal/service/chat_resume.go`, `internal/service/chat_scrubber.go`, `frontend/src/hooks/useStreamingMessage.ts`, `frontend/src/lib/chatEvent.ts`, `docs/adr/0030-cross-replica-commands-and-sse-replay.md`

**Done when.**
- Two-replica test: the client is attached to B, and A's final mirror write is delayed 500 ms after done. The committed assistant text equals the stored answer byte for byte.
- `useStreamingMessage` test: a finishing resume that answers `done=false` with shorter text does not commit; the next resume (`done=true`) does.
- A client that resumed from a snapshot and then receives chunks already in it shows no duplicated text (offset-based dedupe test).

**ADR.** ADR 0030 §1 documents the chunk gap and recovery by resume-by-offset; it does not cover the race at done.

### R-streaming-5

**Delta resume after a reconnect drops the server's events/done when a live chunk raced it; the stall probe never applies events** · tier 2

**Problem.** On reconnect or replay:gap, every stream resumes in delta mode. A live chunk can arrive between computing the offset and the resume response, which is the common case while an answer is flowing. When that happens, `resumeInto` issues a full refetch that only calls `onReplace` with the text and returns early. It skips `res.events`: the tool results, fix proposals and decisions emitted during the gap.

Done and error are not lost there. The racing chunk proves the connection is open, so a later done or error arrives live, and that chunk also re-armed the stall timer (`useStreamingMessage.ts:308-311`). The lost events are also repaired before commit:

- `resumeAll` sets `countsInvalid` (`:273`) and nothing resets it.
- So the later done always goes through `resumeInto` in full mode (`:324-328`).
- That path applies `res.events` through `onResumeState` (`:156-157`, `fixes.ts:159-187`) before `onDone` (`:161-167`).
- `commit.ts:87-99` then copies the corrected cards and tool calls.

The real harm of the race is a stale tool trail and stale approval cards until done.

`probeStall` has a worse hole: it applies text, done and error, but never `res.events`. A wrong committed state is reachable through two paths:

- `probeStall`'s done path (`:232-234`), for example on a silent half-open SSE where the 30 s probe fires before the 45 s inactivity reconnect (`client.ts:514`);
- `resumeInto`'s catch path (`:174`).

On either path, a `fix_decision` lost in the gap leaves the card at "applying". Commit then copies that status and the incomplete tool calls into the message's `fixProposals` and `toolCalls`. For server-owned turns the client commit is not saved (`events.ts:58`), so the wrong state lasts only in the local view, until reload.

**Evidence.**
- `frontend/src/hooks/useStreamingMessage.ts:139` — delta branch: if `now > from`, it calls `resumeStream(id, 0).then(fullRes => onReplace(fullRes.text))` and returns (141-149). That call skips `res.events` (156), `res.error`/`res.done` (159-167) and `armStall` (169).
- `frontend/src/hooks/useStreamingMessage.ts:271` — `resumeAll` always uses mode `delta` on reconnect or gap (274)
- `frontend/src/hooks/useStreamingMessage.ts:223` — `probeStall` handles `res.error` (227), `res.text` (231) and `res.done` (232), but never passes `res.events` to `onResumeState`
- `internal/service/chat_resume.go:552` — `ResumeStream` returns `Events` (`snapshotEvents`) on every result, local (552) or backplane (577)
- `frontend/src/components/chat/hooks/stream-engine/commit.ts:87` — commit copies `slot.toolCalls` (87-89) and `slot.approvals` statuses (90-99) into the committed message
- `frontend/src/components/chat/hooks/stream-engine/fixes.ts:159` — `onResumeState` rebuilds the trail and cards from events; the race branch never reaches it, though the later full-mode resume at done does

**Change.** Factor out one `applyResume(res, {mode})` used by `resumeInto`, its race refetch and `probeStall`. It replaces or appends the text, applies events through `onResumeState`, then handles error, done and `armStall`. In the race branch, hand the full refetch to `applyResume` instead of `onReplace` alone, so the trail and cards are current before done.

**Touches.** `frontend/src/hooks/useStreamingMessage.ts`, `frontend/src/hooks/useStreamingMessage.test.tsx`

**Done when.**
- Test: when a delta resume's response lands after a live chunk, the follow-up full resume's events reach `onResumeState`, so the trail and cards are current before done.
- Test: a stall probe response carrying events calls `onResumeState`.
- Test: a fix decision present only in the resume events moves the card from "applying" to the decided status before commit.

**ADR.** ADR 0030 specifies resume-by-offset; it does not describe the race branch.

### R-streaming-2

**The bounded journal silently truncates the persisted tool trail and orphans fix proposals/decisions on long agentic turns** · tier 3

**Problem.** `maxJournalEvents` is 100 (`chat_stream_control.go:30`). When the bound is hit, `recordJournal` drops the oldest half in place and leaves no marker (`:42-48`). Every tool call journals two events, `tool` and `tool_result` (`chat_tool_exec.go:78`, `:96`, `:121`). The tool loop runs up to 6 iterations (`chat.go:42`). A read-only batch runs in groups of 4, with no per-iteration cap on the number of calls (`chat_tool_exec.go:25`, `:101`). Only a turn with more than about 50 tool calls reaches the bound, for example about 9 parallel calls in each of 6 iterations. The comment at `chat_stream_control.go:28-29` assumes that never happens, so this is an edge case.

When it happens, `persistAssistantTurn` derives `ToolCalls` and `FixProposals` from the surviving journal via `journalTurnRecord` (`chat_conversation.go:224`). `journalTurnRecord` skips a `fix_decision` whose `fix_proposal` is no longer present (`:346-350`). `BeginStream` and `ResumeStream` replay the same truncated list (`chat_resume.go:322`, `:552`, `:577`). The stored assistant message and a reconnecting client therefore lose the first half of the trail without any indication. A pending approval cannot be evicted: a turn that contains a fix tool runs fully sequentially and blocks on the decision (`chat_tool_exec.go:81-98`), so an undecided `fix_proposal` is always at the tail. The real loss is a decided proposal evicted later in the turn: its `fix_decision` is orphaned and skipped, and the user's decision drops out of the record.

**Evidence.**
- `internal/service/chat_stream_control.go:30` — `maxJournalEvents = 100`; the comment assumes 100 "covers every realistic stream" (28-29)
- `internal/service/chat_stream_control.go:42` — the oldest half is dropped in place (42-48), with no marker; `fix_proposal`/`fix_decision` are journaled types like `tool`/`tool_result` (21-26)
- `internal/service/chat_tool_exec.go:77` — one `tool` event per call (78) and one `tool_result` per call (96/121); `maxParallelTools = 4` (25) batches an unbounded call list (101); a turn with a fix tool runs sequentially (81-98); `maxToolIterations = 6` (`chat.go:42`)
- `internal/service/chat_conversation.go:224` — the stored message's `ToolCalls`/`FixProposals` come from `journalTurnRecord(events)`
- `internal/service/chat_conversation.go:346` — a `fix_decision` whose proposal is not in `byID` is skipped (346-350)
- `internal/service/chat_resume.go:322` — `BeginStream` replays the (truncated) journal; `ResumeStream` returns it as `Events` (552, 577)
- `internal/service/chat_journal_test.go:13` — `TestJournal_RecordAndBound`; `TestJournal_DroppedEventsUnreachable` at 130

**Change.** Narrow the scope to two changes. Never evict `fix_proposal` or `fix_decision` events: either keep approvals in a separate list that is never evicted, or evict only `tool`/`tool_result` pairs. When pairs are dropped, record one synthetic `{type: 'tool_result', name: 'journal_truncated', summary: 'N earlier tool calls omitted'}` marker, so the stored trail and the client show the truncation. Update the existing bound tests (`chat_journal_test.go:13-31`, `:130-155`) together with the fix.

**Touches.** `internal/service/chat_stream_control.go`, `internal/service/chat_conversation.go`, `internal/service/chat_journal_test.go`, `internal/service/chat_turn_persist_test.go`

**Done when.**
- A journal holds 150 tool pairs, plus one `fix_proposal` at index 3 and its `fix_decision` at index 149. `journalTurnRecord` returns the proposal with its decided status, and `ToolCalls` includes the truncation marker.
- Journal memory stays bounded: `len(journal) <= cap + number of approval events`.

### R-streaming-10

**Watchdog cancellations reuse STREAM_STOPPED and blame the provider for pre-provider stalls** · tier 3

**Problem.** `cancelWithReason` stores the code `""` (`chat_stream_control.go:272`). Both watchdog cancellations, provider idle and subscriber gone (`chat_resume.go:230-232`, `:247-251`), are therefore reported as `ChatErrStopped` (`chat_errors.go:232`). That is the same machine code as the user's own Stop, distinguishable only by the English copy. The idle clock is touched once at stream start (`chat.go:716`), and `beginProviderTurn` runs only immediately before `provider.Stream` (`chat.go:933-937`, `:1218-1219`). A stall anywhere before that is cancelled after 90 s (`chat_resume.go:50`) with "the AI provider stopped responding", although the provider was never called. The realistic unbounded setup stalls are:
- the budget and database reads in `prepareTurn` (`chat.go:521`);
- the conversation mutex held by another turn (`chat.go:182-184`);
- a live `Models()` listing in `modelOutputInfo` (`chat.go:112-113`, `:547`) on the Copilot, GitHub Models or local providers.

Operators chasing provider health get a false lead. The client does not branch on the code: `classifyChatFailure` has no `STREAM_STOPPED` case and maps any unknown code to `'generic'` (`frontend/src/lib/chatErrors.ts:14-23`). The cost is a misleading message and log line, not a wrong client path.

**Evidence.**
- `internal/service/chat_stream_control.go:272` — `cancelWithReason` → `cancelWithCode("", reason)`
- `internal/service/chat_errors.go:230` — `cmp.Or(cancelCode, ChatErrStopped)` at 232: watchdog cancels share the user-stop code
- `internal/service/chat_resume.go:230` — idle cancel reason "the AI provider stopped responding" (232); subscriber-gone at 247-251; `streamIdleTimeout = 90 s` (50)
- `internal/service/chat.go:716` — a single `ctl.touch()` at start; nothing between here and the first chunk touches the clock
- `internal/service/chat.go:933` — `beginProviderTurn` only right before `provider.Stream` (937); setup such as `modelOutputInfo` → `p.Models` (547, 112-113) runs under the 90 s limit
- `internal/service/chat.go:182` — the conversation mutex (182-184); `prepareTurn` at 521
- `internal/ai/testdata/providers.golden:7` — github-models, copilot (8) and local (10) list models live (`live_listing`)
- `frontend/src/lib/chatErrors.ts:14` — `classifyChatFailure`: no `STREAM_STOPPED` case; any other code is `'generic'` (14-23)

**Change.** Give the watchdog its own codes (`STREAM_IDLE_TIMEOUT`, `STREAM_CLIENT_GONE`) through `cancelWithCode`, with phase-aware copy. Record the stream's phase (setup, provider, tool, approval, apply) on `streamCtl` so the idle message names it. Touch the clock at phase boundaries (after the provider is built, after the context is built) so a setup stall is reported as one. On the client, either add the new codes to `CHAT_ERROR_CODES` and give them a kind (adding `'interrupted'` to `ChatFailureKind` only if a new branch is wanted), or make sure they keep falling through to `'generic'`.

**Touches.** `internal/service/chat_stream_control.go`, `internal/service/chat_resume.go`, `internal/service/chat_errors.go`, `internal/service/chat.go`, `frontend/src/lib/chatErrors.ts`

**Done when.**
- An idle cancel before the first provider call ends with a code other than `STREAM_STOPPED` and a message naming the setup phase.
- The new codes reach the chat bubble with phase-specific copy, either listed in `CHAT_ERROR_CODES` with their own kind or falling through to `'generic'`.
- The existing idle and subscriber watchdog tests assert the new codes.

### R-streaming-c16

**A stream that is storing its answer when the 2 s chat shutdown budget ends gets no terminal event, and its 15 s write runs past the stop budget** · tier 3

**Problem.** `ChatService.Shutdown` waits `chatShutdownBudget` (2 s) for the workers. A server-owned turn stores its answer inside `emit("done")`. That write is bounded by `persistAnswerTimeout` (15 s) on a context detached from cancellation, and before it starts there is an unbounded wait on the conversation stripe mutex. `emit("done")` claims the terminal event (`terminalSent=true`) before the write starts, and `markDone` has already set `done=true`. So when the budget ends with the write still running, the `recordTerminal` call in `forceServerRestarting` refuses that stream and emits nothing for it. `Shutdown` then returns to the fx stop sequence. The server hook (`router.Shutdown`) closes the SSE connections after the backfill, audit-backfill (up to 5 s) and scanner (up to 5 s) hooks. The worker's done goes out only after the write returns. A write that ends inside that gap still gets its done out, but one that outlasts it sends its done after the connections have closed. This is a race bounded by the 15 s write, not a certainty. If a mirror tick ran between `markDone` and the cancel, the backplane snapshot may already show `Done=true`. The impact is narrow:
- On reconnect the client resumes every stream.
- With the backplane, the 1 s mirror tick publishes the `done=true` snapshot that `markDone` set before the write, so `ResumeStream` on another replica reports done.
- The 15 s write fits inside the 28 s fx stop budget, ahead of the pool close, so the answer is normally still stored.

The leftover risk is a store write that takes longer than 2 s at the exact moment of a deploy.

**Evidence.**
- `internal/service/chat_resume.go:373` — `chatShutdownBudget = 2 * time.Second`; the comment (370-372) assumes a just-finished answer's persist takes milliseconds when nothing is stuck
- `internal/service/chat_conversation.go:197` — `persistAnswerTimeout = 15 * time.Second`
- `internal/service/chat_conversation.go:232` — `writeCtx` is `WithTimeout(WithoutCancel(ctx), persistAnswerTimeout)`, so Shutdown's cancel does not stop it
- `internal/service/chat_conversation.go:237` — `mu.Lock()` on the stripe mutex is not bounded by `writeCtx` at all
- `internal/service/chat.go:756` — `claimTerminalEvent("done")` marks `terminalSent` before `storeAnswer()` runs at 764; the event is sent at 774, after the write returns
- `internal/service/chat.go:980` — `ctl.markDone` (`done=true`) runs before `emit("done")` at 981
- `internal/app/server.go:122` — the server hook's `OnStop` closes the event streams with `router.Shutdown()` (122-126)
- `docs/adr/0024-graceful-shutdown-and-server-restarting.md:16` — decision 6: hooks run drain, backfills, audit backfill (5 s), scanner (5 s), then server; `fx.StopTimeout` is 28 s; the pool close is last
- `internal/service/chat_resume.go:279` — each mirror tick (every `resumeMirrorInterval`, 1 s, line 39) publishes a changed snapshot to the backplane (279-287)
- `frontend/src/hooks/useStreamingMessage.ts:271` — after a reconnect, `resumeAll` resumes every stream by offset (269-286)

**Change.** Make the end-of-stream write aware of shutdown:
1. Give `ChatService` a stop context that `Shutdown` cancels when its budget runs out.
2. `persistAssistantTurn` keeps `WithoutCancel` for the stream's own cancel, but also aborts on the stop context (`context.AfterFunc`).
3. `persistAssistantTurn` takes the stripe lock with a context-aware acquire (a channel semaphore or a `TryLock` loop over `convMutexes`) instead of `mu.Lock`.
4. After cancelling the stop context, `Shutdown` waits a short margin for the workers. The aborted write returns, and the worker sends the done it has already claimed while the SSE connections are still open.

A new "terminal delivered" flag on `streamCtl` and a change to the frontend's `events.ts` are unnecessary, because reconnect resume and the backplane snapshot already cover a missed terminal event. Update decision 3 of ADR 0024 to describe the stop-context abort.

**Touches.** `internal/service/chat_resume.go`, `internal/service/chat_conversation.go`, `internal/service/chat.go`, `internal/service/chat_shutdown_test.go`, `docs/adr/0024-graceful-shutdown-and-server-restarting.md`

**Done when.**
- In a test whose conversation store blocks the answer write for 5 s, `Shutdown` returns within its budget plus a small margin.
- In that test, exactly one terminal event for the stream is emitted before `Shutdown` returns, and `ResumeStream` reports the same state.
- In a test where another writer holds the stripe mutex, the answer write gives up when the stop context ends, not after 15 s.
