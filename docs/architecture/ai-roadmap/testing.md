# AI roadmap: Testing and evaluation

Items of category `testing` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-testing-3

**Backend chat event payloads and the frontend event parser share no fixture and already disagree on item keys** · tier 2

**Problem.** The backend builds the SSE and journal payloads (chunk, tool, tool_result, fix_proposal, fix_decision, done, error, heartbeat) as untyped maps, and the frontend parses them defensively. No test checks the two against each other.

The fix-patch item shows how the agreement is held today. The backend sends `id`, `ruleId`, `fixType`, `blockId`, `blockLabel`, `line` and `summary`. The frontend's `requestItem` reads `label` with a `blockLabel` fallback, which is the generic item field of the forward-compatible `approval_request` shape that ADR 0026 has the frontend parse (`0026-tool-specs-and-approvals.md:14`). It gets the block id from `id` (`fixItemID` is `blockId:ruleId`, `chat_fixes.go:132`).

Every check of that agreement is written by hand:

- Frontend unit tests feed hand-written payloads; `chatEvent.test.ts:477-494` covers the backend's batch item keys this way.
- The stream-engine tests inject post-parse objects that bypass `parseChatEvent` altogether.
- The e2e `DEFAULT_SSE` and fix-patch payloads are hand-written.

**Evidence.**
- `internal/service/chat_approvals.go:61` — `eventData` builds the `fix_proposal` map by hand: proposalId, tool, previewKind, requiredScope, items, plus flat fields (61-73)
- `internal/service/chat_fixes.go:135` — `fixProposalItem` keys are id (`blockId:ruleId`, 132), ruleId, fixType, blockId, blockLabel, line and summary (135-146)
- `frontend/src/lib/chatEvent.ts:112` — `requestItem` reads `rec.label`, falling back to `blockLabel` (the ADR 0026 generic item field); the block id travels inside `id` (106-119)
- `frontend/src/lib/chatEvent.test.ts:4` — the parser is tested only against hand-written payloads (4-30); lines 477-494 cover the backend's batch item keys by hand
- `frontend/src/components/chat/hooks/useChatStreamEngine.test.ts:850` — engine tests inject `kind:'fix-proposal'` objects directly into `onFixProposal` (850-859), bypassing `parseChatEvent`
- `frontend/e2e/chat-stream.spec.ts:174` — the unknown-preview-kind card test uses the generic item `{id,label,summary}` (174-179), which stays; `DEFAULT_SSE` (23-28) is hand-written

**Change.** Make the backend the single source of truth. A Go test drives a scripted turn (`FakeProvider`) through the tool loop and applier. It writes every emitted event type, plus one `/api/chat/resume` body, to `internal/service/testdata/chat_events/*.json` as a golden:

- single and batch `fix_proposal`;
- each `fix_decision` status;
- done with served model and rates;
- error with usage;
- tool, tool_result and heartbeat.

A vitest contract spec imports those JSON files (through a path alias or a copy step). It asserts that `parseChatEvent` returns a non-null event that keeps every field the backend emits, which makes the hand-written coverage in `chatEvent.test.ts:477-494` automatic. The stream-engine tests and the fix-patch e2e specs replay the golden payloads instead of hand-written ones, and goldens replace `DEFAULT_SSE`. The unknown-preview-kind e2e test (`chat-stream.spec.ts:173`) keeps its generic shape.

**Touches.** `internal/service/chat_events_golden_test.go`, `internal/service/testdata/chat_events/`, `frontend/src/lib/chatEvent.contract.test.ts`, `frontend/src/components/chat/hooks/useChatStreamEngine.test.ts`, `frontend/e2e/chat-stream.spec.ts`, `docs/adr/0026-tool-specs-and-approvals.md`

**Done when.**
- A backend golden exists for every chat event type and for the resume body, with a documented refresh flag.
- A frontend test fails if any golden payload parses to null or loses a field the backend emits.
- The streaming e2e specs and the fix-patch approval specs replay the golden payloads, not hand-written ones; the unknown-preview-kind test keeps its generic item.

**ADR.** ADR 0026 describes the envelope; there is no shared fixture.

### R-testing-7

**The only router-level approval test approves one apply_fix; decline, timeout and batch opt-outs (excludedItemIndices) never cross the HTTP boundary** · tier 2

**Problem.** `TestApprovals_RoundTripThroughRouter` proves scope and ownership and one approved single fix (statuses applying, then applied). The per-item opt-out path spans three layers:

- the frontend posts `excludedItemIndices`;
- the handler decodes `ExcludedItemIndices`, and `DecideApproval` passes `excludedBatchIdx` on;
- the applier emits `fix_decision` `items[]` per target.

Yet a grep over the `internal/api` tests finds zero hits for `excludedItemIndices`, `ExcludedItemIndices` or `apply_fixes`. The decline and timeout outcomes are asserted only at service level, through direct calls. A field rename or a lost index in any layer is invisible to CI.

**Evidence.**
- `internal/api/handlers_chat_approvals_test.go:81` — the script is one `apply_fix` tool call (81-83), `approved: true` (133) and asserted statuses `[applying applied]` (160-163); there is no decline, timeout or batch case
- `internal/api/handlers_chat.go:218` — `ExcludedItemIndices` is decoded (218) and passed to `DecideApproval` (230); a grep over `internal/api/*_test.go` for `excludedItemIndices|ExcludedItemIndices|apply_fixes` finds no matches
- `internal/service/chat_fix_decision_race_test.go:40` — `excludedBatchIdx` is covered only through direct service calls
- `internal/service/chat_commands_test.go:181` — the excluded index is asserted only on the cross-replica command path (181-186)
- `frontend/src/api/chat.ts:81` — the client posts `excludedItemIndices` (81-82), the field name the router test never sends

**Change.** Extend the router test into a table:

- (a) `apply_fixes` with three fixable targets, approved with `excludedItemIndices` `[1]`. Assert that `fix_decision` `items[]` is applied, skipped, applied in that order, and that the model's next request carries the per-item result text.
- (b) `approved: false`, asserting status declined and a final answer.
- (c) A timeout with the service's decision wait shortened, asserting status timeout and unchanged flow content.
- (d) A second decision after each outcome, answered with no-pending.

Three things need attention:

- **Case (c):** the wait override is unexported (`ChatService.fixDecisionWait`, `internal/service/chat.go:340`, and the `fixDecisionTimeout` var, `chat_approvals.go:21-29`). A router test in package `api` cannot set either, so this case needs an exported seam or a router/test constructor option.
- **Case (a):** asserting the model's next request needs a provider that records requests, which `FakeProvider` cannot do (`internal/testutil/provider.go:59`). That part depends on [R-testing-5](testing.md#r-testing-5).
- **Fallback route:** for approval-sourced cards, the client first posts to `/api/chat/approval-decision` and falls back to `/api/chat/fix-decision` on 404 (`frontend/src/components/chat/hooks/stream-engine/fixes.ts:128-134`, `frontend/src/api/chat.ts:84-90`). No backend route of that name exists, so a rename test must pin the fallback path as well.

**Touches.** `internal/api/handlers_chat_approvals_test.go`, `internal/service/chat_approvals.go`, `internal/testutil/provider.go`

**Done when.**
- Router tests cover `apply_fixes` with `excludedItemIndices`, decline and timeout, asserting the journal's `fix_decision` `items[]` and the stored flow content.
- Each case runs under `-race` within the existing 5 s poll budget.
- Renaming `excludedItemIndices` on either side fails a test.

### R-testing-8

**No snapshot of the assembled request (system prompt + context + messages + tools) for a fixture flow; BuildContext is not deterministic** · tier 2

**Problem.** What a model receives for a given flow is assembled across `BuildContext`, the system-prompt suffix, `buildScrubbedContext`, the tool gate and, in marker mode, `ToolPromptInstructionsFor`. No test pins the result as a whole. `context_test.go` checks that fragments are present, `PreviewContext` is tested only in pieces, and the marker-mode concatenation is never tested as text. So a change in ordering, headings, truncation priority or which tools each actor is offered changes the prompt without any golden diff. `BuildContext` also ranges over three unsorted Go maps that are reachable today: the title counts in `writeFindingsSummary`, the selected block's `Properties` in `writeBlockDetail`, and each block finding's `Metadata`. Its bytes are therefore not even stable between two calls, and a golden needs that stability first. `ContextRequest.VariableEvents` is also ranged unsorted, but the chat service never fills it in. The parser's sample corpus (14 flows, including `synthetic_10k.txt`) is never used as prompt input. Internal tests do call the real parser, but only on inline text.

**Evidence.**

- `internal/ai/context_test.go:51` — `BuildContext` tests check only for non-empty output and substrings (51-55); the nil-flow test checks identity only (369-372)
- `internal/ai/context.go:40` — `BuildContext` composes prompt + suffix + flow sections (40-62); no caller pins the whole output
- `internal/ai/context.go:101` — `for k, v := range f.Metadata` writes each block finding's metadata in map order
- `internal/ai/context.go:213` — `for k, v := range block.Properties` writes the selected block's properties in map order
- `internal/ai/context.go:351` — `for title, count := range counts` writes the Analysis Summary in map order (`RawSourceFiles` is sorted at 66-70; these three are not)
- `internal/service/chat_context.go:347` — the `ContextRequest` is built without `VariableEvents` (347-369), so the unsorted range at `context.go:135` cannot be reached today
- `internal/service/chat_context.go:310` — `contextText` is memoised per (doc, generation, request) in `chatCtxCache` (310-323)
- `internal/service/chat_context.go:632` — `PreviewContext` assembles sysPrompt/contextText/userMessage (632-647); no golden covers it over a fixture flow
- `internal/service/chat.go:1387` — marker mode appends `ToolPromptInstructionsFor` to `base.SystemPrompt` (1387-1394); the combined text is never tested as a whole
- `core/parser/testdata/samples` — 14 sample flows (`actions_flow.txt` ... `synthetic_10k.txt`); no test under `internal/` reads them, while internal tests parse inline text (`internal/service/analysis_test.go:15`, `internal/api/azure_foundry_e2e_helpers_test.go:451`)

**Change.** Sort the three reachable map ranges in `BuildContext`: `f.Metadata` at `context.go:101` (the live nondeterminism), `block.Properties` at 213 and the summary counts at 351. Either remove the unused `VariableEvents` field or sort it too. Pin determinism with a two-call byte-equality test that calls `BuildContext` directly, or otherwise bypasses `chatCtxCache`, which memoises `contextText` per document, generation and request. The test's selected block should have findings with two or more metadata keys. Then add a request-snapshot golden test in `internal/service`:

- Cover 3-4 corpus flows × gate (chat-only token, chat+write, no applier) × mode (native tools, marker).
- Run the same assembly path the worker uses.
- Write the system prompt, the context text, message roles and lengths, and the offered tool names to `testdata/request_snapshots/<flow>_<gate>_<mode>.golden`.
- Use a fixed token budget and a `FakeProvider` with a fixed `EstimateTokens` so truncation is reproducible.
- Compare with CRs stripped, and document a refresh flag.

**Touches.** `internal/ai/context.go`, `internal/ai/context_test.go`, `internal/service/chat_context.go`, `internal/service/chat_request_snapshot_test.go`, `internal/service/testdata/request_snapshots/`, `docs/architecture/safety-nets.md`

**Done when.**

- Two consecutive `BuildContext` calls on the same document produce identical bytes (pinned by a test)
- A golden per flow × gate × mode exists and fails on any change to prompt text, section order, truncation or offered tools
- The refresh flag is documented in `safety-nets.md`; the comparison strips CRs

### R-testing-11

**No opt-in live-provider smoke: wire drift from a vendor surfaces first in production** · tier 2

**Problem.** All `internal/ai` provider tests run on fixtures written by hand from documentation, whether streamed through `sseStub` or served by an httptest server. CI has no AI credentials, and the nightly workflow runs only benchmarks and fuzz. The code already expects vendors to drift: it has per-endpoint parameter repairs, learned windows and a served-model memo. Yet nothing ever confirms that today's fixtures still match what Claude, OpenAI, Gemini, xAI, GLM, GitHub Models and Azure actually send. A new SSE event type, a renamed usage field, a deprecated parameter or a changed tool-call encoding is discovered through a user's broken stream.

**Evidence.**
- `.github/workflows/ci.yml:102` — the go test step's env has only `AZURITE_CONNECTION_STRING` and `DATABASE_URL` (102-106); there is no provider secret
- `.github/workflows/bench-fuzz.yml:3` — the nightly schedule (3-7) runs only the benchmarks and the three-target fuzz matrix (78-85)
- `internal/ai/claude_replay_test.go:22` — SSE fixtures are inline Go strings written by hand (22-44), e.g. model `claude-opus-5-5`; a stub server records the request bodies (46-67)
- `internal/ai/openai_sse_test.go:113` — `sseStub` streams a fixed OpenAI SSE body through the real parser (113-121); other provider tests run the real client against an httptest server instead, and both kinds use hand-written fixtures
- `internal/storage/database/idle_tx_timeout_test.go:30` — the per-test `t.Skip` when `DATABASE_URL` is unset (30-32), the env-gated pattern `ci.yml:22-23` describes; provider keys should follow it

**Change.** Add `//go:build live` tests in `internal/ai`, gated per test on `BAKI_LIVE_<PROVIDER>_KEY` (and the Azure credential document), following the skip pattern in `idle_tx_timeout_test.go:30-32`. For each configured provider, they:

- list models;
- run one non-streaming Chat;
- run one Stream with the `search_flow` tool and a prompt that forces a call;
- run one Embed where the provider has `CapEmbeddings`.

Every call uses `MaxTokens <= 64` under a hard per-run dollar cap. The tests assert invariants rather than text: a terminal chunk arrives, usage is above zero or carried on the error, the tool call parses as valid JSON, and the served-model and cache fields are populated. The run also reports any new repair-memo entries. Add a non-gating nightly/dispatch workflow whose summary lists pass or fail per provider.

**Touches.** `internal/ai/live_smoke_test.go`, `internal/ai/live_helpers_test.go`, `.github/workflows/ai-live-smoke.yml`, `docs/architecture/ai-providers.md`, `docs/architecture/safety-nets.md`

**Done when.**
- `go test -tags live ./internal/ai -run TestLive` skips cleanly with no keys, and with keys runs one chat, stream and tools turn (plus embed) per provider.
- The run spends under a documented cap and never writes a usage row to a real database.
- A nightly/dispatch job exists and is non-gating; its summary lists pass or fail per provider and any new repair-memo entries.

### R-testing-12

**No behavioural eval set for the PAD assistant: prompt changes cannot be judged beyond 'the loop still runs'** · tier 2

**Problem.** The system prompt promises concrete behaviours:

- reference block names and line numbers;
- link `block:`/`finding:` URLs only for ids in context;
- prefer `propose_fix`;
- never claim a change that was not applied;
- list the most important issue first.

Nothing in the repository evaluates any of them. Every loop test scripts the model's turns through `FakeProvider`, so nobody observes the prompt's effect on a real model. There is also no dataset mapping a (flow, question) pair to an expected tool sequence or expected facts. A prompt or tool-description edit ships on faith. A regression in grounding, such as hallucinated block ids or claims of applied fixes, stays invisible until a user notices.

**Evidence.**
- `internal/ai/context.go:11` — `SystemPromptDefault` behaviours (11-26): `block:`/`finding:` links only for ids in context, the `propose_fix` preference, "never claim you changed anything you didn't"
- `internal/ai/context_test.go:370` — the prompt's only test is identity equality
- `internal/testutil/provider.go:59` — `FakeProvider` scripts the model's turns (59-84), so loop tests verify mechanics, never the prompt's effect
- `internal/ai/testdata/tools.golden:1` — `internal/ai/testdata` holds only `providers.golden` and `tools.golden`; Glob `**/*eval*` under `internal/` finds nothing
- `core/parser/testdata/samples/` — a ready corpus of 14 flows for eval cases

**Change.** Introduce `internal/ai/testdata/evals/*.json` cases of the form `{sample flow, optional selected block, question, expect: {toolsSubset, mustMention: [block ids / rule ids], mustLink: 'block:'|'finding:', mustNotClaimChange: true}}`. Add two runners:

1. **Offline and deterministic**, run in `go test ./...`. It replays a recorded transcript per case through the real tool loop and grades the final assistant text and tool trail. The transcript is the provider's turns, captured once with the live runner and stored as the fake's script.
2. **Live**, gated on the same env as the live smoke. It prints a pass rate per model and a per-case diff.

**Touches.** `internal/ai/testdata/evals/`, `internal/service/chat_eval_test.go`, `internal/ai/eval_live_test.go`, `docs/architecture/ai-providers.md`, `docs/testing.md`

**Done when.**
- An eval case format exists, with at least 12 cases over the sample corpus.
- The offline replay runner runs in `go test ./...` and fails when a change to the loop or prompt plumbing changes a graded outcome.
- The live runner, gated on the live-smoke env, prints a pass rate per model and a per-case diff.

### R-testing-2

**Provider SSE parsers, the Responses parser, the marker parser and tool-input decoding have no fuzz targets** · tier 3

**Problem.** Hand-written state machines parse untrusted input with no fuzzing at all:
- bytes from four upstream wire formats: Claude, OpenAI-compatible, Gemini and Azure Responses
- model-authored text and JSON: marker `<tool_call>` blocks and tool inputs

The repository's only Fuzz functions are `FuzzParser`, `FuzzApplyPatch`, `FuzzFixerPipeline`, `FuzzKey_Injective` and `FuzzJPEGOrientation`. The nightly fuzz matrix runs exactly three of them, all in `core`. The chat worker's recover contains a parser panic or bad state, so the failure shows up as unexplained interrupted turns rather than a crash. Scanner lines are already capped, so the allocation risk is in accumulated state (tool-call and argument buffers), not in single lines.

**Evidence.**
- `.github/workflows/bench-fuzz.yml:78` — the fuzz matrix (78-85) runs only `FuzzApplyPatch`, `FuzzFixerPipeline` and `FuzzParser`
- `.github/workflows/bench-fuzz.yml:107` — the crash-corpus upload paths cover only `core/analyzer/testdata/fuzz/` and `core/parser/testdata/fuzz/` (107-109)
- `core/parser/parser_test.go:548` — `FuzzParser`. A grep for `^func Fuzz` over `*_test.go` finds only it, `core/analyzer/fuzz_test.go:17,65`, `core/cache/key_fuzz_test.go:40` and `internal/service/avatar_test.go:347`; nothing is under `internal/ai`
- `internal/ai/stream.go:292` — hand-written SSE state machines: `parseClaudeSSE` (292), `parseOpenAISSE` (531), `parseGeminiSSE` (807); each caps its scanner lines (294, 533, 809)
- `internal/ai/responses.go:1330` — `parseResponsesSSE`, the parser for Azure Responses and agent events
- `internal/ai/prompt_tools.go:19` — the `toolCallRe` regex runs over model text; `ParsePromptToolCalls` is at 45
- `internal/ai/tools.go:747` — tool handlers `json.Unmarshal` model-supplied input (`toolApplyFixes`, 743-748); nothing fuzzes `ExecuteTool` inputs
- `internal/service/chat.go:800` — the stream worker's recover only logs (800-804)

**Change.** Add fuzz targets in `internal/ai`:
- `FuzzParseClaudeSSE`, `FuzzParseOpenAISSE`, `FuzzParseGeminiSSE` and `FuzzParseResponsesSSE`, seeded with the inline fixtures already in the tests.
- `FuzzParsePromptToolCalls`.
- `FuzzExecuteTool`, which runs each registered tool on arbitrary input against `toolFixtureDoc` and asserts: no panic, a result no longer than `maxToolResultBytes` plus the marker, and valid UTF-8. This is the lowest-value target.

The four SSE parsers and `FuzzParsePromptToolCalls` matter most. Their fuzz bodies assert no panic, at most one terminal chunk, valid JSON in every returned tool input, and a return within budget. The scanner caps already rule out unbounded allocation per line. Add all six targets to the `bench-fuzz.yml` matrix, and extend the upload-artifact paths (107-109) to `internal/ai/testdata/fuzz/` so their crash corpus is uploaded.

**Touches.** `internal/ai/stream_fuzz_test.go` (new), `internal/ai/responses_fuzz_test.go` (new), `internal/ai/prompt_tools_fuzz_test.go` (new), `internal/ai/tools_fuzz_test.go` (new), `.github/workflows/bench-fuzz.yml`, `internal/ai/testdata/fuzz/` (new)

**Done when.**
- Six fuzz targets exist and run in the nightly matrix, with their crash corpus uploaded.
- Each target's seed corpus includes the existing inline SSE fixtures from `claude_replay_test.go`, `openai_sse_test.go`, `stream_test.go` and `responses_replay_test.go`.
- The invariants above are asserted inside the fuzz body, not just "no panic".

### R-testing-4

**tools.golden pins neither tool descriptions nor input schemas; the system prompt and marker instructions have no golden** · tier 3

**Problem.** No golden pins the prompt-facing text the model reads from Baki: the eight tool descriptions, their JSON input schemas, `SystemPromptDefault` and the `<tool_call>` marker instructions. `tools.golden` has six metadata columns: name, label, timeout, approval, scope and previewKind. The test that writes it never touches `Def.Description` or `Def.InputSchema`. The schema test only checks that each schema is parseable JSON, and `SystemPromptDefault` is asserted only against itself (`sys == SystemPromptDefault`), never against pinned text. A rewording that changes model behaviour therefore ships without a reviewable diff. Examples are dropping "ALWAYS requires explicit user approval" from `apply_fix`, or letting `apply_fixes`' `maxItems` drift away from `maxBatchFixTargets`.

**Evidence.**
- `internal/ai/tools_spec_test.go:95` — the golden line format is `# name | label | timeout | approval | scope | previewKind` (95-108); Description and InputSchema are never written
- `internal/ai/testdata/tools.golden:1` — six columns and eight rows, with no description or schema text
- `internal/ai/tools.go:316` — the `apply_fix` description and schema (316-317); `apply_fixes` (329-330) has `maxItems` 10 in its schema
- `internal/ai/tools.go:741` — `maxBatchFixTargets = 10` is enforced by hand in `toolApplyFixes` (753-755); nothing ties it to the schema's `maxItems`
- `internal/ai/tools_test.go:190` — `TestToolDefinitions_StableAndComplete` only `json.Unmarshal`s each schema (190-197)
- `internal/ai/context_test.go:370` — the system prompt is checked only as `sys != SystemPromptDefault`; the prompt text itself is at `context.go:11-26`

**Change.**
- Assert that `apply_fixes`' `maxItems` equals `maxBatchFixTargets`. This is the cheapest valuable part.
- Extend the tools golden with one block per tool: its description text and its canonicalised `InputSchema` (sorted keys, compact JSON). Refresh it with the existing `-update-tools` flag instead of adding a new one.
- Add `testdata/prompts.golden` holding `SystemPromptDefault`, `ToolPromptInstructionsFor(nil)` and the chat-only-gate variant, refreshed by `-update-prompts`.
- Add a schema-versus-handler consistency test. For each tool, build an input from the schema's `required` properties and assert that the handler does not return the "required" error. Then drop each required field in turn and assert that it does.
- List `-update-prompts` in `docs/architecture/safety-nets.md` and `CLAUDE.md`; both already list `-update-tools`.

**Touches.** `internal/ai/tools_spec_test.go`, `internal/ai/testdata/tools.golden`, `internal/ai/testdata/prompts.golden` (new), `internal/ai/tools_test.go`, `internal/ai/context_test.go`, `docs/architecture/safety-nets.md`, `CLAUDE.md`

**Done when.**
- Changing one character of any tool Description, any InputSchema, `SystemPromptDefault` or the marker instructions fails a golden test with a readable diff.
- `-update-prompts` is listed in `docs/architecture/safety-nets.md` and `CLAUDE.md`, beside the existing `-update-tools`.
- A test proves that each tool's schema `required` set matches the fields its handler rejects as missing, and that `apply_fixes`' `maxItems` equals `maxBatchFixTargets`.

### R-testing-5

**testutil.FakeProvider has no failure modes, ignores ctx and the request, and its call counter is unsynchronised; ~34 ad-hoc provider stubs re-implement it per test file** · tier 3

**Problem.** The shared fake can only script text and Done chunks, or fail before any chunk (`Err`). It ignores ctx and the request and never records what it received. It cannot emit:
- a mid-stream error chunk
- a retryable, context-limit, tools-unsupported or budget error
- usage on an error
- a keepalive or a served model
- a delay or a blocking turn

`Chat` returns an empty `Response` and `Embed` returns nil vectors. The fake's `calls` counter is read and written without a mutex while the chat worker calls `Stream` on another goroutine. That is a latent hazard, not an existing race: today every `Calls()` reader runs the loop synchronously.

Every failure-mode test therefore writes its own provider type. A grep finds 34 `Stream` implementations across 28 `*_test.go` files under `internal/`. Ten of them are in `package ai`, which cannot import `testutil` because `testutil` imports `internal/ai`. Examples:
- `itemsProvider` scripts chunks, errors, limit, weights and models, and records requests.
- `promptLoopProvider` exists only to record requests.
- `chunkErrProvider` emits one error chunk.
- Six are hand-rolled ctx-aware blocking wrappers: `parkedProvider`, `cmdProvider`, `blockingProvider`, `xrBlocking`, `blockingStreamProvider` and `untilCancelledProvider`.

**Evidence.**
- `internal/testutil/provider.go:25` — FakeProvider has the fields Turns, Fallback, Err, Ctx, Tools and calls (25-32); there is no per-turn error, delay or request capture
- `internal/testutil/provider.go:59` — `Stream(_ context.Context, _ ai.Request, ...)` ignores ctx and req; `Err` fails before any chunk; `i := p.calls; p.calls++` (63-64) has no mutex; only Text/Parts and one Done chunk are scriptable (59-84)
- `internal/testutil/provider.go:55` — `Chat` returns `&ai.Response{}` (55-57); `Embed` returns `nil, nil` (46)
- `internal/testutil/provider.go:6` — `testutil` imports `internal/ai`, so `package ai` tests cannot use it
- `internal/service/chat_test.go:612` — the `Calls()` readers run the loop synchronously (612-702; also `chat_activestream_report_test.go:381-515`)
- `internal/service/chat_replay_test.go:25` — `itemsProvider` re-implements scripted chunks, errors, limit, weights, models and request recording (25-61)
- `internal/service/chat_shutdown_test.go:26` — `parkedProvider`, a ctx-aware blocking wrapper (26-50); `cmdProvider` (`chat_commands_test.go:24-29`) and `blockingProvider` (`chat_turn_lifecycle_test.go:452-457`) are two more
- `internal/di/two_replicas_test.go:66` — `xrBlocking`, another ctx-blocking wrapper; also `internal/api/handlers_chat_activestream_test.go:25` and `internal/app/stop_sequence_test.go:334`
- `internal/service/chat_errors_test.go:124` — `chunkErrProvider` emits a mid-stream error chunk; `promptLoopProvider` (`chat_prompt_loop_test.go:15-26`) exists only to record requests
- `internal/service/chat_turn_lifecycle_test.go:496` — `TestStream_LegacyCancelledBeforeBegin_RecordsTerminalState` depends on a fake that ignores ctx (496-500)

**Change.** Narrow the change instead of building one over-general fake:
- Guard `calls` with a mutex.
- Add an optional `Requests()` recorder and an opt-in ctx check to `testutil.FakeProvider`. The check is opt-in because `TestStream_LegacyCancelledBeforeBegin_RecordsTerminalState` depends on a fake that ignores ctx.
- Fold in the stubs that only duplicate the fake: `itemsProvider`'s request recording and `promptLoopProvider`.

Limit the change to stubs outside `internal/ai`. Do not add an archtest ratchet on test-file `Stream` implementations. `internal/archtest` ratchets production layering rules, and a stub that embeds the shared fake to override one method is a legitimate pattern, not drift.

**Touches.** `internal/testutil/provider.go`, `internal/testutil/provider_test.go` (new), `internal/service/chat_replay_test.go`, `internal/service/chat_prompt_loop_test.go`

**Done when.**
- `FakeProvider` records every `Request`. With its ctx check enabled it returns when the stream's context ends; with the check off, `TestStream_LegacyCancelledBeforeBegin_RecordsTerminalState` still passes.
- `Calls()` and `Requests()` pass `-race` when read concurrently with `Stream`.
- `promptLoopProvider` and `itemsProvider`'s request recording are replaced by the shared fake, and no archtest ratchet pins test-file `Stream` implementations.

### R-testing-9

**Tool handlers are exercised only on a three-block hand-built fixture, never on the parser's sample corpus** · tier 3

**Problem.** Every `internal/ai` tool test runs on `toolFixtureDoc` (two top-level blocks and one child) with a canned `fakeAnalysis` report. No test parses a real PAD flow and runs the six read-only tools over it. As a result, these behaviours are verified only on synthetic input:
- result formatting on large or unusual flows (`synthetic_10k.txt`, `nested_flow.txt`, `goto_label.txt`, `dotted_actions.txt`)
- the 6000-byte truncation (covered on a generated string)
- `list_findings` pagination (covered on 95 canned findings)
- scrubbing of real property values

None of these runs on real parser and analyzer output for the sample corpus. The tools are the assistant's grounding layer, and no test sees how they behave on actual flows.

**Evidence.**
- `internal/ai/tools_test.go:14` — `toolFixtureDoc` has b1 (`Database.Connect`) and a b2 loop with child b3, and is the only document the tool tests use (14-44)
- `internal/ai/tools_test.go:46` — `fakeAnalysis` returns a canned report and lineage (46-57)
- `internal/ai/tools_test.go:157` — `truncateResult` is covered on a generated rune string and on an ASCII string at the cap (157-183)
- `internal/ai/tools_fixes_test.go:284` — offset paging is covered over 95 canned findings (281-308)
- `internal/ai/tools.go:24` — `maxToolResultBytes = 6000`, which the fixture never reaches
- `internal/ai/tools.go:298` — `list_findings` promises pagination (offset) and `stage=preview` lines, but is exercised only on canned reports
- `internal/ai/tools.go:16` — `tools.go` already imports `pad-core/analyzer`
- `core/parser/testdata/samples` — 14 sample flows, including `synthetic_10k.txt`, `nested_flow.txt`, `goto_label.txt` and `dotted_actions.txt`. A grep for `testdata/samples` under `internal/` finds nothing, although internal tests do parse flows (for example `parser.ParseText` at `internal/service/analysis_test.go:15`)

**Change.** Add `internal/ai/tools_corpus_test.go`. It parses every file in `core/parser/testdata/samples` with the real parser and analyzes it by calling `pad-core/analyzer` directly behind a test `ToolAnalysis`, not through the `*service.AnalysisService` adapter. Then, for each flow, it calls:
- `search_flow`, with the first block name fragment as the query
- `get_block`, for every block id
- `get_subflow_blocks`, for every subflow
- `get_variable_lineage`, for every variable
- `list_findings`, paged until exhausted
- `propose_fix`, for every auto-fixable finding

Assert no `error:` prefix for valid ids, no panic, a result no longer than 6000 bytes plus the marker, valid UTF-8, and no secret-looking property values. Write a per-flow output digest golden with a refresh flag.

**Touches.** `internal/ai/tools_corpus_test.go` (new), `internal/ai/testdata/tools_corpus.golden` (new), `core/parser/testdata/samples`

**Done when.**
- All sample flows pass the invariants for all six read-only tools.
- `list_findings` on `synthetic_10k.txt` proves that the pagination hint survives truncation.
- The per-flow output digests are a golden with a documented refresh flag.

### R-testing-10

**No checked-in provider wire fixture corpus: every SSE case is an inline Go string with no parser-output golden** · tier 3

**Problem.** `internal/ai/testdata` holds only `providers.golden` and `tools.golden`. Every SSE case (Claude, OpenAI, Gemini, Responses) is an inline Go string handed to a parser. A change in a real provider's event shape (a new block type, a renamed usage field, a new error envelope) is therefore caught only when someone writes a case by hand. There is no way to refresh fixtures from a live capture and review the diff in parser output, which is how every other golden in the repository works.

**Evidence.**
- `internal/ai/testdata/providers.golden:1` — a glob of `internal/ai/testdata` returns only `providers.golden` and `tools.golden`
- `internal/ai/stream_test.go:67` — an inline SSE string is handed to `parseGeminiSSE(strings.NewReader(input), ...)` (67-71)
- `internal/ai/gemini_errors_test.go:82` — inline SSE input built by string concatenation (82-83)
- `internal/ai/spend_integrity_test.go:20` — inline SSE input handed to `parseGeminiSSE`
- `internal/ai/claude_replay_test.go:22` — Claude SSE turns as inline Go strings (22-44)
- `docs/architecture/safety-nets.md:23` — for `internal/ai`, the golden table lists only `providers.golden` and `tools.golden` (23-24)

**Change.** Add fixtures at `internal/ai/testdata/wire/<family>/<case>.sse` for the `anthropic`, `openai`, `responses` and `gemini` families, captured from real streams with secrets scrubbed. Add a table test that runs each fixture through its parser and compares the emitted chunk sequence against `<case>.golden`. The compared sequence covers text, tokens, cache, finish, tool calls, error class and keepalive count. The test takes an `-update-wire` flag and compares with CR stripped. Add a capture recipe (curl with the provider's stream flag), and list the golden in `docs/architecture/safety-nets.md` and `CLAUDE.md`.

**Touches.** `internal/ai/wire_golden_test.go` (new), `internal/ai/testdata/wire/` (new), `docs/architecture/safety-nets.md`, `CLAUDE.md`

**Done when.**
- `go test ./internal/ai -run TestWireGolden` fails when a fixture's parsed chunk sequence differs from its golden, and passes after `-update-wire`.
- Each wire family has at least these cases: a plain answer, a tool-call turn, a mid-stream error, a content-filter stop, a truncated body and, where the API has one, a thinking or reasoning phase.
- The golden is compared with `\r\n` normalised, as `providers.golden` is.

### R-testing-13

**The only browser-level streaming test pins crypto.randomUUID, so the assistant commit is deduplicated away and never asserted; SSE is a static body** · tier 3

**Problem.** `frontend/e2e/chat-stream.spec.ts` fixes `crypto.randomUUID` to one value so the stub can address the stream id. The engine, however, derives the user message id, the assistant message id and the stream id from the same `uuid()`, and `appendMessage` drops a message whose id already exists in the thread. The assistant commit is therefore discarded, and the spec asserts only the user text and the Generating tab, so a broken commit, save or done path would pass. Every `/api/events` request gets one fixed body. As a result, reconnect, Last-Event-ID replay, resume by offset, `STREAM_ID_IN_USE` and `REPLICA_UNREACHABLE` are exercised only by unit tests with stubbed `ApiError`s.

**Evidence.**
- `frontend/e2e/chat-stream.spec.ts:33` — `Object.defineProperty(crypto, 'randomUUID', {value: () => 'e2e-fixed-stream-id'})`
- `frontend/src/lib/uuid.ts:3` — `uuid()` returns `crypto.randomUUID()`
- `frontend/src/components/chat/hooks/stream-engine/lifecycle.ts:118` — the user id (118), the assistant id `msgId` (124) and the stream id `sid` (176) all come from `uuid()`
- `frontend/src/stores/chat/messages.ts:22` — `appendMessage` returns the state unchanged when the id already exists
- `frontend/e2e/chat-stream.spec.ts:148` — the spec asserts only the user text (148) and the Generating tab (149), never the assistant text
- `frontend/e2e/chat-stream.spec.ts:102` — the route handler (102-120) answers every `/api/events` request with the same static `sseBody` (104-110), so a reconnect gets the identical body again and nothing handles Last-Event-ID
- `frontend/e2e/chat-stream.spec.ts:85` — static per-route stubs for begin, stream, cancel, resume and fix-decision (85-97); a grep of `frontend/e2e` for Last-Event-ID or `resumeAfterReload` finds nothing

**Change.** Stop pinning `randomUUID`. Intercept `POST /api/chat/stream`, read `clientStreamId` from the body and build the SSE events for that id; serve `/api/events` from a route handler that waits for the stream POST. Assert the committed assistant text and its Copy and Regenerate controls. Add two scenarios:
- The SSE is cut after the first chunk. Assert a second `/api/events` request carrying `Last-Event-ID`, and a resume POST whose `from` equals the UTF-8 length of the text received.
- A create is answered 409 `STREAM_ID_IN_USE`. Assert that a resume follows instead of an error bubble.

**Touches.** `frontend/e2e/chat-stream.spec.ts`, `frontend/e2e/helpers.ts`, `frontend/e2e/sse-fixture-server.ts` (new), `frontend/playwright.config.ts`, `frontend/vitest.config.ts`

**Done when.**
- The streaming spec asserts "This flow has 1 error to fix." as a committed assistant message (not `aria-hidden`), with distinct user, assistant and stream ids.
- A reconnect spec asserts that the second `/api/events` request carries `Last-Event-ID`, and that `/api/chat/resume` is posted with `from` equal to the byte length of the text received so far.
- A `STREAM_ID_IN_USE` spec asserts that the turn resumes instead of showing an error bubble.

### R-testing-14

**e2e coverage of both apply paths is vacuous or dismiss-only** · tier 3

**Problem.** `frontend/e2e/apply-fix.spec.ts` wraps the Findings tab, Run Analysis and Apply fix clicks in `if (count > 0)`, so it passes when any of them is missing. It also stubs `/preview-fix` to 403, so the `PatchPreviewModal` path is never driven. `chat-stream.spec.ts` only dismisses a card of an unknown preview kind. No browser test approves a fix-patch card and then checks the applied status and the re-analysis refresh. A regression that hides the Apply fix button, breaks the preview modal, or breaks the approve → applied → refresh path passes CI.

**Evidence.**
- `frontend/e2e/apply-fix.spec.ts:27` — `preview-fix` is stubbed to 403 (27-29)
- `frontend/e2e/apply-fix.spec.ts:57` — the Findings tab, Run Analysis and Apply fix are each guarded by `if ((await x.count()) > 0)` (57-73); only the "Unhandled error" check at 66 is unconditional
- `frontend/e2e/chat-stream.spec.ts:209` — only Dismiss on an unknown-kind card is exercised (209-214)

**Change.** In `apply-fix.spec.ts`:
- Assert that the Apply fix button and the modal exist.
- Stub `/preview-fix` with original and patched text.
- Drive `PatchPreviewModal` through apply, then assert the finding disappears.

Add a chat e2e that:
- stubs a fix-patch `fix_proposal` and clicks Approve
- asserts the fix-decision POST (`approved: true`) and a later analyze call
- checks the `applied` status after a stubbed `fix_decision`

**Touches.** `frontend/e2e/apply-fix.spec.ts`, `frontend/e2e/chat-stream.spec.ts`, `frontend/e2e/helpers.ts`

**Done when.**
- `apply-fix.spec` fails when the Apply fix button or `PatchPreviewModal` is absent.
- A chat e2e posts fix-decision with `approved=true` and shows the applied status.

### R-testing-18

**The exact-prefix truncation tests skip silently when the tokenizer fails to load** · tier 3

**Problem.** `requireTokenizer` calls `t.Skip` when `getTokenizer()` returns nil. The vocabulary is embedded through the offline BPE loader, so the skip fires only when `GetEncoding` fails. When it does fail, production silently falls back to the heuristic. The four tests that call `requireTokenizer` skip, so nothing fails:
- `TestTruncateToTokenLimit_PrefixPathIsExact`
- `TestTokenStablePrefix_TokensArePrefixOfWhole`
- `TestTruncateToTokenLimit_EncodesOnlyThePrefix`
- `TestBuildContext_BoundsSourceFilesBeforeTokenising`

`TruncateToTokenLimit` bounds two things on that tokenizer: BuildContext's flow context and the RAG guidelines cap.

**Evidence.**
- `internal/ai/tokens_truncate_test.go:43` — `requireTokenizer` calls `t.Skip("cl100k tokenizer unavailable")` when `getTokenizer()` is nil (43-50)
- `internal/ai/tokens.go:17` — `getTokenizer` sets the offline loader (21) and leaves `tokenizer` nil on a `GetEncoding` error (22-27)
- `internal/ai/tokens.go:117` — with no tokenizer, `TruncateToTokenLimit` uses the fallback heuristic
- `internal/ai/context.go:156` — BuildContext's flow context goes through `TruncateToTokenLimit`
- `internal/service/chat_context.go:236` — the RAG guidelines are capped with `ai.TruncateToTokenLimit`
- `internal/ai/tokens_truncate_test.go:57` — `requireTokenizer` callers: 57, 98, 162, and `internal/ai/context_bound_source_test.go:41`

**Change.** Fail instead of skipping. Because the loader is embedded, `requireTokenizer` can call `t.Fatal` unconditionally. If a failure only in CI is wanted instead, read `CI` in `internal/testutil`, where the archtest `os.Getenv` exemption for test tooling applies.

**Touches.** `internal/ai/tokens_truncate_test.go`

**Done when.**
- With the offline loader stubbed to fail, `TestTruncateToTokenLimit_PrefixPathIsExact` fails instead of skipping.

### R-testing-19

**tools.golden is compared byte-for-byte while safety-nets.md says every golden strips CR** · tier 3

**Problem.** `TestToolSpecsGolden` compares `string(want) != got` with no CRLF normalisation, unlike `providers.golden` and `metrics.golden`, which strip `\r\n`. The test only passes on Windows because `.gitattributes` forces `.golden` files to LF. An editor or copy that re-ends the file produces a misleading whole-file diff, and the documentation's blanket statement is false for this test.

**Evidence.**
- `internal/ai/tools_spec_test.go:124` — `if string(want) != got {` with no normalisation
- `internal/ai/spec_test.go:271` — `strings.ReplaceAll(string(want), "\r\n", "\n")` before comparing `providers.golden`
- `internal/metrics/metrics_golden_test.go:197` — the same normalisation for `metrics.golden`
- `docs/architecture/safety-nets.md:3` — "Golden tests compare with CR stripped"

**Change.** Normalise `\r\n` in `TestToolSpecsGolden` the way the other two goldens do. If the tools golden rewrite in [R-testing-4](testing.md#r-testing-4) lands first, fold this into it.

**Touches.** `internal/ai/tools_spec_test.go`

**Done when.**
- `TestToolSpecsGolden` passes when `testdata/tools.golden` is re-ended CRLF in a temp copy that the test reads.
