# AI roadmap: Tool loop

Items of category `tool-loop` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-tool-loop-3

**Prompt-based loop treats a <tool_call> cut at the output cap as the final answer, leaks the raw JSON, and embeds tool results unescaped** · tier 1

**Problem.** `runPromptToolLoop` serves Copilot and any provider with `SupportsTools=false`. It buffers the turn, parses complete `<tool_call>...</tool_call>` blocks and strips them. Suppose the output is cut at max tokens in the middle of a block. The regex then matches nothing, so `calls` is empty. `full != ""`, so `emptyOutputLimitTurn` is false. The loop emits `cleanText`, which still holds the partial `<tool_call>{"name": ...` JSON, and finishes with done. The user sees tool JSON in the answer, no tool ran, and the turn is recorded as successful. The native loop refuses such a turn through `truncatedToolTurn`. The prompt loop captures `turnFinish` but never applies the check. Tool results also go back verbatim inside `<tool_result>` markers; ADR 0026 lists this as open. Parsing reads only the assistant's own text (`chat.go:1460`), so a marker inside a result cannot itself be parsed as a call. The risk is that the model misreads where a result ends, or echoes an injected `tool_call` that the next parse then runs. The output-limit leak is the main defect.

**Evidence.**
- `internal/service/chat.go:1459` — `ParsePromptToolCalls` and `StripPromptToolCalls`; the `len(calls)==0` path (1465-1478) emits `cleanText` and `emitDone` with no `truncatedToolTurn` check
- `internal/service/chat.go:1441` — `turnFinish` is captured but only read by `emptyOutputLimitTurn(full != "", ...)` at 1466
- `internal/ai/prompt_tools.go:19` — `toolCallRe` requires the closing `</tool_call>` and matches any complete block in the assistant text; `StripPromptToolCalls` (69-71) removes complete blocks only
- `internal/service/chat_errors.go:87` — `truncatedToolTurn` exists for the native loop only (used at `chat.go:1314`)
- `internal/service/chat.go:1501` — `fmt.Fprintf(&results, "<tool_result name=%q>\n%s\n</tool_result>\n", tc.Name, result)` writes the raw result with no escaping; it is appended as a user message at 1503
- `docs/adr/0026-tool-specs-and-approvals.md:20` — Open: "the prompt loop embeds tool results unescaped in `<tool_result>` markers"

**Change.** In `runPromptToolLoop`, fail with `ChatErrOutputLimit` through `failUsage`, exactly like the native loop, when `isOutputLimitFinish(turnFinish)` holds and the text contains an unterminated `<tool_call>` (or any marker at all). Separately, strip an unterminated `<tool_call>` tail from `cleanText` so raw JSON never reaches the client. Escape the markers in tool results, or wrap each result in a block with a random delimiter. Either way, a forged marker inside a result cannot close the result block from the model's point of view.

**Touches.** `internal/service/chat.go`, `internal/ai/prompt_tools.go`, `internal/service/chat_prompt_loop_test.go`, `internal/ai/prompt_tools_test.go`, `docs/adr/0026-tool-specs-and-approvals.md`

**Done when.**
- A scripted turn `Let me look\n<tool_call>\n{"name":"search_flow"` with FinishReason `length` ends with an error event coded `PROVIDER_OUTPUT_LIMIT`. No chunk contains `<tool_call>` and no tool runs.
- `StripPromptToolCalls` (or a new helper) removes an unterminated trailing block.
- A tool result containing a forged `</tool_result>` cannot close the result block as the model sees it: the marker is escaped, or the block uses a random delimiter.

**ADR.** Unescaped `tool_result` markers are listed as open in ADR 0026 and not implemented.

### R-tool-loop-11

**Fix tools are advertised in the prompt and offered by the gate regardless of tool availability or the caller's flow permission** · tier 1

**Problem.** There are two defects. The first is the prompt. `SystemPromptDefault` hard-codes "The ONLY way to modify the flow is the apply_fix tool…" and "Prefer propose_fix", and `ContextRequest` has no input about tool availability. The same text therefore goes to three kinds of stream:
- the plain no-tools stream path
- chat-only tokens, whose gate removes `apply_fix` and `apply_fixes` (`propose_fix` has no `RequiredScope` and is still offered)
- providers without tools

The model announces fixes, or tells the user to approve a card that never appears. The prompt-loop preamble mentions `apply_fix` just as unconditionally, and the context cache key has no tools flag. The prompt already hedges ("If apply_fix is declined, times out, or unavailable, explain the change"), and a refused tool's result says the same. The harm is the model offering fixes that cannot happen, not claiming changes it did not make.

The second is the gate, which considers only token scope and whether an applier is wired, never the stream's flow permission. A stream needs only `ChatUse` (viewer), so:
1. A viewer's turn is offered `apply_fix`/`apply_fixes`.
2. The model proposes a fix and the `fix_proposal` card is emitted.
3. Only after the user approves does `GetAuthorizedFlow(FlowEdit)` refuse.

In the shipped UI a viewer usually never reaches that refusal. `respondFixProposal` returns early on approve when `flowStore.readOnly` is set, and the library workspace sets `readOnly = !flow.canEdit`. The viewer sees a card whose Approve button does nothing, and the turn waits out `FixDecisionTimeout` (60 s). Either way, the capability boundary is decided after the human-in-the-loop step rather than before the offer.

**Evidence.**
- `internal/ai/context.go:26` — the read-only paragraph names `apply_fix`/`propose_fix` unconditionally and already says to explain the change when `apply_fix` is unavailable; `ContextRequest` (`28-38`) has no tool-availability input, and `BuildContext` always starts from `SystemPromptDefault` (`40-44`)
- `internal/ai/tools.go:139` — `ToolRefusal`: only `RequiresApproval && !approvalsWired` and `RequiredScope` vs actor, with no flow-permission input (`139-147`)
- `internal/ai/tools.go:303` — `propose_fix` declares no `RequiredScope` (`303-311`); `apply_fix` and `apply_fixes` require `auth.ScopeWrite` (`322`, `335`); a refused tool returns "explain the change so the user can make it instead" (`219`)
- `internal/service/chat.go:918` — `NewToolGate(actor, tctx.Fixes != nil)`; the flow was resolved for `ChatUse` at `handlers_chat.go:61`, which is `flowPerm` viewer (`authz_can.go:92`); the plain Stream path (`928-937`) sends the same prompt without tools
- `internal/service/chat_fixes.go:73` — `ApplyFixWithApproval` checks only `requireFixWriteScope` before emitting the card at `85`; `FlowEdit` is checked in `applyApprovedFix` (`319-322`), after approval
- `internal/service/chat_context.go:125` — the context cache key params are block, prompt, provider, model, report, sources and exclude, with no tools flag (`125-128`)
- `internal/ai/prompt_tools.go:29` — the prompt-tool instructions mention `apply_fix` regardless of the gate
- `frontend/src/components/chat/hooks/stream-engine/fixes.ts:119` — `respondFixProposal` returns on approve when `readOnly` is set; `LibraryWorkspace.tsx:170` sets `readOnly: !flow.canEdit`; `FixDecisionTimeout` is 60 s (`internal/ai/tools.go:32`)

**Change.** Resolve the caller's flow permission once at stream start and pass it into `NewToolGate(actor, approvalsWired, canEditFlow)`. Give the refusal a `ToolRefusedFlowPermission` reason and metric label. Keep the apply-time `FlowEdit` check as defence in depth. Add a capability input to `ContextRequest` (none | read-only tools | fix tools), derived from `req.UseTools`, `provider.SupportsTools` and the gate. Render prompt variants that stay byte-stable; the no-tools variant says "you cannot change files; explain the change". Add the variant to the context cache key, and make the preamble of `ToolPromptInstructionsFor` mention `apply_fix` only when it is in the gated list.

**Touches.** `internal/ai/context.go`, `internal/ai/tools.go`, `internal/ai/prompt_tools.go`, `internal/service/chat_context.go`, `internal/service/chat.go`, `internal/service/chat_fixes.go`, `internal/service/metrics.go`, `internal/ai/context_test.go`, `internal/ai/tools_test.go`, `internal/service/chat_fixes_scope_test.go`

**Done when.**
- For a stream with viewer permission, `ToolDefinitionsFor` excludes `apply_fix`/`apply_fixes`, `ExecuteTool("apply_fix")` returns the not-available result counted with reason `flow_permission`, and no `fix_proposal` event is ever emitted on that stream.
- `TestBuildContext_NoTools`: the system prompt of a no-tools stream mentions neither `apply_fix` nor `propose_fix`; the system prompt for a chat-only token's stream does not mention `apply_fix`; the system prompt for a write-scoped editor's stream mentions both.
- Two requests that differ only in `UseTools` get different `chat.context` cache entries, and the existing editor `chat_fixes` tests pass.

### R-tool-loop-15

**Marker-mode tool instructions omit the input schemas and contradict the loop's multi-call behaviour** · tier 1

**Problem.** `ToolPromptInstructionsFor` renders only `- name: description` per tool. The `InputSchema` is never rendered. Some parameter names do appear in descriptions: `propose_fix` says "Input is the finding's blockId + ruleId" (`tools.go:307`), and `list_findings` mentions `ruleId` and "pass offset" (`tools.go:298`). These names are missing from the rendered text:
- `get_block`'s `blockId`, as its own parameter
- `get_subflow_blocks`' `subflowId` (the description says "subflow ID", `tools.go:289`)
- the `targets` array of `apply_fixes`, whose `{blockId, ruleId}` items are described only inside the schema (`tools.go:330`)
- `get_variable_lineage`'s `name`
- `search_flow`'s `limit`

A model without native tools must guess those argument names. It then spends iterations of the 6-iteration budget on "invalid input" and "blockId and ruleId are required" errors. The instructions also say "Emit at most one tool_call per response", while the prompt loop executes every block it finds in the turn.

**Evidence.**
- `internal/ai/prompt_tools.go:36` — only `Name` and `Description` are written (`36-38`); "Emit at most one tool_call per response" is at `39`
- `internal/ai/tools.go:272` — every tool has an `InputSchema` with its required parameter names (`272-343`)
- `internal/ai/tools.go:307` — the descriptions of `propose_fix` (`307`) and `list_findings` (`298`) already name `blockId`, `ruleId` and `offset`; the `{blockId, ruleId}` items of `apply_fixes` appear only in its schema (`330`)
- `internal/service/chat.go:1490` — every parsed call is executed in one turn (`1490-1502`)
- `internal/service/chat.go:42` — `maxToolIterations = 6`
- `internal/ai/tools.go:595` — a missing `blockId`/`ruleId` produces an error result the model must retry (`595-596`)

**Change.** Render a compact form of each tool's `InputSchema`, with its required list, into the marker instructions, and add one example call. Make the instruction and the loop agree: either allow several blocks per response in the text, or execute only the first. Assert an upper byte limit so the instructions do not crowd the context window.

**Touches.** `internal/ai/prompt_tools.go`, `internal/ai/prompt_tools_test.go`, `internal/service/chat.go`

**Done when.**
- `ToolPromptInstructionsFor(nil)` renders each tool's schema. The `get_subflow_blocks` entry contains `subflowId`, the `apply_fixes` entry contains the `targets` schema with its `{blockId, ruleId}` items, and the `get_variable_lineage` entry contains `name`. Today's descriptions already contain `blockId` and `ruleId`, so checking for those two alone proves nothing.
- `prompt_tools_test` asserts that the rendered instruction matches the loop's execution rule for multiple blocks.
- The instruction size stays bounded (assert an upper byte limit).

### R-tool-loop-19

**History load/save failure at stream start answers without context and stores an orphan answer** · tier 1

**Problem.** In cloud mode the turn's question and history are read and written in one compare-and-swap at stream start. When Postgres fails at that point, `reconstructAndPersistUserTurn` sets `req.Messages = nil` on a load failure and only logs a save failure. The stream goes on in both cases. On a load failure the model then answers a follow-up with no prior conversation. That answer is silently wrong ("fix the second one" has no referent), the client is told nothing, and `persistAnswer` stays true. In both cases, at done `persistAssistantTurn` cannot find the question's id in the stored history, so it appends the answer at the end of the previous turns. The store then ends with an answer directly after the previous assistant message. The client never saves a server-owned turn, so the orphan is permanent.

**Evidence.**
- `internal/service/chat_conversation_version.go:339` — load failure: `req.Messages = nil`, returns `stored=false`, `write=nil`; the stream continues (339-343)
- `internal/service/chat_conversation_version.go:346` — the save error is only logged (346-348); `!res.Versioned` gives a nil write
- `internal/service/chat.go:862` — `persistAnswer = true` is set before the user turn is stored (865); `userTurnStored` is set only when stored (868-870)
- `internal/service/chat_conversation.go:268` — the answer is inserted at `len(history)` when the question's id is absent (270-276); `userTurnStored` is not checked
- `internal/service/chat_context.go:377` — with `req.Messages` nil, `buildScopedMessages` sends only the new user turn
- `frontend/src/components/chat/hooks/stream-engine/events.ts:51` — "The server stored this turn … nothing left to save": the client never saves a server-owned turn (`save: !acc.serverOwned && !adopted`, 58)

**Change.** Fail the turn instead of guessing. A history load failure ends the stream with a retryable code before any provider call, so no tokens are spent on an answer without context. The code could be `HISTORY_UNAVAILABLE`, with the same copy pattern as `AI_BUDGET_UNAVAILABLE` (`internal/service/chat_errors.go:17`). When history loaded but the question could not be stored, `persistAssistantTurn` inserts the missing user turn immediately before the answer, in the same CAS round: `req.UserMessage` under `clientMessageID(req.UserMessageID)`. Answering without history on a load failure was a deliberate choice in the fix for the history-wipe bug (`internal/service/chat_history_loss_test.go:91-126`, mirrored in the file path at `internal/service/chat.go:188-194`). The change must keep `TestChatHistory_FailedLoadDoesNotWipeTheStoredConversation` green. The orphan-answer fix covers both triggers: the load failed at start, or the load succeeded but the save failed. The fix for answering without context applies only to the load failure.

**Touches.** `internal/service/chat_conversation_version.go`, `internal/service/chat_conversation.go`, `internal/service/chat.go`, `internal/service/chat_errors.go`, `frontend/src/components/chat/hooks/stream-engine/events.ts`, `frontend/src/lib/chatEvent.ts`

**Done when.**
- A FakeBackend whose `LoadConversationVersioned` fails once makes the stream end with the retryable error, and the provider is never called.
- A backend that fails `SaveConversationIf` at start but works at done leaves the stored conversation ending with [question, answer] in that order, with the question under the client's id.
- A test asserts that the server's own writes never append an assistant message directly after another assistant message.
- `TestChatHistory_FailedLoadDoesNotWipeTheStoredConversation` still passes.

### R-tool-loop-20

**A failed server-side answer write at done is silent: no client fallback, no metric, done looks normal** · tier 1

**Problem.** On a server-owned turn the answer is written inside `emit("done")` (`storeAnswer`). Any store error other than a CAS conflict makes `persistAssistantTurn` return `answerWrite{}` with only a Warn log; examples are a Postgres hiccup or hitting `persistAnswerTimeout`. `mutateConversationAt` returns non-conflict errors at once without touching any counter; the give-up counter fires only when CAS retries run out. `convWrite` is set to nil and the done event goes out without `conversationVersion`. The client has the full text and tool trail but does not save, because the turn is server-owned, and it has no toast for this path. The user sees a completed answer; a reload shows the question with no answer.

**Evidence.**
- `internal/service/chat_conversation.go:278` — a store error gives a Warn and `answerWrite{}`, not Written (278-281)
- `internal/service/chat_conversation_version.go:154` — non-conflict errors return at once with no counter (154-156); the `retry_exhausted` and give-up counters are only at 157-162
- `internal/service/chat.go:872` — `storeAnswer`: `ans.Written` false leads to `convWrite.Store(nil)` (877-881)
- `internal/service/chat_conversation_version.go:178` — `stampConversationVersion` adds nothing when `convWrite` is nil (178-181)
- `frontend/src/components/chat/hooks/stream-engine/events.ts:51` — server-owned done: `adoptStreamVersion` and commit, with no fallback save and no toast (51-58)
- `frontend/src/components/chat/hooks/stream-engine/commit.ts:34` — the client's own save path toasts on failure (34-39); the server-owned path has no equivalent signal

**Change.** Carry `answerStored` (true/false) on done and error for server-owned turns. When it is false, the client runs its existing CAS save (`saveThreadConversation`) and shows the `saveFailed` toast if that save fails too. On the server, retry transient errors (not conflicts, not context errors) once or twice within `persistAnswerTimeout`. Count failures in `pad_chat_answer_write_failures_total{cause=error|timeout|conflict}`. The counter also covers the two other exits of `mutateConversationAt`, the load error and the cancelled backoff (`internal/service/chat_conversation_version.go:136-139`, `168-169`), not only save errors. Keep the write before done so the version stamp stays correct when the write succeeds. Update ADR 0031 as well. Its Consequences paragraph says the client saves after a server-owned turn it cannot adopt (`docs/adr/0031-conversation-versioning.md:19`), but the code never does (`events.ts:58`).

**Touches.** `internal/service/chat_conversation.go`, `internal/service/chat_conversation_version.go`, `internal/service/chat.go`, `frontend/src/components/chat/hooks/stream-engine/events.ts`, `frontend/src/lib/chatEvent.ts`, `internal/api/handlers_chat.go`, `docs/adr/0031-conversation-versioning.md`

**Done when.**
- Go test: a backend whose `SaveConversationIf` fails with a non-conflict error at done emits done with `answerStored=false` and increments the new counter.
- Go test: a backend that fails once and then succeeds stores the answer and emits `conversationVersion`.
- Stream-engine test: done with `answerStored=false` on a server-owned turn calls `saveThreadConversation`, and a failed fallback save shows a toast.

### R-tool-loop-8

**Subflow IDs are undiscoverable and nested or large structures cannot be walked or paged by tools** · tier 2

**Problem.** Subflow IDs are random UUIDs minted by the parser, but `get_subflow_blocks` accepts only the ID, and almost nothing shows it:

- The Flow Overview lists subflow names only, and it appears only when no source files are attached and the context is under half the token budget.
- `search_flow` and `get_block` print the subflow name.
- `get_variable_lineage`'s `subflow=` field is also a name, despite the `SubflowID` field name.
- Only `list_findings` prints a `SubflowID`.

So a flow without findings cannot be navigated by subflow, and "what does the Login subflow do" cannot be answered.

PAD flows are mostly nested (loops, conditions, error blocks), and the tools cannot walk that structure:

- `get_subflow_blocks` lists top-level blocks with `(+N nested)`.
- `get_block` reports `children: N` without their IDs.
- Child IDs appear only for the selected block in the context, and only the first 8.
- `search_flow` scans every block, nested ones included, so a nested block can be found by a name or property substring, but not walked structurally.

Large listings cannot be paged:

- Neither `get_subflow_blocks` nor `search_flow` has an offset.
- A `search_flow` limit above 50 silently falls back to 15.
- A Main subflow with a few hundred top-level actions is cut at 6000 bytes with `(result truncated)` and no way to continue.

Repeated probing with guessed `search_flow` queries uses up the iteration cap.

**Evidence.**

- `core/parser/parser.go:137` — `builtSubflow{id: uuid.NewString(), name: tok.Name}`
- `internal/ai/context.go:255` — `writeFlowOverview` prints `sf.Name` and the block count only (259-261); child IDs appear only for the selected block, first 8 (227-236)
- `internal/ai/context.go:144` — the Flow Overview is written only without source files and under half the token budget (144-153)
- `internal/ai/tools.go:385` — `search_flow` prints `subflow=<name>` via `subflowName` (385-387); a limit of 0 or above 50 becomes 15, and there is no offset (366-369); it scans every block in `BlocksByID`, nested ones included (373-391)
- `internal/ai/tools.go:434` — `get_block` prints the subflow name and `children: %d` with no child IDs
- `internal/ai/tools.go:464` — `get_subflow_blocks` looks up `SubflowsByID[in.SubflowID]`; it lists top-level blocks only, with `(+%d nested)` (469-477); its schema has no offset (290)
- `internal/ai/tools.go:520` — only `list_findings` prints `f.SubflowID`; `truncateResult` (250-258) appends no continuation hint
- `core/analyzer/engine.go:732` — lineage events store `SubflowID: sf.Name`, which `get_variable_lineage` prints as `subflow=` (`tools.go:576`)

**Change.** This is tier 2: nothing breaks, but the model lacks a capability PAD work needs. Prefer extending the existing tools over adding many new ones, so each spec in `tools.golden` stays small:

- `get_subflow_blocks` accepts a unique subflow name as well as the id, and takes `offset`/`limit`.
- `get_block` gains a children section listing child id/type/name/line, with `offset` and `limit`.
- `search_flow` gains `offset`.
- Every paged listing ends with the same "…and N more — call again with offset=" trailer, and `truncateResult` keeps a pagination trailer.
- `search_flow`, `get_block` and `get_variable_lineage` print `subflow=<name> id=<id>`, and the Flow Overview prints `(id: …)`.
- A `list_subflows` tool (id, name, source file, block count, finding count) covers flows where the Flow Overview is not emitted.

Register any new tool as a read-only `ToolSpec` (ADR 0026) and refresh `tools.golden`.

**Touches.** `internal/ai/tools.go`, `internal/ai/context.go`, `internal/ai/testdata/tools.golden`, `internal/ai/tools_test.go`, `internal/ai/context_test.go`

**Done when.**

- On `synthetic_nested_loop`, a scripted model reaches a block three levels inside a loop using only `list_subflows` → `get_subflow_blocks` → `get_block`'s children section, without `search_flow`
- `get_subflow_blocks("Main")` and `get_subflow_blocks(<uuid>)` return the same listing; a 500-block subflow can be listed in pages, and the last page carries no continuation trailer
- `search_flow`, `get_block` and `list_findings` print the same subflow identifier for one subflow, and `tools.golden` lists the new or extended tools with `RequiresApproval=false`

### R-tool-loop-9

**No tool reads raw source or a full property value: values are trimmed to 200 characters and cloud flows never expose source** · tier 2

**Problem.** PAD actions carry their logic in property values: PowerShell, VBScript and DOS scripts, SQL statements, expressions, long selectors and paths. `get_block` renders every value through `trimValue`, which flattens newlines and cuts at byte 200 with `…` and no hint of how to see the rest. It slices `v[:max]` without backing off to a rune boundary, so unlike `truncateResult` it can split a multibyte character and send invalid UTF-8. Only the selected-block context writes properties in full. Raw source reaches the model only as whole selected files on the desktop, which requires `flowCache` and `doc.FilePath` and reads from disk. Cloud single-file documents carry the raw PAD source in `doc.Source`, but no tool reads it, and for multi-file cloud flows `Source` is empty. The system prompt asks the model to cite exact properties and line numbers, and patch previews cite lines. Yet in cloud the model cannot read a "Run PowerShell script" body unless that block is the selected context block, and it cannot inspect the lines it proposes to change.

**Evidence.**

- `internal/ai/tools.go:447` — `get_block` renders `trimValue(b.Properties[k])`
- `internal/ai/tools.go:797` — `trimValue` caps at 200 bytes with `v[:max]` (no rune back-off), flattens newlines and appends `…` with no continuation (797-804); `truncateResult` backs off to a rune boundary (254-257)
- `internal/ai/context.go:211` — the selected-block context writes properties in full, and only there (211-215)
- `internal/ai/context.go:18` — the system prompt: "Reference exact block names, properties, and line numbers."
- `internal/service/chat_context.go:361` — "Selected source files (desktop only)" requires `s.flowCache != nil && doc.FilePath != ""` (362-369)
- `internal/service/flow.go:912` — `ReadSourceFiles` reads `filepath.Join(dir, name)` from disk (912-926)
- `core/models/document.go:25` — `Source string`: raw PAD source kept for cloud apply/preview-fix, read by no tool
- `internal/service/flow.go:624` — `Source` is stored for single-file flows; it is empty for multi-file flows (`flow.go:1499`)
- `core/ai/scrubber/scrubber.go:100` — `ScrubDocument` starts from a shallow copy, so the scrubbed doc's `Source` is unmasked

**Change.** Add a read-only `read_source` tool that takes a block id or subflow id plus an optional line range, bounded to about 120 lines / 6000 bytes:

- On the desktop it reads the subflow's file.
- In cloud it reads `tctx.RealDoc.Source`, never the scrubbed `Doc`'s `Source` field, which is copied unmasked.
- Either way the text passes through `scrubSourceText` and `BoundSourceText`.
- For multi-file cloud flows, where `Source` is empty, it rebuilds the text from the stored files or answers "source unavailable".

Add `get_block_property(blockId, name, offset)`, which returns the scrubbed value in pages. Make `get_block` say "(…N more chars; get_block_property/read_source for the full value)" when it trims. That paging hint in `trimValue`'s output is the cheapest first step. In the same change, make `trimValue` cut at a rune boundary. Mark both tools read-only in the registry and pin them in `tools.golden`.

**Touches.** `internal/ai/tools.go`, `internal/ai/testdata/tools.golden`, `internal/ai/tools_test.go`, `internal/service/chat.go`, `internal/service/flow.go`

**Done when.**

- On a cloud flow, `read_source(blockId)` returns the scrubbed lines around the block with line numbers, with secrets masked exactly as the AST scrub masks selected files on the desktop
- For a block whose property holds a 5 KB script, two `get_block_property` calls return the whole value scrubbed, each within `maxToolResultBytes`, and a masked credential stays masked
- For a property longer than 200 characters, `get_block` output tells the model which tool reads the rest

### R-tool-loop-14

**Copilot never gets the flow tools: SupportsTools is false and the spec has no CapTools** · tier 2

**Problem.** `CopilotProvider.SupportsTools` returns false and `copilotSpec` lacks `CapTools`, so the chat service sends every Copilot tool turn through the prompt-marker loop. Copilot gets `search_flow`, `get_block`, `list_findings` and `propose_fix` only as prompt markers and is never offered them through native function-calling. Copilot is built on `openaiBase`, whose `buildRequest` already serialises `req.Tools` and parses `tool_calls`. The native loop also already degrades gracefully when a specific model refuses tools: `ErrToolsUnsupported` produces a `tools_unsupported` notice and the iteration is retried without tools.

**Evidence.**

- `internal/ai/copilot.go:95` — `SupportsTools() bool { return false }`
- `internal/ai/spec_copilot.go:18` — `Caps: CapChat | CapStream | CapReasoning | CapLiveListing`, no `CapTools`
- `internal/service/chat.go:919` — the native loop runs only when `ts.provider.SupportsTools()`; otherwise `runPromptToolLoop` runs (919-923)
- `internal/ai/prompt_tools.go:10` — the prompt-marker loop names GitHub Copilot as its use case (10-11)
- `internal/ai/openai_compat.go:127` — `buildRequest` already serialises `req.Tools` for every `openaiBase` provider (127-131)
- `internal/service/chat.go:1272` — a model that rejects tools degrades with a notice and the iteration is retried without them (1272-1279)
- `internal/ai/copilot.go:153` — `fetchModels` decodes only `id`, `name` and `context_limit` (153-159)

**Change.** Enable `CapTools` and `SupportsTools` for Copilot, and rely on the existing `ErrToolsUnsupported` degradation for models that refuse tools. Gating per model by the `/models` listing's capabilities, where the listing reports them, needs new parsing. `fetchModels` decodes only `id`, `name` and `context_limit`, and `modelListCache` caches the listing but exposes no capability data today. Refresh `providers.golden` and the spec completeness test.

**Touches.** `internal/ai/copilot.go`, `internal/ai/spec_copilot.go`, `internal/ai/copilot_provider_test.go`, `internal/ai/testdata/providers.golden`

**Done when.**

- A Copilot `Stream` with tools sends a `tools` array, and a `tool_calls` finish yields `Chunk.ToolCalls` on Done
- A Copilot stub model answering 400 "tools are not supported" produces `ErrToolsUnsupported` and the loop's `tools_unsupported` notice
- `TestProviderSpecs_Complete` passes with `CapTools` <=> `SupportsTools` for copilot

### R-tool-loop-7

**Single-call, fix-tool and prompt-loop tool executions run without the panic guard, and a panic after approval leaves the card unresolved** · tier 3

**Problem.** `guardedToolRun` turns a tool panic into the error-shaped result only on the parallel read-only path. `allReadOnly` requires more than one call. So every one-call turn and every turn containing `apply_fix` or `apply_fixes` calls `run(i)` bare, and the prompt loop calls `mem.exec` directly.

Model-controlled input can reach a panic, for example a blockId or ruleId that hits a bug in a fixer or builder. The panic unwinds through the applier, past two things:
- the desktop file write, which may already have committed
- `emitFixDecision`, which is not deferred

It reaches the stream worker's recover, which only logs, and `ensureTerminal` reports the stream as interrupted. The user's turn ends with a generic error, the card is never resolved with a `fix_decision`, and the model gets no readable result. This breaks `ExecuteTool`'s contract that tool failures become model-readable errors.

The unguarded surface is `PatchForFix` and the apply hook or flow write. A panic during the post-apply re-analysis is already recovered inside the analyzer engine.

**Evidence.**
- `internal/service/chat_tool_exec.go:27` — the `guardedToolRun` comment says "the sequential path needs none" (27-31); the guard is used only at 116, on the parallel path
- `internal/service/chat_tool_exec.go:81` — `allReadOnly := len(calls) > 1`; the sequential branch calls `run(i)` bare (89-98)
- `internal/service/chat.go:1498` — the prompt loop calls `mem.exec` directly
- `internal/service/chat.go:800` — the worker's recover only logs (800-804); `ensureTerminal` (812-816) emits the terminal error `STREAM_INTERRUPTED` (`chat_stream_control.go:352-353`)
- `internal/service/chat_fixes.go:103` — `applyApprovedFix` is at 103 and `emitFixDecision` at 105/113, neither deferred, so a panic skips both; only the keepalive is closed via defer (101-102)
- `internal/ai/tools.go:210` — `ExecuteTool`'s contract: it always returns a string so the model can recover
- `internal/ai/tools.go:645` — `analyzer.PatchForFix` runs unguarded
- `internal/service/chat_fixes.go:325` — the apply hook (and the batch applier's at 556) runs unguarded
- `core/analyzer/engine.go:146` — rule panics are recovered in `safeCheck` and `safeCheckFlow` (143-160), so a panic in the re-analysis does not reach the applier
- `internal/service/chat_fixes_keepalive_test.go:110` — the test pins that the apply panic propagates to the stream worker's recover (110-117)

**Change.** Treat this as hardening:
- Wrap `run(i)` in `guardedToolRun` on the sequential branch of `execTurnTools`, and wrap `mem.exec` in the prompt loop. Each is a one-line change.
- In both appliers (`ApplyFixWithApproval` and `ApplyFixesWithApproval`), defer a guard that resolves the card as `error` when no decision was emitted. A panic after approval then still emits `fix_decision{status: error}`.
- Keep the metric, the error-shaped result and the existing keepalive close via defer.
- Update the pinned assertion in `chat_fixes_keepalive_test.go:110-117` to expect an error-shaped result and a `fix_decision` error event instead of a propagated panic.
- Correct the `guardedToolRun` comment.

**Touches.** `internal/service/chat_tool_exec.go`, `internal/service/chat.go`, `internal/service/chat_fixes.go`, `internal/service/chat_tool_exec_test.go`, `internal/service/chat_fixes_keepalive_test.go`

**Done when.**
- A single-call turn whose tool panics returns a result starting with "error: tool <name> failed unexpectedly", `pad_chat_tool_results_total{tool,ok=false}` increments, and the loop continues to done.
- A panicking apply hook during an approved `apply_fix` yields an error-shaped tool result and a `fix_decision` error event, and the stream does not end as interrupted.
- A prompt-loop test with a panicking tool produces an error `tool_result` instead of a stream error.

### R-tool-loop-10

**No tool returns a finding's description/suggestion, and fix tools cannot target a finding by its stable key** · tier 3

**Problem.** `list_findings` prints one line per finding: severity, category, rule, title, block, subflow, key and fix type. A finding's description, suggestion and metadata reach the model in full only when the finding belongs to the selected context block. The Analysis Summary adds only the first suggestion per finding title. The Fix-with-AI button pastes the description, suggestion and autoFixHint into the prompt. It also sets `contextBlockId` to the finding's block, so the pasted text duplicates the selected-block context. Outside that path, no tool returns a finding's details.

The fix tools accept only `blockId`+`ruleId`, matched by equality on the current report. The system prompt tells the model to cite findings by their key. After every approved apply, however, the model is told "ALL block IDs have changed — call list_findings again", which costs an iteration. The key is not a stable target either: the fingerprint hashes the block's line number, so a patch that shifts lines changes it, although the comment on `FixProposal.Fingerprint` says it survives the re-parse.

**Evidence.**
- `internal/ai/tools.go:520` — the `list_findings` line carries the title only, with no description or suggestion (520-531)
- `internal/ai/context.go:93` — description, suggestion and metadata are rendered only for the selected block's findings (93-105)
- `internal/ai/context.go:60` — the Analysis Summary is written when findings exist (60-63) and includes the first suggestion per finding title (341-360)
- `frontend/src/lib/fixWithAI.ts:5` — the client pastes the description, suggestion and autoFixHint into the prompt (5-15) and sets `contextBlockId: finding.blockId` (27)
- `internal/ai/tools.go:585` — `fixTargetInput` has only `BlockID` and `RuleID` (585-588); `resolveFixTarget` matches `f.BlockID == in.BlockID && f.RuleID == in.RuleID` (606-612)
- `internal/ai/tools.go:57` — the comment on `FixProposal.Fingerprint` reads "content-stable finding identity — survives the re-parse the apply triggers"
- `core/analyzer/engine.go:46` — the fingerprint payload includes `block.LineNumber`
- `internal/service/chat_fixes.go:376` — "ALL block IDs have changed — call list_findings again before referencing any block" (also at 438)
- `internal/service/chat_fixes.go:392` — `fixContentKeyOf` re-associates findings by rule, subflow name, raw type, block name and subject (392-409)
- `internal/service/chat_fixes.go:347` — `applyApprovedFix` checks whether the finding is resolved by fingerprint alone (347-363)

**Change.** Add a read-only `get_finding` tool with no required scope, so chat-only tokens get it. It takes a `key` or `blockId`+`ruleId` and returns, scrubbed:
- description, suggestion and autoFixHint
- confidence, gating stage and metadata
- whether the finding's fix is behaviour-changing

For fix targets that survive a re-parse, let the `propose_fix`, `apply_fix` and `apply_fixes` targets carry a `key`. Resolve the key against the pre-apply report, then re-associate it on the current report by its line-free content identity (`fixContentKeyOf`), as `applyApprovedFixes` already does. Do not match `Fingerprint` on the new report. Identical sibling blocks may be ambiguous; accept that. The alternative is to drop key targeting and return the refreshed findings page in the apply result (the tier-0 pass did not add it; the apply result names only the grounded block's new id). Pin the new tool and the extended input schemas in `tools.golden`, and fix the misleading comment at `internal/ai/tools.go:57`.

A related defect for whoever touches this code: `applyApprovedFix` checks resolution by fingerprint alone. A fix that inserts lines before the target block changes that block's line and therefore its fingerprint, so an unresolved finding can be reported as "APPLIED and verified".

**Touches.** `internal/ai/tools.go`, `internal/ai/testdata/tools.golden`, `internal/service/chat_fixes.go`, `internal/ai/tools_fixes_test.go`, `internal/ai/tools_test.go`

**Done when.**
- `get_finding` on a key from `list_findings` returns the suggestion text with secrets masked.
- `apply_fix({key})` resolves the finding after an earlier approved apply has re-parsed the flow, without an intervening `list_findings`.
- `tools.golden` pins the new tool (no scope) and the extended input schemas.
