# AI roadmap: Streaming

Items of category `streaming` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

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
