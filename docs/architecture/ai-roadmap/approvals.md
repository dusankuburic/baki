# AI roadmap: Approvals and AI-made changes

Items of category `approvals` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-approvals-2

**Single apply_fix applies to a stale BlockID and surfaces every apply error (incl. version conflicts) raw to the card and the model** · tier 1

**Problem.** `applyApprovedFix` resolves a fresh FlowEdit document, so a desktop save or another collaborator's cloud edit during the approval wait of up to 60 s is picked up. It then applies by the proposal's original `BlockID`, which `FindBlockByID` no longer finds after a re-parse.

Any error in the apply path takes the same route. Examples are "block … not found", a fixer decline, FlowEdit denied, and `storageif.ErrVersionConflict` from `persistCloudDoc`'s OCC. The conflict is a narrow race: the version is read with `LoadFlowHeader` immediately before `SaveFlow`, so an edit during the approval wait never conflicts. The error becomes a `fix_decision` "error" carrying the raw `err.Error()`, and it is also returned as an error. `ExecuteTool` renders that as "error: …" for the model with no guidance, although the `ToolFixApplier` contract says an error means the stream context died. The REST fix path, by contrast, maps `ErrVersionConflict` to a 409 "reload and retry".

The batch path does not handle this correctly either. It tries to re-associate targets by `fixContentKey` and reports "already-resolved" when a target is gone. Its fallback key for a `BlockID` missing from the fresh document is `{RuleID, Name: BlockLabel}`. That key can never equal `fixContentKeyOf` for an existing block, which also fills RawType and Subflow. After an external edit the batch therefore reports "already-resolved" for findings that still exist. Its per-item failure message is the raw `ferr.Error()`.

The single path has no re-association at all. The user must get the model to re-propose and re-approve, instead of the fix being re-resolved transparently on the fresh document.

**Evidence.**
- `internal/service/chat_fixes.go:319` — a fresh doc via `GetAuthorizedFlow(FlowEdit)`, then `applyFixHook(ctx, doc, prop.BlockID, …)` with the original id (325); any error becomes `chatFixOutcome{error, err.Error()}` and is returned (326-328)
- `internal/service/chat_fixes.go:104` — `ApplyFixWithApproval` emits "error" with `err.Error()` and returns the error (104-106), while the contract at 66-67 says an error means the stream context died
- `internal/ai/tools.go:243` — `ExecuteTool` returns `"error: "` + the scrubbed error for any applier error
- `internal/service/flow.go:1137` — `generateFixPatch` returns "block %q not found" when the id is stale (1137-1139); the OCC in `persistCloudDoc` reads the version right before `SaveFlow` (1507-1517)
- `internal/api/flow_fixes.go:53` — the REST path maps `ErrVersionConflict` to 409 "flow was modified concurrently; reload and retry"
- `internal/service/chat_fixes.go:483` — the batch path builds a `fixContentKey` per item (483-495) and falls back to `{RuleID, Name: p.BlockLabel}` on a miss (492-494). `fixContentKeyOf` fills RawType and Subflow (397-401), `findFinding` matches on the full key (497-520), "already-resolved" is reported at 553, and the raw `ferr.Error()` is the item message at 558-562

**Change.**
- **Re-association.** Compute the finding's content key from the proposal-time document, where the `BlockID` is valid. In `applyApprovedFix`, analyse the fresh document read-only and locate the finding first by exact (`BlockID`, `RuleID`), then by that key. Apply to the located block, and report "already-resolved" when the finding is gone.
- **Batch path.** Fix it the same way: compute each target's key from the proposal-time document instead of the `{RuleID, Name}` fallback.
- **Conflict retry (secondary).** On `errors.Is(err, storageif.ErrVersionConflict)`, re-fetch the flow, re-resolve and retry once, per item in batches.
- **Errors on both paths.** Convert apply failures into `chatFixOutcome` statuses with text the model can act on. For a conflict: "the flow changed while applying — call list_findings and try again". For a permission failure, explain it. Return a nil error unless ctx is done, matching the documented contract, and never emit the raw storage text.

**Touches.** `internal/service/chat_fixes.go`, `internal/service/chat_fixes_test.go`, `internal/service/chat_fixes_stale_test.go`, `internal/service/chat_batch_test.go`, `internal/ai/tools.go`

**Done when.**
- Test: if the document is replaced between `fix_proposal` and the approve decision (new block IDs, same content), the outcome is "applied". If the finding is gone before approval, the outcome is "already-resolved", and the `fix_decision` status is not "error"
- An apply hook that returns `ErrVersionConflict` once and then succeeds yields "applied". Two conflicts in a row yield a status "error" whose message does not contain the storage error text, a curated tool result with no "error:" prefix, and a nil error
- Batch items hit by a conflict are retried individually and reported per item; after an external edit, a batch target whose finding still exists is applied, not reported as "already-resolved"
- Neither the single nor the batch path puts raw storage error text in a card or tool result

### R-approvals-4

**Any non-done ending drops the turn's record on both sides, including an applied fix** · tier 1

**Problem.** `persistAssistantTurn` writes only when `done && errMsg == ""`. The tool loop changes the flow (`applyApprovedFix`) and emits `fix_decision` long before anything is saved. Any of these exits can follow:
- the iteration cap
- a budget re-check failure mid-loop
- a provider error on the next iteration
- the idle watchdog
- `SERVER_RESTARTING`
- the replica dying

Each leaves the flow changed with no trace in the conversation. The server stores nothing, not even from the journal. The client's `onError` builds the error message from `acc.text` and `slot.toolCalls` but not from `slot.approvals`. It appends that message in memory without the fix cards and does not persist it, and `endStream` then deletes the slot.

The flow change itself is recorded. On the desktop, `PatchFlow` pushes a "before fix" snapshot; in cloud, `SaveFlow` writes a new version and publishes `FlowChanged`. The same loss applies to the partial text of a plain stream that fails after text has streamed.

**Evidence.**
- `internal/service/chat_conversation.go:219` — `if !done || errMsg != ""` → `answerWrite{}`: nothing is stored, not even from the journal
- `internal/service/chat_fixes.go:103` — the flow is changed and the decision event emitted; nothing is saved until done
- `internal/service/chat.go:1341` — the iteration cap fails the whole turn after any applied fix
- `internal/service/chat.go:1107` — a budget re-check failure mid-loop → `st.fail` (1107-1113)
- `internal/service/chat.go:1281` — a provider error on a later iteration → `st.fail` (1281, 1295)
- `frontend/src/components/chat/hooks/stream-engine/events.ts:106` — the error message carries `acc.text` + `slot.toolCalls` (120-122) and no `fixProposals`; then `appendMessage` and `endStream` (123-125)
- `internal/service/flow.go:1083` — the desktop records a "before fix" snapshot (1083-1089); cloud `SaveFlow` writes a version and publishes `FlowChanged` (1516-1523)

**Change.**
- **Server.** `persistAssistantTurn` stores the turn whenever the journal holds a tool trail or fix records, or there is non-blank partial text. The stored message gets `finishReason` "error" and the error code, matching the shape the client uses for interrupted and error messages. Keep the existing de-duplication by `answerID` (`chat_conversation.go:262-264`) so a client-saved error message and the server write do not duplicate.
- **Client.** `onError` copies `slot.approvals` onto the error message and saves it when the server did not store the turn, the same decision `onDone` makes with `save: !acc.serverOwned && !adopted` (`events.ts:58`).
- **Loop.** Once a fix has been applied, treat the rest of the loop as best-effort, so a later failure still leaves a final record with the applied-fix trail instead of failing the whole turn.

**Touches.** `internal/service/chat_conversation.go`, `internal/service/chat.go`, `internal/service/chat_fixes.go`, `frontend/src/components/chat/hooks/stream-engine/events.ts`, `frontend/src/components/chat/hooks/stream-engine/commit.ts`

**Done when.**
- A `FakeProvider` that approves and applies a fix and then fails on the next iteration leaves a stored assistant message with `fixProposals[0].status == 'applied'` and `finishReason` "error", and a GET of the conversation afterwards returns it
- A plain stream that fails after text has streamed leaves the partial text stored with `finishReason` "error" and the error code
- Stream-engine test: the error message carries `fixProposals` from the slot and is saved when the server did not store the turn

### R-approvals-5

**Stop during the apply window persists the fix card as 'Applying…' forever; closing the thread loses it** · tier 1

**Problem.** The client sets a card to `applying` on the Approve click, before the server answers. A Stop (`stopAndCommit`) drops the stream accumulator first and then commits the message with the slot's approvals as they are. The card is therefore stored with status `applying` by the client's own conversation save. When no text has streamed yet, `stopAndCommit` commits nothing at all and the card is lost outright. The server's real `fix_decision` (`applied`, `applied-unresolved`, `error` or `cancelled`) arrives moments later, but `onFixDecision` returns early because the accumulator is gone. `abandon` drops the accumulator and cancels without any commit. `MessageBubble` remaps only `pending` to `cancelled` on interrupted messages. The reloaded thread therefore shows `FixOutcomeStrip` with the `outcome.applying` copy, "Applying…", next to a static brand dot, with no end, and the user cannot tell whether the flow was changed.

**Evidence.**
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:88` — `stopAndCommit` calls `dropAcc(streamId)` before `commitAssistantMessage`, and commits only if `acc.text` is set (88-96). `abandon` (434-441) drops the accumulator and cancels with no commit.
- `frontend/src/components/chat/hooks/stream-engine/commit.ts:90` — `fixProposals` are copied from `slot.approvals` with their current status (90-112).
- `frontend/src/components/chat/hooks/stream-engine/fixes.ts:73` — `onFixDecision` returns when there is no accumulator (73-74), so the server's outcome is dropped.
- `frontend/src/components/chat/hooks/stream-engine/fixes.ts:126` — the card is patched to `applying` on click, before the server answers.
- `frontend/src/components/chat/MessageBubble.tsx:472` — only status `pending` is remapped to `cancelled` on an interrupted message.
- `frontend/src/components/chat/ToolTrail.tsx:98` — `FixOutcomeStrip` renders `outcome.<status>` (98-105). The `applying` dot is a static `bg-brand-400` (64).
- `frontend/src/i18n/en.ts:1076` — `outcome.applying` is "Applying…". The live card's "Applying the fix…" (1166) is not what a stored message shows.
- `internal/service/chat_conversation.go:288` — `fixCardStatuses` includes `applying`. It is the clamp `journalTurnRecord` applies to journal statuses (353-356), and `persistAssistantTurn` runs it only for done streams (219-224). It is not where the `applying` card gets stored.

**Change.** Keep a stopped stream's accumulator in a "closing" map until its terminal event or its last `fix_decision` arrives, or until a short grace period passes. Patch the committed message's card from that event. On any non-streaming message, render `applying` as "outcome unknown — re-analyse" instead of the "Applying…" outcome. On stop and abandon, commit the message whenever the slot holds approvals, even if no text streamed. On the server, let a Stop received during an apply window wait for the apply to finish (the DB write cannot be cancelled mid-flight anyway). The decision event is then emitted before the stream's terminal event.

**Touches.** `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts`, `frontend/src/components/chat/hooks/stream-engine/fixes.ts`, `frontend/src/components/chat/hooks/stream-engine/commit.ts`, `frontend/src/components/chat/MessageBubble.tsx`, `frontend/src/components/chat/ToolTrail.tsx`, `internal/service/chat_fixes.go`

**Done when.**
- A stream-engine test runs approve → stop while `applying` → `fix_decision` `applied` arrives, and the committed message's card is `applied` and persisted.
- Closing a thread with an `applying` card, with or without streamed text, commits a message carrying that card.
- No non-streaming message ever renders the "Applying…" outcome copy.

### R-approvals-c9

**Desktop undo of AI-applied fixes lives only in memory, while the fix rewrites the user's own flow file in place** · tier 1

**Problem.** An approved chat `apply_fix` (or `apply_fixes`) on the desktop goes through `ApplyFix` → `PatchFlow`. `PatchFlow` overwrites the user's PAD source file on disk and records the pre-fix bytes only in the in-process snapshot ring. Cloud flows got a durable ring (migration 0045, 24 h, survives restarts), but desktop file flows deliberately stay in memory. Any app close, crash, sidecar respawn (DESK-2) or Windows hard kill (DESK-3) wipes the undo list. The AI's edit to the user's own export then has no in-app way back, and the app keeps no backup copy anywhere.

**Evidence.**
- `internal/service/flow_snapshots.go:23` — `maxSnapshotsPerFlow` bounds "the in-memory undo ring".
- `internal/service/flow_snapshots.go:41` — "the in-memory ring above is the desktop's"; cloud snapshots go to the database.
- `internal/service/flow_snapshots.go:244` — `durableSnapshots` is false whenever `doc.FilePath != ""`.
- `internal/service/flow_snapshots.go:278` — the desktop push goes to `s.snapshots`, in memory.
- `internal/service/flow.go:1083` — `PatchFlow` captures the pre-fix bytes, overwrites the source file (1086), then pushes the snapshot to memory (1089).
- `internal/service/flow.go:1193` — desktop `ApplyFix` falls through to `PatchFlow`.

**Change.** Persist the desktop ring under the config directory as `snapshots/<stable flow id of the target>/<id>.json`. Each file holds the label, `createdAt`, the target path and the source (or the folder's file map). Write it atomically before the in-place write commits. Apply the cloud ring's bounds (20 snapshots, 32 MiB per flow, 24 h TTL) plus a global byte cap with an LRU sweep at boot. These differ from today's in-memory bounds, which are 10 snapshots per flow, 64 flows and 256 MiB in total (`flow_snapshots.go:25-36`). Have `ListSourceSnapshots` load the ring lazily. Have `RestoreSourceSnapshot` refuse when the target path no longer exists, and take its own pre-restore snapshot before it overwrites the file.

**Touches.** `internal/service/flow_snapshots.go`, `internal/service/flow.go`, `internal/di/services.go`

**Done when.**
- Apply a chat fix on a desktop file and restart the backend: `/api/flow/snapshots` still lists the "before fix" snapshot, and restoring it brings back the exact pre-fix bytes.
- The persisted ring obeys the count, byte and TTL bounds and is pruned at boot.
- A snapshot whose target path was deleted is refused on restore with a clear error.

### R-approvals-c17

**An approval card stays decidable through the drain, and an approved fix is then cancelled mid-apply: partial batches, lost undo snapshots and 'not allowed to edit' errors** · tier 1

**Problem.** During `PAD_SHUTDOWN_DRAIN_DELAY` (cloud default 3 s), chat streams are not told the server is draining. A tool loop can still emit a new `fix_proposal` card, and `POST /api/chat/fix-decision` is not one of the refused routes. An approve is accepted with 200, either locally or relayed from another replica, whose command subscription stays up until `Shutdown` returns. `ChatService.Shutdown` then cancels every stream without exception. The apply runs on the stream context (through the tool's 120 s deadline), so the cancel lands at an arbitrary point in an approved mutation:

- (a) Before the edit check, `GetAuthorizedFlow` fails and the card shows its raw error as the message, for example a wrapped `context canceled`. The "not allowed to edit this flow: …" text is only the outcome detail, which the error path discards.
- (b) Between `SaveFlow` committing and `pushSnapshot`, the fix is saved, but for a cloud flow the snapshot is pushed under the same cancelled context. A snapshot that cannot be stored is logged and dropped, so the fix has no undo entry.

**Evidence.**
- `internal/api/handlers_chat.go:51` — stream creation is refused while draining.
- `internal/api/handlers_chat.go:211` — `handleFixDecision` has no draining check and delivers the decision (230).
- `docs/adr/0024-graceful-shutdown-and-server-restarting.md:11` — draining refuses only chat stream creation, event streams and WS. Decisions and running loops are untouched.
- `internal/platform/drain/drain.go:68` — `Drain` runs `Begin`, sleeps for `delay`, then calls `ChatService.Shutdown`. Nothing reaches the streams during the delay.
- `internal/service/chat_resume.go:362` — `cancelAll` cancels every active stream with `SERVER_RESTARTING`, whether or not an apply is in progress.
- `internal/service/chat.go:900` — `tctx.Ctx` is the stream ctx, and `internal/ai/tools.go:231` derives the tool deadline from it.
- `internal/service/chat_fixes.go:105` — on an apply error, the single path emits `err.Error()` as the card message. The batch path does the same at 235.
- `internal/service/chat_fixes.go:321` — "not allowed to edit this flow: …" is only the outcome `Detail`. The batch path does the same at 458.
- `internal/service/flow_cloudfix.go:247` — a single-file cloud fix runs `persistCloudDoc(ctx, …)`, then `pushSnapshot(ctx, …)` (247-251). `internal/service/flow_snapshots.go:249-250` logs and drops a snapshot that cannot be stored.
- `internal/service/flow_cloudfix.go:319` — a cloud batch cut short persists what it applied, with its snapshot, under a detached 15 s context (319-337). `partialPersistTimeout` and `persistContext` are at `internal/service/flow.go:1296-1305`.

**Change.**
- **Soft drain for approvals.** From `drain.Begin`, `requestApproval` shows no new card and returns a model-readable "the server is restarting, nothing was changed". While draining, `handleFixDecision` answers an approve with 503 `SERVER_RESTARTING` (`drain.Refuse`); a decline is still accepted. `DecideApproval`'s relay path refuses the same way on the owner.
- **Approved apply as a critical section.** Mark `ctl.applying`. `cancelAll` defers the cancel of an applying stream until the apply returns, bounded by the context `Shutdown` runs under.
- **Scope.** Limit the critical-section work to the single-fix path and the edit-permission check. A cloud batch cut short already persists what it applied, with its snapshot, under a detached 15 s context. Confirm the batch path's per-item status reporting before adding new batch logic.
- **ADR.** ADR 0024 records the decision route among the refused ones.

**Touches.** `internal/api/handlers_chat.go`, `internal/service/chat_commands.go`, `internal/service/chat_approvals.go`, `internal/service/chat_fixes.go`, `internal/service/chat_resume.go`, `internal/service/flow_cloudfix.go`, `internal/service/flow_snapshots.go`, `docs/adr/0024-graceful-shutdown-and-server-restarting.md`

**Done when.**
- An approve sent to `/api/chat/fix-decision` while `drain.Process().Draining()` is true returns 503 `SERVER_RESTARTING` with `Retry-After` and applies nothing; a decline still returns 200.
- A test calls `Shutdown` while a single approved apply is inside `SaveFlow`, and the run ends one of two ways:
  - The fix is applied, its flow snapshot is stored, and the card resolves `applied` or `applied-unresolved`.
  - Nothing is applied, and the card resolves `error` with a restart message.
- A test calls `Shutdown` during a 5-item batch after item 2:
  - Items 1-2 are reported with their real status.
  - Items 3-5 are reported as not attempted, with a restart message.
  - No event or model-visible text carries a raw `context canceled`.

### R-approvals-3

**One 120 s tool deadline covers both the human decision and the apply + re-analysis** · tier 2

**Problem.** `ExecuteTool` wraps the tool context with `fixApplyToolTimeout = FixDecisionTimeout + 60 s`. The applier uses that same context for the approval wait, the apply and the verifying analysis. A user who takes 55 s to approve leaves 65 s for the write, re-parse, re-analysis and (in cloud) persist. In a batch, up to 10 sequential apply and analysis rounds on a large flow share that window. When the deadline lands mid-apply, the analysis shards abort on `ctx.Err()`. The single-fix path then reports "applied-unresolved … re-analysis failed" for a fix that worked. The batch path marks later items "error: re-analysis failed mid-batch", and a cloud persist can fail after the patch was computed. When the deadline lands inside `applyFixHook`, `ExecuteTool` reports "tool apply_fix timed out" (`internal/ai/tools.go:237-238`) even though the write may already have landed.

**Evidence.**
- `internal/ai/tools.go:36` — `fixApplyToolTimeout = FixDecisionTimeout + 60 s`
- `internal/ai/tools.go:230` — `ExecuteTool` replaces `tctx.Ctx` with the timeout ctx for the whole handler (230-234)
- `internal/service/chat_fixes.go:85` — `requestApproval(ctx, …)` and `applyApprovedFix(ctx, …)` at 103 share that ctx
- `internal/service/chat_fixes.go:339` — verification `AnalyzeFlow(ctx, updated)`; on error, "applied-unresolved … re-analysis failed" (341-344)
- `core/analyzer/engine.go:448` — analysis shards abort when `gctx.Err() != nil` (448-451)
- `internal/service/chat_fixes.go:529` — batch: per-item "re-analysis failed mid-batch" when ctx is gone (529-534)

**Change.** Split the budget. The decision wait keeps its own timer under the stream ctx, as it already does. Apply and verify then run under a fresh timeout derived from the stream ctx, not from the tool deadline. For batches, size that timeout per item (for example 30 s + n × perItem), and keep touching the keepalive. Report a timed-out verification as "applied, verification skipped" rather than "unresolved". The decision wait is capped at 60 s (`internal/service/chat_approvals.go:126`), so apply plus verify always gets at least 60 s. The failure needs both a slow approval and an apply/verify phase longer than 60 s, such as a very large flow or a batch near 10 items. That is a real but edge-case risk, closer to tier 2 than tier 1.

**Touches.** `internal/ai/tools.go`, `internal/service/chat_fixes.go`, `internal/service/chat_approvals.go`, `internal/service/chat_fixes_keepalive_test.go`

**Done when.**
- A test with `decisionWait` close to its maximum and an analysis stub that sleeps sees the single fix report "applied", not "applied-unresolved".
- A batch of 10 with a slow analysis stub completes every item.
- The stream idle watchdog is still touched throughout (the existing keepalive tests pass).

### R-approvals-11

**Chat can only apply finding-bound auto-fixes although the service already has block-edit primitives** · tier 2

**Problem.** The only changes the assistant can make go through `apply_fix`/`apply_fixes`. Both require a finding with an `AutoFix` and refuse behaviour-changing fixers. The system prompt states that `apply_fix` is the ONLY way to modify the flow. A PAD developer who asks to change a property (timeout, selector, path), rename a label or comment, or reorder actions gets an explanation instead. `FlowService` already exposes `UpdateBlockProperties`, `RenameBlock`, `RemoveBlock`, `DuplicateBlock`, `MoveBlock`/`MoveBlockTo` and `RemoveBlocks` behind `/api/flow/block/*`, with a parse gate, snapshots and version-conflict handling. The approval envelope is generic (`tool`, `previewKind`, `items[]`).

**Evidence.**
- `internal/ai/tools.go:613` — `resolveFixTarget` requires a finding and `finding.AutoFix != ""` (613-618)
- `internal/ai/tools.go:637` — behaviour-changing fixes are refused through chat (637-642)
- `internal/ai/context.go:26` — "The ONLY way to modify the flow is the apply_fix tool"
- `internal/api/flow_blocks.go:256` — `registerBlockRoutes`: remove, duplicate, properties, move, move-to, remove-batch, rename (256-264)
- `internal/service/flow_blockedit.go:462` — `UpdateBlockProperties` applies targeted in-line replaces
- `internal/service/flow_blockedit.go:278` — `RenameBlock` rewrites same-file GOTO references; it renames only LABEL and COMMENT blocks (276-277)
- `internal/ai/tools.go:263` — the registry holds six read-only tools and two fixer-based apply tools only (263-347); `PatchForFix` is called at 645
- `core/analyzer/fix_dispatch.go:89` — `PatchForFix` only knows registered fix types (unknown `fixType` is an error, 90-93)

**Change.** The gap is arbitrary property edits, label and comment renames, and reordering. Removing a disabled block already works through `apply_fix` on its finding. The disabled-block rule registers AutoFix `remove-block` (`core/analyzer/rule_disabled_block.go:42-48`), which is not behaviour-changing (`core/analyzer/autofix.go:500`, flag 0). Derived action names cannot be renamed (`flow_blockedit.go:276-277`). Register four approval-gated tools: `set_block_properties`, `rename_block`, `remove_block` and `move_block_to`, each with RequiredScope write and PreviewKind `block-edit`. Their handlers preview the change from the scrubbed doc. On approval they call the `FlowService` methods through the `chatFixApplier` path: FlowEdit re-check, `InvalidateChatContext`, `onDocReplaced`, and a version conflict mapped to a message the model can read. Add a `BlockEditPreview` to `ApprovalCard` and update the system prompt sentence.

**Touches.** `internal/ai/tools.go`, `internal/ai/testdata/tools.golden`, `internal/service/chat_fixes.go`, `internal/service/chat_approvals.go`, `internal/ai/context.go`, `frontend/src/components/chat/approval/ApprovalCard.tsx`, `frontend/src/components/chat/approval/previews/`, `docs/adr/0026-tool-specs-and-approvals.md`, `internal/ai/tools_spec_test.go`, `internal/service/flow.go`

**Done when.**
- "Set WaitTimeout to 30 on block X" produces an approval card showing the key, old value and new value. The change is applied through `UpdateBlockProperties` only after approval.
- A scoped token without write scope is not offered the tools, and is refused if it calls them.
- A concurrent-edit conflict returns a "reload and retry" result the model can read, not a stream failure.

**ADR.** ADR 0026 (`docs/adr/0026-tool-specs-and-approvals.md:20`): "A new approval-gated tool is a `ToolSpec` plus a `requestApproval` call — no new event, endpoint or card". No such tools exist.

### R-approvals-1

**Approval countdown is a client constant decoupled from the server deadline and restarts at 60 s on resume** · tier 3

**Problem.** The server starts its `decisionWait` timer the moment `fix_proposal` is emitted, but the event carries no deadline. The client counts `FIX_DECISION_WINDOW_S` from `receivedAt`. That constant is a copy of `ai.FixDecisionTimeout` and cannot see the per-service `fixDecisionWait` override, which only tests set. `receivedAt` is stamped with `Date.now()` on first receipt, and again when a card is first seen through the resume journal in a new stream slot (`replaceStreamFixes`). An in-page reconnect keeps the slot and the stored `receivedAt`. After a page reload, though, a card the server created 40 s ago shows a fresh 60 s. The common symptom is that the server's own `timeout` `fix_decision` flips the card while the client still shows seconds left. If the user clicks in the gap before that event arrives, the server answers 404 and the card flips to `decisionFailed`: an error, not a timeout. Since the override is test-only, the runtime risk comes from editing the constant: changing the server timeout silently desynchronises the countdown from the moment the card actually expires.

**Evidence.**

- `internal/service/chat_approvals.go:61` — `eventData` carries `proposalId`, `tool`, `previewKind`, `requiredScope`, `items` and the flat fields, but no deadline (61-73)
- `internal/service/chat_approvals.go:124` — emits `fix_proposal`, then `timeout := time.NewTimer(s.decisionWait())` (124-126); `decisionWait` honours a per-service override (25-30)
- `internal/service/chat.go:340` — the `fixDecisionWait` field; it is set only in tests (`chat_approvals_test.go:234`, `chat_batch_test.go`, `chat_fixes_test.go`)
- `internal/ai/tools.go:32` — `FixDecisionTimeout = 60 s`, the number the frontend duplicates
- `internal/service/chat_commands.go:76` — the approval key TTL also uses `ai.FixDecisionTimeout` directly
- `frontend/src/lib/constants.ts:4` — `FIX_DECISION_WINDOW_S = 60`
- `frontend/src/components/chat/approval/ApprovalCard.tsx:17` — `secondsLeft` counts `DECISION_WINDOW_S` from `startedAt = approval.receivedAt ?? Date.now()` (17-19, 39-48)
- `frontend/src/stores/chat/streams.ts:69` — `replaceStreamFixes` keeps a stored `receivedAt` via the `seen` map (68) and stamps `Date.now()` only for cards not already in the slot; `setFixProposal` does the same at 106

**Change.** Tier 3. Add `expiresAt` (unix ms), derived from `decisionWait()`, to the `fix_proposal` payload and the journal entry. `chatEvent` parses it into `card.expiresAt`, and `ApprovalCard` counts down to `expiresAt`, falling back to the constant for old servers. Two further changes are optional, because the server's own timeout event already resolves the card: sending `decisionWindowS`, and rendering a 404 for a card whose `expiresAt` has passed as `timeout` instead of `error`. `chat_commands.go:76` also uses `ai.FixDecisionTimeout` directly for the approval key TTL, so derive the key TTL and the deadline from one source. Drop the duplicated `FIX_DECISION_WINDOW_S` once servers send the deadline.

**Touches.** `internal/service/chat_approvals.go`, `internal/service/chat_approvals_test.go`, `frontend/src/lib/chatEvent.ts`, `frontend/src/stores/chat/types.ts`, `frontend/src/stores/chat/streams.ts`, `frontend/src/components/chat/approval/ApprovalCard.tsx`, `frontend/src/components/chat/approval/ApprovalCard.test.tsx`, `frontend/src/components/chat/hooks/stream-engine/fixes.ts`, `frontend/src/lib/constants.ts`

**Done when.**

- The approval event-shape test asserts `expiresAt` within [now+decisionWait−1s, now+decisionWait], and, if sent, `decisionWindowS` equal to the service's `decisionWait`
- ApprovalCard test: a card with `expiresAt` 20 s ahead renders 20 regardless of `receivedAt`; a replayed proposal created 40 s ago renders at most 20 after `onResumeState`
- If the remap is adopted, a 404 on a decision for a card past its `expiresAt` renders status `timeout`, not `error`

### R-approvals-7

**Approval card shows a scrubbed op summary instead of a diff and carries no risk signal, so behaviour-changing fixes are refused in chat** · tier 3

**Problem.** `buildFixProposal` refuses any fix that `analyzer.IsBehaviourChangingFix` reports, and the applier calls `ApplyFix` with `unsafe=false`. Three fixers are registered with `FixBehaviourChanging` today: `replace-with-variable` (the fix for the hardcoded-* security findings), `mask-sensitive-variable` and `sanitize-command-vars`. Both refusals cite the same reason: the card carries no diff and no risk signal. It therefore cannot collect the explicit acknowledgement that `bakicli fix -unsafe` and the REST `unsafe` flag require.

The card is not strictly one line. `renderPatchPreview` already lists inserted and wrapped lines with `+` and replacements as old→new. What it lacks is context lines and unmasked values. Its summary comes from a patch computed on the scrubbed doc, so value-dependent ops (replace, insert) echo masked values that differ from what the real patch writes at apply time. The findings card, by contrast, already shows a real before/after diff from `PreviewFix`.

Users who ask the assistant to fix exactly these findings are sent back to the findings list. ADR 0026 designed the envelope (`previewKind`, `items[]`) to be extended, so the fix is additive.

**Evidence.**

- `internal/ai/tools.go:634` — behaviour-changing fixes are not offered through the assistant "until the card can express that" (634-642)
- `core/analyzer/autofix.go:439` — `replace-with-variable` is registered with `FixBehaviourChanging` (438-441); `mask-sensitive-variable` at 580-582; `sanitize-command-vars` at 626
- `internal/service/chat_fixes.go:294` — `applyFixHook` passes `unsafe=false`: "a chat approval is a one-line card with no diff" (294-299)
- `internal/service/flow.go:1178` — `ApplyFix` refuses behaviour-changing fixers without `unsafe`; `PreviewFix` at 1550-1603 already produces original/patched text without writing
- `internal/api/flow_fixes.go:35` — the REST path has the explicit `Unsafe` opt-in
- `internal/ai/tools.go:619` — the block is taken from the scrubbed `Doc`; `PatchForFix` runs on it at 645; `Summary = renderPatchPreview` at 663; "computed from the SCRUBBED doc, so echoed values are masked" (667-668); inserts and wraps are listed with `+` (675-696) and replace is rendered at 700
- `frontend/src/components/chat/approval/previews/FixPatchPreview.tsx:89` — renders the `approval.summary` text only (66 per batch item)
- `frontend/src/components/findings/FindingCard.tsx:242` — confirming the preview dialog calls `doApplyFix(…, true)`, which sends `unsafe=true` (`useFindingFix.ts:70-75`); there is no acknowledgement checkbox

**Change.** At proposal time, compute the real patch on `RealDoc`. Derive a bounded unified diff from it (`PreviewFix` on the member file), scrub the diff, and attach it as `diff` to each `fix_proposal` item so the card renders a diff. For behaviour-changing items:

- Add `risk: 'behaviour-changing'` with the fixer's own warning text to the item and to `FixProposalSnapshot`.
- Render a required "I understand this changes runtime behaviour" checkbox.
- Carry `acknowledgedRisk` in the decision and in the cross-replica command.
- Let the applier pass `unsafe=true` only for acknowledged items.
- Refuse server-side when the client did not acknowledge, and keep the refusal when no diff could be produced.

This checkbox is stricter than the findings card, where confirming the preview dialog alone sends `unsafe=true`. Record the rule in ADR 0026. Its §4 defines `previewKind` and `items[]`, and its Consequences paragraph (line 20) says a new approval-gated tool is "a ToolSpec plus a requestApproval call".

**Touches.** `internal/ai/tools.go`, `internal/service/chat_fixes.go`, `internal/service/chat_approvals.go`, `internal/service/chat_commands.go`, `internal/service/flow.go`, `internal/api/handlers_chat.go`, `core/models/chat.go`, `frontend/src/components/chat/approval/previews/FixPatchPreview.tsx`, `frontend/src/lib/chatEvent.ts`, `frontend/src/components/chat/hooks/stream-engine/fixes.ts`, `internal/service/chat_approvals_test.go`, `docs/adr/0026-tool-specs-and-approvals.md`

**Done when.**

- `fix_proposal` items for replace/insert ops carry a scrubbed `diff` whose `+` lines equal the lines `PreviewFix` would write (scrub-aware comparison); the `ScrubText` invariants hold for the field
- For a behaviour-changing finding, `apply_fix` emits `fix_proposal` with `risk` set and the card shows the acknowledgement control. An approval without `acknowledgedRisk` is refused server-side (declined with a message, nothing written); with it, the fix applies with `unsafe=true` and verifies
- Non-risky fixes, and old clients that send no acknowledgement, are unchanged: no checkbox, `unsafe=false`, and a behaviour-changing fix never applies

**ADR.** The extension point is specified in ADR 0026 §4 (`previewKind`, `items[]`) and its Consequences paragraph ("a ToolSpec plus a requestApproval call"); the risk signal and the diff are not implemented.

### R-approvals-8

**Hosted MCP approval requests are terminal errors instead of approval cards** · tier 3

**Problem.** When a Foundry agent run stops on an `mcp_approval_request`, `runBlockers` collects it and the provider returns `mcpApprovalError`. That error tells the user to set `require_approval` to "never" in the agent definition. The pieces for a real approval already exist:

- The approvals component (`requestApproval`) is generic over `approvalRequest`.
- `ApprovalCard` renders unknown `previewKind`s through `GenericPreview`.
- ADR 0026 anticipated a second preview kind.

Even so, the only hosted-agent approval path fails the turn, so agents must disable approvals to work at all.

**Evidence.**

- `internal/ai/responses.go:971` — `mcpApprovalError`: "this app cannot approve MCP tool calls: set require_approval to \"never\"" (971-978)
- `internal/ai/responses.go:1030` — `runBlockers.observe` collects `mcp_approval_request` as a blocker (1032-1037)
- `internal/service/chat_approvals.go:113` — `requestApproval` is generic over `approvalRequest` (tool, previewKind, requiredScope, items)
- `frontend/src/components/chat/approval/ApprovalCard.tsx:21` — only `fix-patch` is mapped; unknown kinds render `GenericPreview` (21-33)
- `docs/adr/0026-tool-specs-and-approvals.md:14` — renaming the events waits until "a second preview kind ships", so one is anticipated

**Change.** Surface `mcp_approval_request` as a chunk that the loop turns into a `requestApproval` with `previewKind` `mcp-call`, showing the server label, the tool name and the scrubbed arguments.

- On approve, send the follow-up Responses request: replay the input plus an `mcp_approval_response` item, using a stateless `store:false` replay as `ProviderItems` already do.
- On decline or timeout, answer `approve:false`.

Register the kind as a ToolSpec-like spec so the golden and the gate cover it. There is a feasibility caveat. The comment at `responses.go:971` says the app cannot answer approvals because it is stateless and has no stored response. The proposal therefore depends on the Foundry agent endpoint accepting an `mcp_approval_response` next to the replayed `mcp_approval_request` under `store:false`. Verify that against the service first.

**Touches.** `internal/ai/responses.go`, `internal/ai/provider.go`, `internal/service/chat.go`, `internal/service/chat_approvals.go`, `frontend/src/components/chat/approval/ApprovalCard.tsx`, `frontend/src/components/chat/approval/previews/`, `internal/ai/responses_foundry_test.go`, `internal/service/chat_approvals_test.go`

**Done when.**

- A stream whose agent run emits `mcp_approval_request` produces a `fix_proposal` event with `previewKind` `mcp-call` instead of an error
- Approving leads to a second Responses request whose input contains an `mcp_approval_response` with `approve=true` and the original `approval_request_id`
- Declining or timing out sends `approve=false`, and the turn ends with a readable answer, not an error

**ADR.** ADR 0026 §4 anticipates a second preview kind; it is not implemented.

### R-approvals-9

**An approval whose Redis record failed to publish is decidable only on the owner; another replica answers 404 and the card shows as failed while the owner still waits** · tier 3

**Problem.** `registerApproval` is best effort. A failed SET (Redis slow or briefly unreachable, within the op bound) is only logged, and the card is still emitted. `DecideApproval` on any replica other than the owner then finds no record and no claim and returns `ErrNoPendingFix`, which the handler renders as 404. The client turns the 404 into status `error` (`decisionFailed`), but the owner keeps waiting the full `decisionWait` and then emits a timeout. The decision POST can land on any replica, whichever SSE connection delivered the card. (`fix_proposal` is a durable event, logged and published to every replica.)

Two conditions must hold for this to happen:

- A multi-replica deployment. The repository ships one replica and no Redis.
- The SET fails while Redis is reachable again by the time the decision arrives. With Redis down, `loadApproval` errors and the request gets a retryable 503.

The resume snapshot already names the owner replica and its live flag, which is all a relay needs.

**Evidence.**

- `internal/service/chat_approvals.go:117` — `registerApproval` runs before the card is shown, "Best effort" (117-122)
- `internal/service/chat_commands.go:599` — a failed SET is `LimitedWarn` and `return false` (599-602); the comment at 69-71 says a decision from another replica then answers 404
- `internal/service/chat_commands.go:460` — `DecideApproval`: no record found and no lost claim → `ErrNoPendingFix` (460-472); a `loadApproval` error is a retryable `ErrReplicaUnreachable` (461-464)
- `internal/service/chat_commands.go:408` — `CancelStreamAs` already relays through `lookupSnapshot`'s owner and live flag (408-429)
- `internal/api/handlers_chat.go:236` — the default branch renders the decision error as 404
- `frontend/src/components/chat/hooks/stream-engine/fixes.ts:149` — any failure other than unreachable patches the card to status `error` / `decisionFailed` (149-150)
- `internal/api/events_fanout.go:79` — only chunk and heartbeat chat events are ephemeral; `fix_proposal` is logged and published to every replica
- `docs/architecture/operations.md:114` — the repository ships one replica and no Redis

**Change.** Tier 3. When the approval key is missing, let `DecideApproval` fall back to `lookupSnapshot`, the same way `CancelStreamAs` relays through the snapshot's owner and live flag. The decision then goes to the owner named by the stream's snapshot. No new event fields are needed. The owner repeats its checks as it does today: `ownerDecide` → `ResolveApproval`, including the scope check. On the client, a 404 for a card that is still pending keeps it pending with a retry action until a `fix_decision` event settles it.

**Touches.** `internal/service/chat_approvals.go`, `internal/service/chat_commands.go`, `internal/api/handlers_chat.go`, `frontend/src/components/chat/hooks/stream-engine/fixes.ts`

**Done when.**

- Two-replica test: with the approval SET made to fail, a decision posted to the other replica reaches the owner and the fix is applied
- Client test: a 404 on a pending card leaves it pending and offers a retry; a later `fix_decision` settles it
- `pad_chat_commands_total` gains an outcome for a relay resolved from the snapshot rather than the approval key

**ADR.** ADR 0026's Consequences paragraph (line 20) and ADR 0033 §4 describe the relay through the pending key only ("A failed write of the pending key is logged only").

### R-approvals-10

**A fix proposal on a non-active thread is invisible: no badge, no toast, no announcement, and the window expires** · tier 3

**Problem.** Up to three threads may stream at once (`MAX_CONCURRENT_STREAMS = 3`), but approval cards are rendered only in the active thread's footer. When a tool loop in a background thread emits `fix_proposal`:

- `onFixProposal` only stores the card.
- The thread tab shows the same pulsing "generating" dot it shows for any stream, because it subscribes to stream keys only.
- No toast is raised.
- The card's focus and sr-only announcement run only when the card is mounted.

The server times the decision out after `decisionWait`. The user finds out only later, from a "timed out" outcome strip, and must ask again.

**Evidence.**

- `frontend/src/components/chat/aitab/useStreamFooter.tsx:27` — `ApprovalCard` is rendered only when `isCurrentThreadStreaming && activeThreadId`, for the active thread's approvals (27-37)
- `frontend/src/components/chat/ChatThreadBar.tsx:156` — the tab shows only a pulsing "generating" dot for streaming threads; line 30 subscribes to stream keys only, and approvals are not read
- `frontend/src/components/chat/hooks/stream-engine/fixes.ts:56` — `onFixProposal` calls `setFixProposal` only, with no toast or announcement (56-61)
- `frontend/src/components/chat/approval/ApprovalCard.tsx:50` — focus (50-58) and the sr-only announcement (100-103) happen only when the card is mounted, which means only in the active thread
- `internal/service/chat_approvals.go:126` — a timer of `decisionWait` (126) resolves the card as `timeout` at 138-140; `fixDecisionTimeout = ai.FixDecisionTimeout` (22), 60 s at `internal/ai/tools.go:32`
- `frontend/src/stores/chat/types.ts:3` — `MAX_CONCURRENT_STREAMS = 3`: multi-thread streaming is a supported mode

**Change.** On `fix_proposal` for a thread that is not active, or when `document.hidden`, do three things. Raise a toast with a "Review" action that switches to the thread. Add a distinct "needs your decision" badge on the thread tab. Announce the pending decision through the sr-only status region. Optionally, set a `document.title` hint while a decision is pending. The countdown needs no new work: after a thread switch, the card already counts from the stamped `receivedAt` (`ApprovalCard.tsx:39`, `streams.ts:106`), so the remaining seconds are correct relative to arrival.

**Touches.** `frontend/src/components/chat/hooks/stream-engine/fixes.ts`, `frontend/src/components/chat/ChatThreadBar.tsx`, `frontend/src/components/chat/AITab.tsx`, `frontend/src/i18n/en.ts`

**Done when.**

- Test: a fix-proposal event for thread B while A is active gives B's tab a decision badge and raises a toast whose action calls `switchThread(B)`
- After switching, the card shows the remaining seconds derived from the server deadline (from `receivedAt` until [R-approvals-1](approvals.md#r-approvals-1) lands)
- The status region announces the pending decision once

**ADR.** ADR 0026 describes the card and the window; multi-thread visibility is unspecified.

### R-approvals-12

**Decision-claim takeover compares the writer's clock with the reader's, with a 4 s margin, in a fleet that only requires clocks within 30 s** · tier 3

**Problem.** A replica that loses the first-decision claim takes it over when the claim's `At` is older than `claimStale` by the reader's own clock. `At` is the winner's `time.Now().UnixMilli()`, and `claimStale` is `chatCommandWait` 2 s + 2 s = 4 s. The signed command bus accepts up to 30 s of skew, and that is all the runbook requires.

- **Reader 5 s ahead.** It takes over a live winner's claim at once and sends a second decision. The owner's buffered channel keeps first-wins, so the first decision is applied. The second caller is answered ok only if its command arrives before `requestApproval` deletes the pending entry. Otherwise it gets `replyNoPending`, a 404 that the client shows as `decisionFailed`.
- **Reader 5 s behind.** It never sees a dead winner's claim as stale within its 6 s deadline, so it answers 503. That 503 is retryable, and a retry can take over once the real age passes 4 s plus the skew.

The per-user slot caps avoid exactly this by judging leases with the server's `TIME` inside the scripts.

**Evidence.**

- `internal/service/chat_commands.go:647` — `claimStale()` = `chatCommandWait` + 2 s; `chatCommandWait` = 2 s at 123
- `internal/service/chat_commands.go:663` — `claimValue` stamps `At` with the writer's `time.Now().UnixMilli()`
- `internal/service/chat_commands.go:808` — `awaitClaim`: `claimSending && time.Since(UnixMilli(c.At)) > claimStale()`, so the takeover is judged with the reader's clock
- `internal/service/chat_commands.go:773` — the loser's deadline is `claimStale` + `chatCommandWait` (6 s)
- `internal/service/chat_approvals.go:179` — the owner's buffered send keeps the first decision (179-182); `requestApproval` deletes the pending entry once it receives a decision (116, 132-133)
- `internal/service/chat_commands.go:318` — `ownerDecide` answers `replyNoPending` when no pending entry is left (318-324)
- `internal/service/chat_commands.go:204` — `chatCluster` has a `now` field, but the claim path calls `time.Now` directly
- `docs/architecture/operations.md:118` — "every replica needs the same secrets and clocks within 30 s" (also ADR 0033 §7)
- `internal/redisx/slots.go:22` — slot leases are written and judged on the server's clock (`TIME` inside the scripts), so a skewed replica cannot change their length (22-24)

**Change.** Judge the claim's age on Redis. Either write `At` from `TIME` inside a Lua script, or keep the `sending` state in a companion key with `PX = claimStale` so that its expiry is the takeover signal. The loser then polls for the key's absence instead of comparing clocks. For the acceptance test: `chatCluster` already has a `now` field (`chat_commands.go:204`), but `claimValue` and `awaitClaim` call `time.Now` directly (663, 808). Switch them to `cl.now`, or land the server-clock fix first.

**Touches.** `internal/service/chat_commands.go`, `internal/service/chat_commands_test.go`

**Done when.**

- A test with two `chatCluster` values whose `now()` differ by +10 s and -10 s shows two things: a live winner's claim is not taken over before `claimStale` on the server clock, and a dead winner's claim is taken over within the loser's deadline
- No wall-clock comparison across replicas remains in the claim path

**ADR.** ADR 0033 §4 describes the claim states without saying whose clock judges staleness.
