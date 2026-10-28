# AI roadmap: Testing and evaluation

Items of category `testing` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

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
