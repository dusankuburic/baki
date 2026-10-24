# AI roadmap: Frontend chat

Items of category `frontend` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-frontend-4

**Usage meter and tool trail poll the store on timers, and the meter re-announces the token count through role=status on every tick** · tier 1

**Problem.** `ChatUsageMeter` and `LiveToolTrail` each read `useChatStore.getState()` on a `setInterval` (250 ms and 400 ms) while the active thread streams, instead of subscribing with a selector. There is one meter and one trail, so two timers run per visible chat. Tool status lags up to 400 ms, and the trail polls even when no tool is active.

Worse, the meter renders the live count inside a div with `role="status"`, an implicit polite live region. The count appears both in its `aria-label` and as "{n} tokens so far" text, so every tick whose number changed is queued for announcement. Meanwhile the streaming bubble itself is `aria-hidden`: a screen-reader user hears a stream of token counts and nothing of the answer until commit. `ThinkingIndicator` already does this right, with a static status label and an `aria-hidden` elapsed counter, so the meter is an oversight.

**Evidence.**
- `frontend/src/components/chat/ChatUsageMeter.tsx:13` — `TOKEN_REFRESH_MS = 250`. `setInterval(read, TOKEN_REFRESH_MS)` reads `streams[activeThreadId].tokens` into state (20-29).
- `frontend/src/components/chat/ChatUsageMeter.tsx:33` — the wrapper div has `role="status"` with an `aria-label` carrying the count (33-36). The inner text `tokens.streamingSoFar` carries the live count (41-43).
- `frontend/src/components/chat/StreamingBubble.tsx:12` — the streaming bubble is wrapped in `aria-hidden="true"`.
- `frontend/src/components/chat/ThinkingIndicator.tsx:36` — a static `role="status"` label, with the elapsed label `aria-hidden` (36-42). This is the pattern the meter should follow.
- `frontend/src/components/chat/LiveToolTrail.tsx:27` — `setInterval(read, TRAIL_REFRESH_MS)` copies `toolStatus`/`toolCalls` into local state (27-40). `TRAIL_REFRESH_MS = 400` is set at `lib/constants.ts:6`.
- `frontend/src/components/chat/ChatInput.tsx:531` — the one meter. The one trail is at `frontend/src/components/chat/aitab/useStreamFooter.tsx:26`.
- `frontend/src/i18n/en.ts:815` — `streamingSoFar` is "{{formatted}} tokens so far", and `streamingAria` (816) is "Generating answer — {{count}} output tokens so far".

**Change.** Replace both intervals with Zustand selectors on `streams[activeThreadId]`: `tokens` for the meter, `toolStatus` and `toolCalls` for the trail. Updates then become event-driven. Take the changing number out of the live region:

- One static sr-only status ("Generating answer") that changes only on start and finish.
- The live count rendered `aria-hidden` (or `aria-live="off"`).
- The final totals announced once on commit, plus at most a throttled (~10 s) progress announcement.

`LiveToolTrail.test.tsx` already exists, so the trail test is an edit; only `ChatUsageMeter.test.tsx` is new.

**Touches.** `frontend/src/components/chat/ChatUsageMeter.tsx`, `frontend/src/components/chat/LiveToolTrail.tsx`, `frontend/src/lib/constants.ts`, `frontend/src/i18n/en.ts`, `frontend/src/components/chat/ChatUsageMeter.test.tsx` (new), `frontend/src/components/chat/LiveToolTrail.test.tsx`

**Done when.**
- RTL test: across several store updates during streaming, the `textContent` of every `role=status` element under the composer is unchanged, and the live token figure is `aria-hidden` or outside any live region.
- There is no `setInterval` in `ChatUsageMeter` or `LiveToolTrail`. Tests assert that the meter and trail re-render on a store update without advancing fake timers.
- A jest-axe run on `ChatInput` while streaming reports no live-region violations.

### R-frontend-5

**aria-live on the virtualised message list: scrolling a long thread re-announces old messages as additions** · tier 1

**Problem.** The Virtuoso scroller carries `role="log" aria-live="polite" aria-relevant="additions text"`. Virtuoso mounts and unmounts item nodes as the user scrolls, keeping only the viewport plus 400 px in the DOM. Scrolling back through a long conversation therefore inserts earlier messages into a live region, and assistive tech announces them as new content. Conversely, the only genuinely new content, the streamed answer, is `aria-hidden` until commit. At commit, `appendMessage` inserts the whole assistant message as one list item, and it is read in full. An sr-only `role="status"` announcer (`StatusRegion`) already exists in `AITab`. Today it carries only the model-notice sentences, and it could carry turn announcements instead.

**Evidence.**
- `frontend/src/components/chat/ChatMessageList.tsx:31` — the scroller has `role="log" aria-live="polite" aria-relevant="additions text"` on the scrolling element (31-33).
- `frontend/src/components/chat/ChatMessageList.tsx:148` — Virtuoso with `data={messages}` and `increaseViewportBy` top/bottom 400 (:157). Items outside that window are not in the DOM.
- `frontend/src/components/chat/hooks/stream-engine/commit.ts:114` — `appendMessage` inserts the whole assistant message as one list item on commit.
- `frontend/src/components/chat/StreamingBubble.tsx:12` — the streaming text is `aria-hidden`, so nothing is announced while it streams.
- `frontend/src/components/chat/aitab/StatusRegion.tsx:6` — the sr-only `role=status` announcer already exists (6-11).
- `frontend/src/components/chat/AITab.tsx:278` — `StatusRegion` carries only the model-notice sentences (278-280).
- `frontend/src/components/chat/LiveToolTrail.tsx:49` — tool activity is announced by the trail's own `role="status"` (49-50).

**Change.** Keep `role="log"` on the virtualised scroller and drop `aria-live` and `aria-relevant`. Route turn announcements through the existing sr-only `StatusRegion`. On commit, announce the answer, or a capped prefix of it, rather than only "answer complete". Announce nothing on scroll. Tool activity is announced today by `LiveToolTrail`'s own `role="status"`, not by `StatusRegion`, so routing it there is new work. Optionally, offer a "read answer as it streams" preference that un-hides the streaming bubble with `aria-live="polite"` and sentence-level batching.

**Touches.** `frontend/src/components/chat/ChatMessageList.tsx`, `frontend/src/components/chat/AITab.tsx`, `frontend/src/components/chat/aitab/StatusRegion.tsx`, `frontend/src/i18n/en.ts`

**Done when.**
- The scroller element has no `aria-live` attribute.
- A committed assistant message produces exactly one announcement in the `AITab` status region.
- Test: changing the Virtuoso viewport (mounting earlier items) changes no live-region text.

### R-frontend-6

**Tools are off by default, so the assistant answers PAD questions from a 4000-token summary it cannot verify** · tier 1

**Problem.** `createThread` builds a thread without a `useTools` value, and `buildChatRequest` sends `useTools: thread.useTools ?? false`. The server runs the grounding tool loop only when `req.UseTools` is set or the model is a Foundry agent. Without tools, the model sees the cached analysis report (nil when no analysis has run), the flow overview, the selected block, any raw source files the user picked, and RAG guidelines. All of it sits inside a context budget that defaults to 4000 tokens; this is the per-provider `contextTokenBudget` setting, which users can change. None of it lets the model verify blocks outside the budget.

The built-in templates and the PAD-specific system prompt invite exactly the questions that need `search_flow`, `get_subflow_blocks` and `get_variable_lineage`. Yet the toggle is a dropdown menu item labelled toolsOn/toolsOff behind the header menu. The only code path that turns tools on for a purpose is `fixWithAI.stageFindingFix`.

**Evidence.**
- `frontend/src/components/chat/hooks/useChatRequestBuilder.ts:62` — `useTools: thread.useTools ?? false`.
- `frontend/src/stores/chat/threads.ts:35` — `createThread` builds the thread without `useTools` (35-46).
- `frontend/src/components/chat/ChatHeader.tsx:146` — the toggle is a dropdown menu item labelled toolsOn/toolsOff (146-167).
- `internal/service/chat.go:898` — `if req.UseTools || ts.model.agent()` gates the tool loop. Without native tools it falls back to the marker loop (919-923).
- `internal/service/chat.go:680` — the report is only the cached `CurrentReport`, nil when no analysis has run (680-685). RAG guidelines join the user turn (599-617).
- `internal/ai/context.go:65` — user-selected raw source files are added to the context, bounded by the same budget (65-80).
- `internal/service/chat_context.go:338` — `tokenBudget := 4000` is the default of the per-provider `ContextTokenBudget` setting (337-345). `fixWithAI.ts:28` is the only caller that sets `useTools: true`.
- `frontend/src/stores/chat/persistence.ts:146` — a stored `useTools` of false is read back as undefined.

**Change.** Default `useTools` to true for providers whose `ModelDetail.supportsTools` is true, and to the marker loop otherwise. Defaulting to true is safe for every provider, because `chat.go:919-923` already falls back to the marker loop. Keep the per-thread override persisted: store `useTools: false` explicitly (`persistence.ts:146` maps false to undefined today), or apply the settings default only inside `createThread`. Add an AI setting for the default. Templates and suggested prompts that need lookups turn tools on the way `fixWithAI` does. Show a one-line composer hint when tools are off and the thread has a flow loaded.

**Touches.** `frontend/src/components/chat/hooks/useChatRequestBuilder.ts`, `frontend/src/stores/chat/threads.ts`, `frontend/src/stores/chat/persistence.ts`, `frontend/src/components/chat/AITab.tsx`, `frontend/src/components/chat/PromptTemplates.tsx`, `frontend/src/components/settings/AIBehaviorPanel.tsx`, `frontend/src/i18n/en.ts`

**Done when.**
- A new thread on a tool-capable model sends `useTools: true`, and a thread whose stored `useTools` is false keeps it.
- Selecting a template that needs lookups on a tools-off thread enables tools before sending.
- `useChatRequestBuilder` tests cover the default and the override, and an e2e test asserts that the header state matches the request.

### R-frontend-7

**Context chip is labelled with the graph's selected block, not the thread's context block** · tier 1

**Problem.** `AITab` passes the thread's `contextBlockId` to the context bar. But `SelectionContextBar` supplies `blockName`/`blockType` from `useSelectedBlock()`, which reads `flowStore.selectedBlockId`, the block currently selected in the graph. `ChatContextBar` renders `blockName || contextBlockId` in the context chip. Selecting a different block in the graph relabels the chip with that block's name, while `buildChatRequest` still sends `thread.contextBlockId`. The user is told the model is grounded on block B when it is grounded on block A.

The same bug has two more symptoms:

- With nothing selected in the graph, the chip shows the raw `contextBlockId` UUID. This is the normal state right after `stageFindingFix`, which sets `contextBlockId` without selecting the block.
- `SelectionPromptTemplates` labels the templates with the graph selection's name, while `hasBlock` comes from `contextBlockId`.

**Evidence.**
- `frontend/src/components/chat/AITab.tsx:215` — `contextBlockId = activeThread?.contextBlockId` is passed to `SelectionContextBar` at :379.
- `frontend/src/components/chat/aitab/SelectionContextBar.tsx:8` — `blockName`/`blockType` come from `useSelectedBlock()`, the graph selection (8-14).
- `frontend/src/components/chat/hooks/useSelectedBlock.ts:18` — reads `flowStore.selectedBlockId`, which is unrelated to the thread.
- `frontend/src/components/chat/ChatContextBar.tsx:68` — the chip text is `blockName || contextBlockId`.
- `frontend/src/components/chat/hooks/useChatRequestBuilder.ts:56` — the request sends `thread.contextBlockId`.
- `frontend/src/lib/fixWithAI.ts:22` — `stageFindingFix` sets `contextBlockId` (27) and never selects the block in the graph.
- `frontend/src/components/chat/aitab/SelectionPromptTemplates.tsx:7` — the template block name comes from the graph selection (7-9), while `hasBlock` comes from `contextBlockId` (`AITab.tsx:445-448`).

**Change.** Resolve the chip label from `contextBlockId` via the document's block lookup (`buildBlockLookup`). Use the graph selection only for the "focus selected block" button. Show a "block no longer in flow" state when the lookup misses. Resolve the `SelectionPromptTemplates` block name from `contextBlockId` the same way.

**Touches.** `frontend/src/components/chat/aitab/SelectionContextBar.tsx`, `frontend/src/components/chat/aitab/SelectionPromptTemplates.tsx`, `frontend/src/components/chat/ChatContextBar.tsx`

**Done when.**
- Unit test: with `contextBlockId` = A and `flowStore.selectedBlockId` = B, the chip shows A's name and type.
- Unit test: with a `contextBlockId` not in the document, the chip shows the missing-block state, not a UUID.

### R-frontend-8

**Post-fix document refresh in cloud mode depends on the presence socket** · tier 1

**Problem.** After a chat-applied fix, `refreshAfterFix` always re-analyses the flow. It reloads the flow document only when `curDoc.filePath` is set, which means desktop only. Cloud flows rely on the `FlowChanged` → presence → `useFlowChangeSync` path, and that effect returns early unless the presence status is `'connected'`. `useFlowChangeSync` is the only path that reloads a cloud document after a server-side change. Presence connects for every non-Tauri session that has a document, and the hub broadcasts `flow.changed` to every client in the room, the actor included. So only a disconnected, reconnecting or blocked socket triggers the bug. In that window a chat-applied cloud fix updates the findings report with new block IDs, but the flow store keeps the pre-fix document. "Jump to block" and block lookups from the new report then miss, and the graph shows the unfixed flow.

**Evidence.**
- `frontend/src/components/chat/hooks/stream-engine/fixes.ts:44` — `loadFlowFromPath` runs only when `curDoc.filePath` is set (44-51); `analyzeFlowById` always runs (38-43).
- `frontend/src/hooks/useFlowChangeSync.ts:16` — `if (!flowId || status !== 'connected') return`, so there is no reload without presence. A reload sets `libraryVersion` (38-40), then calls `applyRemoteDocumentUpdate` (42).
- `frontend/src/stores/flowStore.ts:136` — `applyRemoteDocumentUpdate` (136-152) swaps the document in place. It is not the only writer: `setDocument` is at :113, and `SourceEditor` and `blockEdit` also call it. But `useFlowChangeSync` is the only caller that reloads a cloud document after a server-side change.
- `frontend/src/App.tsx:100` — presence connects for every non-Tauri session with a document (100-104).
- `internal/websocket/hub.go:658` — `NotifyFlowChanged` broadcasts `flow.changed` to every client in the room, including the actor (658-666).

**Change.** In `refreshAfterFix`, when the thread's flow is a library flow (no `filePath`), reload it through `libraryApi.get` + `libraryApi.getContent` regardless of presence status. Set `libraryVersion` from the fetched metadata along with the document, as `useFlowChangeSync.ts:38-40` does, and call `applyRemoteDocumentUpdate`. Alternatively, have the applied `fix_decision` carry the new library version so the client fetches it directly.

**Touches.** `frontend/src/components/chat/hooks/stream-engine/fixes.ts`, `frontend/src/hooks/useFlowChangeSync.ts`, `internal/service/chat_fixes.go`

**Done when.**
- A unit test shows that an applied `fix_decision` for a cloud document with presence status `'disconnected'` triggers a document reload, and the store then holds the post-fix document.
- Desktop behaviour is unchanged (`loadFlowFromPath` is still used).

### R-frontend-12

**Error bubbles are re-sent to the model as assistant turns whenever the client sends history** · tier 1

**Problem.** A failed turn is stored as an assistant message whose content is the partial text plus a markdown error line, with `finishReason` `error`. `conversationSync.persistable` filters these out for saves, but `buildChatRequest` does not share the predicate. The client sends messages when `saveConversationHistory` is off, or with `includeHistory` on resend and on the `SERVER_RESTARTING` auto-resend. In those cases every error bubble goes to the server. The server uses non-empty `Messages` as-is, and `buildScopedMessages` drops only unknown roles and empty content. Resend removes just the last error bubble, so earlier failed turns still ride along, and the model sees its own prior "answer" as an error banner.

**Evidence.**
- `frontend/src/components/chat/hooks/stream-engine/events.ts:106` — `displayContent = acc.text + '\n\n---\n' + errorLine` is appended as role `assistant` with `finishReason` `error` (106-113).
- `frontend/src/components/chat/hooks/useChatRequestBuilder.ts:44` — `messages.map(...)` with no `finishReason` filter (44-53). `sendHistory` (38) is `includeHistory || saveConversationHistory === false`.
- `frontend/src/lib/conversationSync.ts:19` — `persistable()`: "a failed turn's error bubble stays local". The predicate is not shared with the request builder, and it is duplicated at `conversationSync.ts:50` and `frontend/src/components/chat/hooks/useChatThreads.ts:91` (compact).
- `frontend/src/components/chat/hooks/useAIChat.ts:189` — resend removes only the last assistant message and the last user message (185).
- `internal/service/chat.go:861` — `serverOwnsHistory := len(req.Messages) == 0`, so non-empty `Messages` are used as-is.
- `internal/service/chat_context.go:379` — `buildScopedMessages` keeps every user/assistant message and drops only unknown roles.
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:367` — the auto-resend after `SERVER_RESTARTING` sends with `includeHistory: true`.
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:94` — a stopped turn is committed with `finishReason` `interrupted`.

**Change.** Share the persistable predicate (`finishReason !== 'error'`). Export it from `conversationSync` as one helper and use it at all four call sites:

- `buildChatRequest`
- `persistable()` (`conversationSync.ts:19-25`)
- the conflict reload (`conversationSync.ts:50`)
- compact (`useChatThreads.ts:91`)

Error bubbles stay UI-only. `buildChatRequest` also drops empty `interrupted` messages; non-empty ones hold real partial answers and stay. Consider sending `finishReason` to the server so it can apply the same rule to legacy clients.

**Touches.** `frontend/src/components/chat/hooks/useChatRequestBuilder.ts`, `frontend/src/lib/conversationSync.ts`, `frontend/src/components/chat/hooks/useChatThreads.ts`, `frontend/src/components/chat/hooks/useChatRequestBuilder.test.ts`

**Done when.**
- A `buildChatRequest` test with `saveConversationHistory=false` and an error-finish assistant message in the thread shows `req.messages` excluding it.
- A resend after two failed turns sends no message whose content contains the error line.
- `useChatRequestBuilder.test.ts` pins the shared predicate.

### R-frontend-13

**Only one queued follow-up per thread; a second silently replaces the first** · tier 1

**Problem.** `queueMessage` overwrites `queuedByThread[threadId]`, and when `canQueue` is set, `ChatInput.handleSend` calls `onQueue` and then `resetComposer`. A user who types two follow-ups while a long reply streams loses the first, with no notice beyond the single-text banner changing. `ChatStreamProvider`'s drain takes one message per thread. With tool loops that run for minutes, this is a routine way to lose typed text.

**Evidence.**
- `frontend/src/stores/chat/streams.ts:87` — `queueMessage` overwrites the entry: `{...state.queuedByThread, [threadId]: msg}`.
- `frontend/src/components/chat/ChatInput.tsx:202` — the `canQueue` path calls `onQueue`, then `resetComposer` (202-207).
- `frontend/src/components/chat/ChatInput.tsx:472` — the banner renders only the single `queued.text` (472-477).
- `frontend/src/components/chat/ChatStreamProvider.tsx:31` — the drain iterates `queuedByThread` and takes one message per thread (31-43). It already runs on every `streams` change (46-51).
- `frontend/src/stores/chatStore.test.ts:407` — the overwrite is pinned: queue "first", then "second", and `takeQueuedMessage` returns "second" (407-409).

**Change.** Make `queuedByThread` a FIFO array (cap 5) and have `takeQueuedMessage` shift the head. The drain already runs on each `streams` change, so it sends the next message after each `endStream` with no further change. The banner shows the count, with a cancel per item. Queueing beyond the cap is refused with a toast. Replace the assertion at `chatStore.test.ts:407-409` that pins the overwrite, rather than adding a test alongside it. A smaller acceptable fix is to refuse queueing (or append to the queued text) while a message is already queued, so nothing is overwritten.

**Touches.** `frontend/src/stores/chat/types.ts`, `frontend/src/stores/chat/streams.ts`, `frontend/src/components/chat/ChatStreamProvider.tsx`, `frontend/src/components/chat/ChatInput.tsx`, `frontend/src/components/chat/hooks/useAIChat.ts`, `frontend/src/stores/chatStore.test.ts`

**Done when.**
- Store test: two `queueMessage` calls keep both messages in order, and `takeQueuedMessage` returns them FIFO.
- Provider drain test: after each `endStream`, exactly one queued message is sent, until the queue is empty.
- ChatInput test: the banner reads "2 queued", and cancelling the first leaves the second.

### R-frontend-14

**Closing or evicting a thread orphans its server-side conversation** · tier 1

**Problem.** `closeThread` posts only a cancel for a live stream and drops the thread from local state. Neither close nor eviction issues `/api/chat/clear`: `clearThreadConversation` is called only from the explicit Clear actions. Conversations are purged on the server only with their flow ("never on a timer"). Every closed thread therefore leaves a conversation row, with its full message content, that no client can reach again. On busy flows this accumulates indefinitely, and a user who closed a thread reasonably expects its content gone.

The `MAX_THREADS` eviction in `createThread` picks the first thread, of any flow, that is not new, active or streaming, and drops it from memory, but this is not reliably an orphaning. `createThread` persists only the new thread's flow, so a victim from another flow stays in that flow's localStorage list. It comes back after a page reload, or when that flow is hydrated with no threads in memory. It is lost only once that flow's threads are next persisted.

**Evidence.**
- `frontend/src/stores/chat/threads.ts:129` — `closeThread` only calls `cancelStream` (130-133) and removes local state, with no `clearConversation`.
- `frontend/src/stores/chat/threads.ts:56` — the eviction victim is the first thread not new, active or streaming, across all flows (56-67). There is no server delete.
- `frontend/src/stores/chat/threads.ts:88` — `createThread` persists only the new thread's flow. `hydrateThreads` restores a flow's stored threads when none are in memory (92-105).
- `frontend/src/stores/chat/persistence.ts:88` — `persistFlowThreads` writes one flow's threads to localStorage (88-115).
- `frontend/src/components/chat/hooks/useAIChat.ts:204` — `clearThreadConversation` is called only from the explicit Clear action here and at `useChatThreads.ts:63`.
- `internal/storage/database/lifecycle_flows.go:24` — conversations: `noPurge` "user content: it goes with its flow, never on a timer".

**Change.** When history is enabled, `closeThread` calls `chatApi.clearConversation(flowId, threadId, baseVersion)` best-effort, ignoring 404/409 and logging once. `conversationSync.clearThreadConversation` already handles `baseVersion` and 409. Eviction prefers the oldest thread of the same flow and asks the user before evicting a victim that has messages. Before any server clear on eviction, eviction first persists the victim's flow (or skips cross-flow victims). Otherwise clear-on-evict deletes content for a thread that would otherwise reappear. A client-side clear cannot cover threads lost when localStorage is wiped. Covering those needs an optional server sweep for conversation scopes with no thread reference, which in turn needs the server to know thread references.

**Touches.** `frontend/src/stores/chat/threads.ts`, `frontend/src/components/chat/hooks/useChatThreads.ts`, `frontend/src/stores/chatStore.test.ts`

**Done when.**
- Store test: closing a thread whose history is loaded issues `POST /api/chat/clear` with that thread's key and `baseVersion`.
- Store test: creating the 51st thread never evicts a thread of another flow while one of the same flow qualifies, and the evicted thread's conversation is cleared.

### R-frontend-c7

**After the sidecar auto-restarts, the frontend keeps the dead backend's port and token: the crash banner closes and every request, chat included, fails until a manual reload** · tier 1

**Problem.** When the backend dies, the Tauri shell respawns it automatically. The new process listens on a new ephemeral port and mints a new per-process session secret, and the shell emits `backend-ready` with the new config. The frontend's only listener for backend events is `BackendCrashBanner`, which on `'ready'` just hides itself. The API client's `configCache` (`apiUrl` + token) is reset only on token-refresh paths. Requests to the old port fail as `NETWORK` errors (a fetch `TypeError`), not 401, so the refresh-path invalidation never runs. Every fetch keeps targeting the old port with the old token. So do the `/api/events` SSE reconnect loop (which carries `chat:event`) and the chat stream and resume calls. The app looks healthy but nothing works. The only manual recovery paths are the banner's Restart button and the View > Reload menu item. The shell also clears only the child on `Terminated`, so `state.config` keeps the dead port and token until the new process reports its config.

**Evidence.**
- `src-tauri/src/lib.rs:277` — a non-manual `Terminated` respawns through `spawn_sidecar_attempt` after a 2 s delay (273-288; `BACKEND_RESTART_DELAY_SECS` at :13).
- `src-tauri/src/lib.rs:238` — the new config is stored and `backend-ready` is emitted with the new port and token (237-241).
- `src-tauri/src/lib.rs:265` — `Terminated` clears only `state.child`; `state.config` keeps the dead port.
- `src-tauri/src/lib.rs:82` — `get_backend_config` answers "Backend not ready" only when `state.config` is empty (82-85).
- `src-tauri/src/lib.rs:366` — the View > Reload menu item.
- `internal/config/config.go:504` — local `Port: 0` is an OS-assigned ephemeral port, so every respawn gets a new port.
- `internal/config/loader.go:626` — `ResolveRuntimeSecrets` mints a per-process secret when none is configured, so every respawn gets a new token.
- `frontend/src/api/client.ts:64` — `getBackendConfig` returns `configCache` forever once set. `invalidateConfigCache` runs only on the token-refresh paths (127, 139).
- `frontend/src/components/layout/BackendCrashBanner.tsx:20` — on `'ready'` the banner only clears its phase (20-23). Its Restart button reloads the window (36-40).

**Change.**
- Treat a `'ready'` that follows `'terminated'` or `'error'` as a new backend. In the platform layer (`TauriAdapter`), call `invalidateConfigCache()`, reset `lastEventId` and restart the events stream. Alternatively, reuse the Restart button's `window.location.reload()` once drafts are saved.
- Move every chat stream still marked in flight to the interrupted state with a "backend restarted" code, so nothing tries to resume against the old process.
- In `lib.rs`, clear `state.config` on `Terminated`, so `get_backend_config` answers "Backend not ready" until the new process has reported its config.

**Touches.** `frontend/src/platform/adapters/TauriAdapter.ts`, `frontend/src/components/layout/BackendCrashBanner.tsx`, `frontend/src/api/client.ts`, `src-tauri/src/lib.rs`

**Done when.**
- A unit test shows that after `'terminated'` then `'ready'` with a new port, the next `request()` and the events stream use the new `apiUrl` and token.
- Killing the sidecar process from Task Manager and waiting for the auto-respawn lets the next chat message stream successfully without any manual reload.
- A chat stream that was running when the backend died ends in a visible interrupted state instead of retrying the old port.

### R-frontend-c26

**Closing a thread, the 50-thread eviction, or a new browser orphans the stored conversation for good** · tier 1

**Problem.** Threads exist only on the client. The thread index (ids, titles) lives in browser `localStorage` per user and flow. The server knows only `(flow_id, scope = thread uuid)` rows that can be reached by key, and it has no list route. Closing a thread drops it from `localStorage` and memory but never deletes the stored row, even though the confirm dialog says the user won't be able to reopen the thread or its messages. The `MAX_THREADS = 50` cap evicts a thread the same way. The cap counts every thread in memory across all flows, so the victim can belong to another flow. Only the creating flow's index is rewritten, so that victim stays in its own flow's `localStorage` list until that list is next rewritten. Signing in from another browser, or clearing site data, loses the whole index. Those rows can no longer be reopened, and no operation short of deleting the flow or its owner reaches them. They are never purged (`noPurge`). They remain in the flow owner's export with only a UUID scope and no title, because titles are kept only in the browser.

**Evidence.**
- `frontend/src/stores/chat/threads.ts:129` — `closeThread` removes the thread from state and `localStorage` only. The only server call is cancelling a live stream (129-166).
- `frontend/src/stores/chat/threads.ts:56` — the `MAX_THREADS` eviction drops the victim locally only (56-78). The victim is the first thread in memory, from any flow, that is not the new, active or a streaming thread (57). Only the creating flow's index is rewritten (88).
- `frontend/src/stores/chat/types.ts:5` — `MAX_THREADS = 50`; `conversationKey(thread)` = `thread.id` (7-9), so each thread is its own server row.
- `frontend/src/stores/chat/persistence.ts:13` — the thread index (ids, titles) is stored in `localStorage` under `baki:chatThreads:v2:<user>:<flow>` (13-17, 88-115).
- `frontend/src/components/chat/hooks/useChatThreads.ts:44` — `handleCloseThread` only calls `closeThread` (44-49), whereas `handleClearContext` calls `clearThreadConversation` (58-83).
- `frontend/src/components/chat/hooks/useAIChat.ts:225` — close is `engine.abandon` + `handleCloseThread`, with no server clear.
- `frontend/src/i18n/en.ts:893` — the confirm copy says the thread and its messages cannot be reopened.
- `internal/storage/database/lifecycle_flows.go:24` — `conversations` is exported and has no purge step (`noPurge`: "it goes with its flow, never on a timer").
- `internal/storage/interfaces/lifecycle.go:55` — an exported conversation carries only `flowId`, `scope` and `messages` (55-59).
- `internal/storage/database/postgres_flows.go:656` — `DeleteFlow` deletes the flow's conversations.
- `internal/api/handlers_chat.go:61` — stream resolves the flow with `ChatUse`, which needs only viewer level (`internal/service/authz_can.go:92`).
- `internal/api/handlers_chat.go:474` — `/api/chat/clear` requires the editor role.

**Change.**
- **(a)** When the user closes a thread that has stored history, delete it on the server through `clearThreadConversation` (the versioned `ClearConversationIf`) before dropping it locally. Reword the confirm copy to say the messages are deleted. On a 409, keep the thread open and show the existing changed-elsewhere toast.
- **(b)** Treat the `MAX_THREADS` victim the same way, or refuse to evict a thread with stored history without asking. Since the victim can belong to another flow, rewrite that flow's index too.
- **(c)** Make the index server-side. Add `conversations.created_by`, stamped by the server when the thread's first turn is stored, and a route that lists the caller's threads so any browser can rebuild the index.
- **Viewer gap.** Viewers can create threads, because stream needs only `ChatUse`, but `/api/chat/clear` requires the editor role. So (a) as written fails for viewers, and so does today's Clear context. The fix has to let a `ChatUse` caller clear their own thread, for example once threads carry an owner (see [R-security-3](security.md#r-security-3)).

**Touches.** `frontend/src/stores/chat/threads.ts`, `frontend/src/components/chat/hooks/useChatThreads.ts`, `frontend/src/components/chat/hooks/useAIChat.ts`, `frontend/src/stores/chat/persistence.ts`, `frontend/src/i18n/en.ts`, `internal/api/handlers_chat.go`, `internal/service/chat_conversation_version.go`, `internal/storage/database/postgres_conversations_version.go`

**Done when.**
- Closing a thread with stored messages sends `/api/chat/clear` with `baseVersion`. Afterwards `POST /api/chat/get` for that key returns `[]` and the row is gone.
- A 409 on close leaves the thread open and shows the changed-elsewhere warning.
- Creating a 51st thread either deletes the evicted thread's stored row or prompts instead of evicting it.

### R-frontend-9

**Frontend hard-codes which fix types need variable/property metadata** · tier 2

**Problem.** `useFindingFix.fixArgs` forwards `finding.metadata.variable` only for `init-variable` / `mask-sensitive-variable`, and `metadata.property` only for `replace-with-variable` / `parameterize-sql`. This mirrors the server's `FixUsesVariable` / `FixUsesProperty` flags by hand. The apply-fix HTTP handler takes variable and property only from the request body and passes them straight to `ApplyFix`. The service turns an empty patch into a 400 "fixer declined … missing the metadata this fix needs". Registering a new fix with `FixUses*` flags would silently break the findings-card Apply path, while the chat path (which reads `finding.Metadata` on the server) keeps working. Nothing is broken today. The frontend sets match the registry exactly (`core/analyzer/autofix.go:394-396`, `439-441`, `534-536`, `580-582`), so this is a drift hazard, not a live defect.

**Evidence.**
- `frontend/src/components/findings/hooks/useFindingFix.ts:17` — the literal `VARIABLE_FIXES` / `PROPERTY_FIXES` sets; `fixArgs` gates on them (17-25)
- `core/analyzer/fix_dispatch.go:28` — `FixUsesVariable` / `FixUsesProperty` flags are declared per fix (28-31)
- `core/analyzer/autofix.go:394` — `init-variable` is registered with `FixUsesVariable` (394-396); `mask-sensitive-variable` at 580-582
- `internal/api/flow_fixes.go:33` — Variable and Property come from the request body only (33-34) and are passed straight through (51)
- `internal/service/flow.go:1162` — an empty patch gives `ErrInvalidInput` "fixer declined … missing the metadata this fix needs" (1162-1164)
- `internal/ai/tools.go:643` — the chat path reads `finding.Metadata["variable"/"property"]` on the server (643-645)

**Change.** Make it a frontend-only change. `fixArgs` always forwards `metadata.variable` and `metadata.property`, and the `VARIABLE_FIXES` / `PROPERTY_FIXES` sets are removed. Builders ignore arguments they do not read, and `TestFixFlags_DeclareWhatTheBuilderReads` (`core/analyzer/fix_dispatch_test.go:84-122`) keeps that true. The original proposal had two server-side parts: looking the finding up in the cached report, and exposing each fix's flags in `GET /api/analysis/rules`. Neither is needed.

**Touches.** `frontend/src/components/findings/hooks/useFindingFix.ts`, `frontend/src/components/findings/FindingCard.test.tsx`

**Done when.**
- A test pins that `fixArgs` forwards both metadata values for every fix type, with no frontend allow-list.
- `TestFixFlags_DeclareWhatTheBuilderReads` still passes.

### R-frontend-10

**Failed AI-behaviour saves roll back silently and leave the chat store pointing at a provider the settings do not name** · tier 2

**Problem.** When a persist fails, `settingsStore.applyAndPersist` restores the previous document with no user-visible signal. The persist queue only calls `logger.warn`. Every `updateAI` call in `AIBehaviorPanel` is fire-and-forget. A rejected save (400 from validation, 5xx, offline) snaps the control back with no message. The active-assistant control and `ProvidersPanel.onActivated` also call `useChatStore.setProvider` and never revert it. `selectedProvider` is write-only state, though, so the chat does not follow it. The whole settings document is posted after a 1 s debounce. The client names every section, so concurrent tabs overwrite the whole document, not one section. Other tabs re-sync only after the fact, through `settings:changed` (`frontend/src/hooks/useAppEvents.ts:93-94`). One small edge case remains: a turn sent during the 1 s optimistic window can go to the unsaved provider and be pinned on it (`frontend/src/components/chat/hooks/useProviderSetup.ts:219-224`).

**Evidence.**
- `frontend/src/stores/settingsStore.ts:251` — `applyAndPersist`: catch, then `set({settings: prev})` with no user-visible signal (251-257); 239-242 is `logger.warn` only
- `frontend/src/components/settings/AIBehaviorPanel.tsx:80` — `void updateAI({activeProvider: v})`, then `useChatStore.getState().setProvider(v)` unconditionally (80-81)
- `frontend/src/components/settings/providers/ProvidersPanel.tsx:81` — `setProvider(id)`, then `void updateAI({activeProvider: id})` (81-82)
- `frontend/src/stores/settingsStore.ts:230` — a 1 s debounce posts the whole document through `settingsApi.updateSettings(settings)` (230-246)

**Change.** Make `applyAndPersist` rethrow or return the failure (`settingsStore.ts:251-257`) so callers can show the server's field error. Show it as a toast, then re-sync from GET. Post only the changed section (`{ai: ...}`); the route already keeps the sections a body leaves out (`internal/api/handlers_system.go:84`, `105-107`). Do not revert the chat store. Delete the write-only `selectedProvider`/`setProvider` and their call sites instead: `AIBehaviorPanel.tsx:81`, `ProvidersPanel.tsx:81`, `frontend/src/hooks/useSettingsPersistence.ts:21`, and `frontend/src/components/settings/providers/LegacyProvidersPanel.tsx:48`, `54`, `70`. Add a `settingsStore` test for the failed-persist path. The toast with re-sync and the section-only posts are tier-3 polish.

**Touches.** `frontend/src/stores/settingsStore.ts`, `frontend/src/components/settings/AIBehaviorPanel.tsx`, `frontend/src/components/settings/providers/ProvidersPanel.tsx`, `frontend/src/components/settings/providers/LegacyProvidersPanel.tsx`, `frontend/src/hooks/useSettingsPersistence.ts`, `frontend/src/stores/chat/`, `frontend/src/api/settings.ts`

**Done when.**
- `AIBehaviorPanel` test: when `updateSettings` rejects, the control returns to the stored `activeProvider` and an error is shown.
- `settingsStore` test: a rejected persist emits a visible notification, and the next GET re-syncs the document.
- Network assertion: changing `activeProvider` posts a body whose top-level keys are exactly `{ai}` (and version).

### R-frontend-15

**A subflow cannot be the chat context; only a block can** · tier 2

**Problem.** `ChatRequest` has `ContextBlockID` as its only context selector. `ChatMessage` persists a `ContextSubflowID` that nothing on the request side can set. `SelectedBlock` and `SelectedSubflow` are both derived from `req.ContextBlockID`, so the Subflow Outline is injected only when a block is selected. PAD developers organise work by subflow, and the graph views select subflow nodes. Yet "explain this subflow" requires picking an arbitrary block inside it. That also pulls the block's detail, siblings and findings into the budget. The `ChatContextBar` chip is block-only. The desktop has a partial path. In Tauri, the `ChatContextBar` file dropdown lists per-subflow source files labelled by `subflowName` (`ChatContextBar.tsx:46-54`, `useChatConversations.ts:99-124`). Selecting one injects that subflow's raw source (`chat_context.go:361-368`), on the desktop only. That is not the structured outline or the subflow's findings, and selecting sources suppresses the Subflow Outline (`context.go:144`). The gap is a structured subflow selector in both modes. Cloud has no subflow selector at all.

**Evidence.**
- `core/models/chat.go:89` — `ChatRequest.ContextBlockID` is the only context selector (83-121)
- `core/models/chat.go:56` — `ChatMessage.ContextSubflowID` exists and is persisted
- `internal/service/chat_context.go:357` — `SelectedBlock` and `SelectedSubflow` are both derived from `req.ContextBlockID` (357-360)
- `internal/ai/context.go:145` — the Subflow Outline comes only from `req.SelectedSubflow` (144-148)
- `frontend/src/components/chat/ChatContextBar.tsx:64` — the context chip is block-only (64-87)

**Change.** Add `contextSubflowId` to `ChatRequest`, mirroring `ChatMessage`, and include it in the context cache key. When it is set without a block, render the Subflow Outline plus "Known Issues in this subflow". Expose "Ask about this subflow" from the graph and subflow views, and add a subflow chip in `ChatContextBar`. Also update the stale comment at `internal/service/chat_context.go:356`: the history key is now `ConversationID`, not `ContextBlockID`. Tier 2.

**Touches.** `core/models/chat.go`, `internal/service/chat_context.go`, `internal/ai/context.go`, `frontend/src/stores/chat/types.ts`, `frontend/src/components/chat/hooks/useChatRequestBuilder.ts`, `frontend/src/components/chat/ChatContextBar.tsx`, `frontend/src/types/chat.ts`

**Done when.**
- A request with `contextSubflowId` and no `contextBlockId` renders `## Subflow Outline` and the subflow's findings in the context preview.
- Two turns that differ only in `contextSubflowId` do not share a cached context entry.
- The persisted `ChatMessage.contextSubflowId` equals the request's value.

### R-frontend-16

**Slash commands insert literal text and have no link to tools or PAD flows** · tier 3

**Problem.** `/explain`, `/fix` and `/test` are "insert" commands: they write their literal id plus a space into the textarea (`frontend/src/components/chat/SlashCommandAutocomplete.tsx:16-22`, `frontend/src/components/chat/ChatInput.tsx:385`). Nothing on the server interprets them, and the i18n descriptions talk about "this code" and "unit tests" (`frontend/src/i18n/en.ts:979-981`). `/fix` does not turn on the per-thread tools toggle, and it does not steer the model toward `list_findings`/`propose_fix`. `useTools` comes only from the thread toggle (`useChatRequestBuilder.ts:62`). The only code that turns tools on for a fix is `fixWithAI.stageFindingFix` (`frontend/src/lib/fixWithAI.ts:28`). A user who types `/fix` with tools off gets a text answer and no approval card. The exception is a Foundry agent model: the server always runs the tool loop for one (`req.UseTools || ts.model.agent()`, `internal/service/chat.go:898`).

**Evidence.**
- `frontend/src/components/chat/SlashCommandAutocomplete.tsx:16` — five client-only commands; explain, fix and test are kind `insert` (16-22)
- `frontend/src/components/chat/ChatInput.tsx:385` — the insert kind writes `cmd.id + ' '` into the textarea
- `frontend/src/i18n/en.ts:979` — "Explain how this code works in detail", "Find and fix bugs in this code", "Generate unit tests for this code" (979-981)
- `frontend/src/components/chat/hooks/useChatRequestBuilder.ts:62` — `useTools` comes only from `thread.useTools`
- `frontend/src/lib/fixWithAI.ts:28` — only `stageFindingFix` sets `useTools: true` (with `buildFindingFixPrompt`)
- `internal/service/chat.go:898` — a Foundry agent always runs the tool loop

**Change.** Tier 3. Fix the misleading copy now, since that is the cheap part. The command descriptions in `en.ts` and the other locales should talk about PAD flows and blocks, not code or unit tests; for example, `/test` walks through run scenarios and edge cases of the flow. Giving the commands semantics in the request builder is optional polish:
- `/fix` would set `useTools` for the turn and expand to a PAD-specific prompt. It should reuse the `fixWithAI` prompt shape (`buildFindingFixPrompt`) rather than add a second fix prompt.
- `/explain` and `/test` would expand to flow-oriented templates.

`/clear` and `/help` stay as they are. This complements [R-frontend-6](frontend.md#r-frontend-6) (tools default).

**Touches.** `frontend/src/components/chat/ChatInput.tsx`, `frontend/src/components/chat/SlashCommandAutocomplete.tsx`, `frontend/src/components/chat/hooks/useChatRequestBuilder.ts`, `frontend/src/i18n/en.ts`, `frontend/src/components/chat/ChatInput.test.tsx`

**Done when.**
- The command descriptions in `en.ts` and the other locales mention flows and blocks, not code or unit tests.
- The i18n coverage tests pass for the new keys.
- If the per-turn override is built: a message that starts with `/fix` produces a `ChatRequest` with `useTools=true` and the expanded prompt, whatever the thread toggle says.

### R-frontend-17

**Closing a streaming thread sends the cancel twice** · tier 3

**Problem.** `useAIChat.handleCloseThread` calls `engine.abandon`, which posts `cancelStream` for the slot's stream id, and then calls the store's `closeThread`, which posts `chatApi.cancelStream` again for the same id (`useAIChat.ts:227-228`, `lifecycle.ts:434-441`, `threads.ts:130-133`). The server treats the cancel as idempotent. However, each POST runs its own `REPLICA_UNREACHABLE` retry loop: up to 3 attempts, 2-10 s apart (`frontend/src/api/chat.ts:54-68`). On a relay failure the traffic doubles, and the split hides which layer owns cancellation. The two paths also handle failure differently. The engine's cancel offers the stop-retry toast (`useChatStreamEngine.ts:23`), while `closeThread` swallows its error with `.catch(() => {})`.

**Evidence.**
- `frontend/src/components/chat/hooks/useAIChat.ts:227` — `engine.abandon(threadId)` then `threadActions.handleCloseThread(threadId)`
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:434` — `abandon` calls `bumpGen`, then `cancelStream(slot.streamId)` (434-441)
- `frontend/src/stores/chat/threads.ts:130` — `closeThread` posts `chatApi.cancelStream(slot.streamId).catch(() => {})` again (130-133)
- `frontend/src/api/chat.ts:54` — `cancelWithRetry`: `CANCEL_RETRIES = 2` on `REPLICA_UNREACHABLE`, 2-10 s apart (54-68)
- `frontend/src/components/chat/hooks/useChatStreamEngine.ts:23` — the engine's cancel failure offers the `chat:errors.stopRetry` action

**Change.** Make the engine the only owner of cancellation, and keep the stop-retry toast on the surviving path. The minimal fix deletes the `chatApi.cancelStream` call from `threads.ts:130-133`, so `closeThread` only removes state. Keep `engine.abandon`: it bumps the generation synchronously (`bumpGen`) before the store mutation, which stops an in-flight `run()` from adopting the stream. The slot watcher in `useChatStreamEngine`, which cancels a stream whose slot vanished, stays as the fallback. Removing `abandon` instead would require the watcher's `bumpGen` to run before any `await` in `send()` could observe the closed thread.

**Touches.** `frontend/src/stores/chat/threads.ts`, `frontend/src/components/chat/hooks/useAIChat.ts`, `frontend/src/components/chat/hooks/useAIChat.test.tsx`

**Done when.**
- A `useAIChat` test shows that closing a streaming thread issues exactly one `POST /api/chat/cancel`.
- A failed cancel on close still offers the stop-retry toast.

### R-frontend-18

**Dead /api/chat/begin path kept in the client, its tests and the e2e stub; the server still mounts it** · tier 3

**Problem.** The web client always calls `registerStream(sid, false)` (`frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:210`, `231`, `403`). Because of this, the `if (!begin) return true` guard in `useStreamingMessage` (`frontend/src/hooks/useStreamingMessage.ts:398`) makes the following unreachable:
- the `chatApi.beginStream` branch (400-406);
- `BeginStreamResult` (`frontend/src/api/chat.ts:37-49`);
- the e2e `/api/chat/begin` stub (`frontend/e2e/chat-stream.spec.ts:85`).

This dead code still has unit tests and mocks (`useStreamingMessage.test.tsx:44`, `438`, `456`; `AITab.test.tsx:61`; `useAIChat.test.tsx:53`). The finished-before-subscribe case it was written for is now covered by `STREAM_ID_IN_USE` adoption and the stall probe.

The dead code is in the frontend only. On the server, `/begin` (`internal/api/handlers_chat.go:556`, handler at 111-137) is still the start gate for streams created without a `clientStreamId`. Such streams begin emitting only once `BeginStream` runs (`internal/service/chat.go:729-732`, `internal/service/chat_resume.go:317-320`). The `ClientStreamID` comment that describes this legacy two-POST handshake (`core/models/chat.go:102-106`) is accurate as long as that path exists.

**Evidence.**
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:210` — `registerStream(sid, false)`; also :231 and :403
- `frontend/src/hooks/useStreamingMessage.ts:398` — `if (!begin) return true`; the `chatApi.beginStream` branch below it (400-406) is unreachable
- `frontend/src/api/chat.ts:73` — `beginStream` endpoint; `BeginStreamResult` at 37-49; mocked in `useStreamingMessage.test.tsx:44,438,456`, `AITab.test.tsx:61` and `useAIChat.test.tsx:53`
- `frontend/e2e/chat-stream.spec.ts:85` — `'/api/chat/begin': {status: 'ok'}` stubbed
- `internal/api/handlers_chat.go:556` — `rb.Post("/begin", h.handleBeginStream)` still mounted; handler at 111-137
- `internal/service/chat.go:729` — only a client-supplied id unblocks emission at create; "legacy leaves it to BeginStream" (729-732); `chat_resume.go:317-320` is that gate
- `core/models/chat.go:102` — the `ClientStreamID` comment describes the legacy two-POST handshake (102-106), which is still live

**Change.** Remove the frontend path now: the `begin` parameter and branch, `chatApi.beginStream`, `BeginStreamResult`, the e2e stub, and their tests and mocks. Removing `/begin` on the server means retiring the legacy handshake first. There are two options:
- require `clientStreamId`, a breaking `/api/v1` change that needs an ADR and a swagger update;
- auto-begin streams whose ids the server generates.

Without one of these, streams started by legacy API callers never emit. Deprecating the route through the route-policy goldens alone is not enough. Keep `BeginStream`, or an internal equivalent, as the service-test helper. Update the `ClientStreamID` comment only when the legacy path goes.

**Touches.** `frontend/src/hooks/useStreamingMessage.ts`, `frontend/src/api/chat.ts`, `frontend/src/hooks/useStreamingMessage.test.tsx`, `frontend/src/components/chat/AITab.test.tsx`, `frontend/src/components/chat/hooks/useAIChat.test.tsx`, `frontend/e2e/chat-stream.spec.ts`; when the server side follows: `internal/api/handlers_chat.go`, `internal/api/handlers_chat_test.go`, `internal/service/chat.go`, `internal/service/chat_resume.go`, `core/models/chat.go`, `internal/api/testdata/route_policy.golden`, `internal/api/testdata/route_policy_declared.golden`, `docs/swagger.yaml`, `docs/swagger.json`, `docs/docs.go`, and the service tests that call `BeginStream` (`chat_test.go`, `chat_journal_test.go`, `chat_errors_test.go` and others)

**Done when.**
- grep finds no `beginStream` or `BeginStreamResult` under `frontend/src`.
- Once the legacy handshake is retired and the server side removed, the route policy goldens no longer list `/api/chat/begin`.

### R-frontend-19

**Thread auto-title is computed twice with different truncation** · tier 3

**Problem.** One derived field has two divergent rules:
- `appendMessage` sets the thread title from the first user message at 40 characters, replacing newlines after slicing (`frontend/src/stores/chat/messages.ts:26-27`).
- The engine's send then overwrites it at 50 characters, collapsing newlines and trimming before slicing (`frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:140-143`).

On every engine send the 50-character rule wins, and it is the one persisted. The 40-character rule survives only for `appendMessage` callers outside send, and for whitespace-only text. Only `chatStore.test.ts:120-126` pins the 40-character rule.

**Evidence.**
- `frontend/src/stores/chat/messages.ts:27` — `title = message.content.slice(0, 40).replace(/\n/g, ' ')`
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:143` — `updateThread` title = `text.replace(/\n+/g, ' ').trim().slice(0, 50)`, only when `text.trim()` is non-empty (142)
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:193` — the later `updateThread(threadId, {inFlight: …})` call that persists the thread
- `frontend/src/stores/chatStore.test.ts:120` — "truncates title to 40 chars" (120-126)

**Change.** Add one `autoTitle(text)` helper in `stores/chat` and use it in `appendMessage`; the engine stops touching the title. The store's title is still persisted by the later `updateThread` call that records `inFlight` (`lifecycle.ts:193`). Pick one rule for length, newline handling and trimming, and update `chatStore.test.ts:120-126` to match it.

**Touches.** `frontend/src/stores/chat/messages.ts`, `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts`, `frontend/src/stores/chatStore.test.ts`

**Done when.**
- A store test shows that a 60-character first user message with newlines yields one title of the chosen length, the same whether it is appended via the store or sent through the engine.

### R-frontend-21

**Thread bar is not keyboard-operable: rename is double-click only, close button is invisible on focus, and a button is nested inside role=tab** · tier 3

**Problem.**
- **Arrow keys only.** `ThreadTab`'s `handleTabKeyDown` handles only ArrowLeft and ArrowRight (`frontend/src/components/chat/ChatThreadBar.tsx:42-43`).
- **Rename.** Renaming starts only from `onDoubleClick` (`handleDoubleClick` at 121, wired at 150).
- **Close button.** On the active tab the close button is visible (`opacity-60`) and reachable from the keyboard, so the active thread can be closed without a mouse. On inactive tabs the X button stays at `opacity-0` until the group is hovered and has no focus-visible rule. A keyboard user can tab to it, since it is a native button inside the tab, but cannot see it. No close button has a focus ring (197-203).
- **Nesting.** A focusable button nested inside a `div role="tab"` (140) is an ARIA authoring violation that axe flags.

**Evidence.**
- `frontend/src/components/chat/ChatThreadBar.tsx:42` — `handleTabKeyDown` handles only ArrowRight/ArrowLeft (42-43)
- `frontend/src/components/chat/ChatThreadBar.tsx:121` — rename starts only from `handleDoubleClick` (wired at :150)
- `frontend/src/components/chat/ChatThreadBar.tsx:197` — close button classes: `opacity-60` on the active tab, `opacity-0 group-hover:opacity-60` on inactive tabs, no focus-visible rule (197-203)
- `frontend/src/components/chat/ChatThreadBar.tsx:140` — the `div role="tab"` contains the close `<button>`
- `frontend/package.json` — no axe package
- `frontend/src/components/chat/AITab.tsx:160` — the existing close-confirm flow (160-173)

**Change.**
- Add F2 or Enter to rename and Delete to close on the focused tab. Delete goes through the confirm flow `AITab` already has (`AITab.tsx:160-173`).
- Give the close button a `focus-visible:opacity-100` rule and a focus ring, or move it out of the tab with `aria-describedby`, following the WAI-ARIA tabs pattern.
- Announce the rename mode.
- For the structural check, assert directly that no focusable element is nested inside `[role=tab]` (or that the close button sits outside the tab), unless jest-axe is added deliberately as a new dev dependency.

**Touches.** `frontend/src/components/chat/ChatThreadBar.tsx`, `frontend/src/components/chat/ChatThreadBar.test.tsx`, `frontend/src/i18n/en.ts`

**Done when.**
- A keyboard-only test shows: focus a tab, press F2, type a name, press Enter, and `onRename` is called; press Delete and `onClose` is called.
- The close button is visible when it gets keyboard focus.
- A structural test finds no focusable element nested in `[role=tab]`, or jest-axe, once added as a dev dependency, reports no nested-interactive violation on `ChatThreadBar`.

### R-frontend-22

**A failed chat turn shows the user no identifier an operator can search for** · tier 3

**Problem.** A failed chat turn shows the user no identifier an operator can search for:
- **The error event** carries only message, code and shared (`internal/service/chat_errors.go:170-179`).
- **The client's `onError`** receives the stream id, but the `ChatMessage` it builds keeps only `errorCode` and `errorShared`, and `errorCode` only for provider error codes (`stream-engine/events.ts:77`, `107-116`, `114`).
- **`MessageBubble`** shows actions by error code, with no id and no copy-to-clipboard (`MessageBubble.tsx:365`, Retry at 506).
- **Start-request failures** carry `ApiError.requestId` (`frontend/src/api/client.ts:12`). The start-failure bubble (`appendErrorBubble`, `stream-engine/lifecycle.ts:152-162`) does not keep it, and no `.tsx` under `frontend/src` renders it; the only hit is a test mock in `useAIChat.test.tsx`.

A user who reports "my chat failed" cannot give support the stream id or request id, so the operator has to search logs by user and time.

**Evidence.**
- `internal/service/chat_errors.go:170` — `errorEventData`: message, code and shared only (170-179)
- `frontend/src/components/chat/hooks/stream-engine/events.ts:77` — `onError` receives `streamId`; the `ChatMessage` built at 107-116 keeps `errorCode` (provider codes only, :114) and `errorShared`, not the id
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:152` — `appendErrorBubble` for start-request failures keeps no request id (152-162)
- `frontend/src/components/chat/MessageBubble.tsx:365` — error rendering keyed on `finishReason`/`errorCode`; Retry at :506; no id or copy
- `frontend/src/api/client.ts:12` — `ApiError.requestId` exists; a grep of `*.tsx` under `frontend/src` finds `requestId` only in `useAIChat.test.tsx`
- `internal/service/chat_errors.go:185` — `classifyFailure`, and `setFailure` at `internal/service/chat_stream_control.go:289`: the worker's terminal failure paths

**Change.**
- **Client.** Keep the stream id on the error message, and also the start request's `X-Request-ID`, returned on the create response. This includes the start-failure bubble built in `lifecycle.ts:152-162`.
- **UI.** Render a small "Reference: <streamId>" line with copy-to-clipboard in the error bubble, and in the budget and server-restarting toasts.
- **Error event.** Carry `requestId` in the error event's data from the worker context, so a reloaded client gets it too.
- **Server logs.** Log the stream id on the worker's terminal failure paths (`classifyFailure`, `setFailure`), or log the create request's `request_id` together with the `clientStreamId` when the stream is accepted. Without that, the reference shown to the user matches nothing in the logs.

**Touches.** `internal/service/chat_errors.go`, `internal/service/chat.go`, `internal/service/chat_stream_control.go`, `frontend/src/components/chat/hooks/stream-engine/events.ts`, `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts`, `frontend/src/components/chat/MessageBubble.tsx`, `frontend/src/lib/chatEvent.ts`, `frontend/src/i18n/en.ts`

**Done when.**
- An error event renders a reference line containing the stream id, and clicking it copies the id.
- `MessageBubble.test` covers the reference line for a `PROVIDER_UNAVAILABLE` error.
- The error event data includes `requestId`, and the resume snapshot carries it.
- The worker logs the stream id on a terminal failure, or the create logs `request_id` with the `clientStreamId`.

### R-frontend-25

**Suggested prompts are static settings lists, not derived from the analysis** · tier 3

**Problem.** `GetSuggestedPrompts` picks one of four lists from `settings.AI.Prompts`, keyed only on `hasBlock`/`hasFindings` (`internal/service/chat.go:1640-1661`), and the handler accepts only those two booleans (`internal/api/handlers_chat.go:535-542`). The lists are admin-configurable settings, not hardcoded text; "static" here means they do not depend on the analysis. The cached report and the selected block's findings are available on the server. The client also already holds the report: `AITab.tsx:217-221` reads `useAnalysisStore` but computes only `hasFindings` for the context block. As a result, the prompt strip cannot suggest "fix the 3 hardcoded credentials" or name the block's actual findings.

**Evidence.**
- `internal/service/chat.go:1640` — four configured lists chosen by `hasBlock`/`hasFindings` (1640-1661)
- `internal/api/handlers_chat.go:535` — the request carries only `HasBlock`/`HasFindings` (535-542)
- `frontend/src/components/chat/AITab.tsx:217` — the analysis report is already in `useAnalysisStore`; only `hasFindings` is derived from it (217-221)

**Change.** Derive suggestions from the analysis, such as the top rule by count and the selected block's findings with their fix types, and append them after the configured lists. The cheaper path does this on the client, in `useSuggestedPrompts`, from the report `AITab` already holds, and avoids a server-side report read on every welcome screen. The alternative is server-side: accept `flowId`/`contextBlockId` on the endpoint and read the cached report in the service, scrubbing the derived suggestions.

**Touches.** `internal/service/chat.go`, `internal/api/handlers_chat.go`, `frontend/src/components/chat/aitab/useSuggestedPrompts.ts`

**Done when.**
- A test shows that, for a report with 3 hardcoded-credential findings, the suggestions include a prompt naming that rule and the count. The test targets `useSuggestedPrompts`, or the service on the server path.

### R-frontend-27

**Model reasoning is never surfaced: users see only the thinking indicator and heartbeats on thinking models** · tier 3

**Problem.** Reasoning never reaches the client:
- Claude `thinking_delta` events are recorded for the replay but never emitted (`internal/ai/stream.go:414-416`).
- OpenAI-compatible `reasoning_content` and `<think>` blocks are emitted as `Chunk.Reasoning` (`stream.go:680-689`), but `Chunk.Reasoning` is documented as "never displayed, stored or re-sent" (`internal/ai/provider.go:141-142`).
- The chat service forwards only `chunk.Text`, in both the plain path and the tool-loop path (`internal/service/chat.go:985-989`, `1263`).

Users also never see heartbeats. The server's `heartbeat` event, sent after 20 s of silence, is a keepalive for the client's stall probe (`internal/service/chat_stream_control.go:171`, `186-190`), and the client drops it (`frontend/src/hooks/useStreamingMessage.ts:341-342`).

Thinking models have a reasoning output floor and a multi-minute limit on waiting for output. During that wait the user sees only the generic `ThinkingIndicator`, with an elapsed-time counter after 3 s (`ThinkingIndicator.tsx:36-43`, `SHOW_ELAPSED_AFTER_S` at :5). Beyond elapsed time there is no signal of what the model is thinking about or how much of its reasoning budget it has used. These are the turns a user is most likely to stop as "hung".

Hiding reasoning is a deliberate contract (`provider.go:139-142`), so this change reverses a design decision rather than fixing a bug.

**Evidence.**
- `internal/ai/stream.go:414` — `thinking_delta`: "Recorded for the replay, never emitted" (414-416)
- `internal/ai/stream.go:680` — `reasoning_content` emitted as `Chunk{Reasoning}` (680-684); the think splitter likewise (686-689)
- `internal/ai/provider.go:141` — `Chunk.Reasoning`: "never displayed, stored or re-sent" (141-142)
- `internal/service/chat.go:985` — plain path: "keepalives and split-off reasoning carry no answer text", only `Text` is forwarded (985-989); tool-loop comment at :1263
- `internal/service/chat_stream_control.go:186` — `heartbeatStream` is a stall-probe keepalive (186-190), 20 s default at :171; `frontend/src/hooks/useStreamingMessage.ts:341-342` drops it
- `frontend/src/components/chat/ThinkingIndicator.tsx:36` — the client shows the static "thinking" label plus elapsed time after 3 s (36-43)

**Change.** Add a `reasoning` SSE event, controlled by a user setting that is on by default. It carries a bounded summary: either the Claude thinking deltas folded to the first N characters per block, or a "thinking…" status with elapsed time and estimated reasoning tokens. Any reasoning text goes through the same scrubber as answer text. It never enters the journal, the resume snapshot or the stored conversation. Render it in the thinking indicator or the tool-trail row, and reuse the Notice row for providers that only expose `SilentStep`. On the Responses path, reasoning summaries must first be requested before there is anything to show.

**Touches.** `internal/ai/stream.go`, `internal/service/chat.go`, `internal/service/chat_stream_control.go`, `frontend/src/components/chat/ThinkingIndicator.tsx`, `frontend/src/components/chat/LiveToolTrail.tsx`, `internal/service/chat_hosted_idle_test.go`

**Done when.**
- A Claude stream with three `thinking_delta` events before the first text emits at least one `reasoning` event to a subscribed client, and none of it reaches the stored conversation.
- The resume snapshot and the journal contain no reasoning text.
- With the user setting off, the stream emits only heartbeats during thinking.

### R-frontend-c24

**Rate-limit refusals on AI routes show a generic error with no wait time, and the user's message stays in the thread** · tier 3

**Problem.** The limiter answers 429 with code `RATE_LIMITED` and gives the wait only in the `Retry-After` header. The client never reads that header. It reads only `details.retryAfterSeconds`, which the limiter does not set. `classifyChatFailure` maps `RATE_LIMITED` to `'generic'`, so the send path appends the error bubble "rate limit exceeded". It does not restore the draft and show a toast the way it does for `CHAT_CAPACITY_REACHED`, and it shows no wait or countdown. The bubble's Retry button (`MessageBubble.tsx:506-517`, wired through `AITab.tsx:259` to `handleResend`) resends at once. With the default rates the wait is about 1 s for the chat, general and per-user buckets (`ratelimit.go:95-98` with `config.go:416`, `:422`, `:436`). A human resend therefore usually succeeds, unless others behind the same address keep the bucket drained. The gap is a generic message with no stated wait, and a draft that is not restored.

**Evidence.**
- `internal/api/middleware/ratelimit.go:93` — `refuse()` sets only the `Retry-After` header and calls `render.Error("rate limit exceeded", 429)` (93-101); the wait is `max(1, ceil(1/rate))` (95-98)
- `internal/api/render/render.go:74` — 429 maps to code `RATE_LIMITED`
- `internal/config/config.go:416` — default rates: general 1 rps (416), chat 3 rps (422), per-user 10 rps (436)
- `frontend/src/lib/chatErrors.ts:14` — `classifyChatFailure`: any other code is `'generic'` (14-23)
- `frontend/src/lib/chatErrors.ts:42` — `retryAfterHint` reads only `details.retryAfterSeconds` (42-49); nothing under `frontend/src` reads a `Retry-After` header outside two test names
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:263` — only `'capacity'` (264) and `'modelNotAvailable'` (269) call `withdrawUserMessage`; the generic branch appends an error bubble (295-297)
- `frontend/src/components/chat/MessageBubble.tsx:506` — every error bubble with `onRetry` gets a Retry button (506-517); `AITab.tsx:259` passes `handleResend` for the last assistant error

**Change.** Have `refuse()` add `details.retryAfterSeconds` and the bucket group to the error body. Add a `'rateLimited'` `ChatFailureKind` that puts the user's message back into the draft and shows a toast with the wait, the same way capacity refusals do. Apply the same handling to the provider and Azure Test buttons. The Retry button stays. The change adds the stated wait and the restored draft.

**Touches.** `internal/api/middleware/ratelimit.go`, `internal/api/middleware/ratelimit_retry_after_test.go`, `frontend/src/lib/chatErrors.ts`, `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts`

**Done when.**
- A middleware test shows that the 429 body has code `RATE_LIMITED` and `details.retryAfterSeconds` equal to the `Retry-After` header.
- A frontend test shows that a 429 on `/api/chat/stream` restores the draft, shows the wait time and appends no error bubble.
