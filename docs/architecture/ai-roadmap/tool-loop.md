# AI roadmap: Tool loop

Items of category `tool-loop` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

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
