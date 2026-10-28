# AI roadmap: Frontend chat

Items of category `frontend` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-frontend-c7

**After the sidecar auto-restarts, the frontend keeps the dead backend's port and token: the crash banner closes and every request, chat included, fails until a manual reload** · tier 1

**Status (step 8).** The TypeScript half is done (see [Closed in step 8](../ai-roadmap.md#closed-in-step-8-conversation-durability-and-the-turn-record)): a `ready` after `terminated` or `error` drops the config, the GET cache and the event cursor, reopens the events stream, and ends every live chat stream with `BACKEND_RESTARTED`. What remains is the `lib.rs` change (clear `state.config` on `Terminated`) and the manual check of killing the sidecar from Task Manager, both of which need the Rust toolchain.

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

`/clear` and `/help` stay as they are. Tools are on by default since phase 1 of the open-item plan.

**Touches.** `frontend/src/components/chat/ChatInput.tsx`, `frontend/src/components/chat/SlashCommandAutocomplete.tsx`, `frontend/src/components/chat/hooks/useChatRequestBuilder.ts`, `frontend/src/i18n/en.ts`, `frontend/src/components/chat/ChatInput.test.tsx`

**Done when.**
- The command descriptions in `en.ts` and the other locales mention flows and blocks, not code or unit tests.
- The i18n coverage tests pass for the new keys.
- If the per-turn override is built: a message that starts with `/fix` produces a `ChatRequest` with `useTools=true` and the expanded prompt, whatever the thread toggle says.

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
