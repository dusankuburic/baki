# AI roadmap: Analyzer–assistant boundary

Items of category `analyzer-boundary` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-analyzer-boundary-3

**Context preview is not what is sent: findings omitted, history and tool schemas not counted, different cache entry** · tier 1

**Problem.** The "Preview context" modal is meant to show what the model receives and whether it fits the context window. It differs from the send in three ways:
- **Findings.** The handler calls `PreviewContext` with a nil report, whereas `StreamChatMessage` substitutes the cached analysis report when the report is nil. The preview therefore never shows the `## Analysis Summary` section, which `BuildContext` writes first into the real context.
- **Token estimate.** The preview counts only systemPrompt + contextText + userMessage. The send's ceiling and fixed component count every history message and every tool schema. On a long thread with tools, the modal under-reports and its over-limit and near-limit warnings are wrong.
- **Cache entry.** When a cached report exists, its fingerprint is part of the `chat.context` cache key, so the preview warms a different cache entry than the send that follows it.

**Evidence.**
- `internal/api/handlers_chat.go:513` — `PreviewContext(..., doc, nil, req)`: the report is nil
- `internal/service/chat.go:681` — `StreamChatMessage` substitutes `analysisCache.CurrentReport(doc)` when the report is nil (681-684)
- `internal/ai/context.go:60` — findings are written first into the real context (58-63)
- `internal/service/chat_context.go:639` — the estimate is `sysPrompt + contextText + scrubbedUserMsg` only
- `internal/service/chat_context.go:430` — the send's ceiling counts tool schemas and all messages (430-443); the fixed component includes tools (504-508)
- `internal/service/chat_context.go:119` — `reportFP` is part of the cache key (119-128), so with a cached report the preview and the send use different entries

**Change.** Make `PreviewContext` build the same `ai.Request` the stream would:
- Resolve the cached report exactly as `StreamChatMessage` does.
- When `req.Messages` is empty, rebuild the stored conversation on the server as the send does (`chat.go:861-867`); the client's request messages are not enough.
- Run `buildScopedMessages` with that history, which adds the "Context:\n" framing (`chat_context.go:399-401`), and `ToolDefinitionsFor(gate)`. The prompt loop's tool instructions are appended to the system prompt (`chat.go:1386-1394`); include them too.
- Return a breakdown (system, context, history, tools, user), whether tools are offered, and the effective context budget.

Let the handler pass the cached report so the preview and the send share a cache entry.

**Touches.** `internal/api/handlers_chat.go`, `internal/service/chat_context.go`, `core/models/chat.go`, `frontend/src/components/chat/ContextPreviewModal.tsx`, `frontend/src/components/chat/hooks/useAIChat.ts`, `internal/service/chat_preview_policy_test.go`

**Done when.**
- Service test: with a cached report, `PreviewContext.ContextText` contains `## Analysis Summary`
- Service test: `EstimatedTokens` is at least the tokens of system + context + every forwarded history message + tool schemas
- The preview and the following stream hit the same `chat.context` cache key (cache hit counter)

**ADR.** `chat_context.go:80` cites ADR 0017 for the context caches; that ADR does not cover parity between preview and send.

### R-analyzer-boundary-4

**Variable lineage is exact-match on the bare name, and the selected block's Variable History section is dead code** · tier 1

**Problem.** `get_variable_lineage` passes `name` through unchanged. `BuildVariableLineage` compares it with `==` against the parser's bare identifiers, which are extracted from `%…%` with the percent signs stripped. Property values and Source Files spell references as `%Name%`. When the model calls the tool with that spelling, or with a different case, it gets "No lineage found" with no hint and no candidates. The model also sees bare names in some places ("Variables referenced", the `get_block` "variables:" line), so the `%Name%` risk comes mainly from property values and source text.

Separately, `ContextRequest.VariableEvents` and the `## Variable History` section exist in `BuildContext`, but nothing ever fills that field. The only production caller, `computeContextCoreWith`, builds the `ContextRequest` without it, and a search of the whole repository finds no other writer. The lineage the tool computes is therefore never offered in the first turn, even when the user selected the block whose variables are in question.

**Evidence.**
- `internal/ai/tools.go:570` — `GetVariableLineage(tctx.Doc, in.Name)` with the raw input; "No lineage found" at 572 with no hint
- `core/analyzer/engine.go:723` — `output == varName` exact compare; line 740 has `v == varName` for reads
- `core/parser/grammar.go:46` — `reVariableRef = %([^%]+)%`: names are stored without the percent signs
- `core/parser/extractors.go:184` — `extractVariables` yields bare identifiers (184-209)
- `internal/ai/context.go:34` — the `VariableEvents` field; `## Variable History` is rendered at 133-141 only when the field is non-empty
- `internal/service/chat_context.go:347` — the `ContextRequest` is built without `VariableEvents` (347-352); there is no other writer in the Go tree
- `core/analyzer/dataflow.go:198` — the analyzer's own variable indexes match case exactly (198-202; `rule_uninitialized_variable.go:118-122`)
- `internal/ai/context.go:218` — the model already sees bare names in "Variables referenced" (218-220) and in `get_block` "variables:" (`tools.go:436-437`)

**Change.** Normalise the tool input: always trim whitespace and strip surrounding `%`. Fall back to a case-insensitive match against `_output` and `Variables` only on a miss, or offer it as a candidate list. Lowercasing all input would disagree with every other variable rule, which matches case exactly. On a miss, list up to five near matches from the document. Then either fill `VariableEvents` or remove the field and its section. To fill it, when a block is selected, take the lineage of the block's `_output` and referenced variables from `AnalysisService.GetVariableLineage`, bounded (for example 5 variables × 10 events).

**Touches.** `internal/ai/tools.go`, `core/analyzer/engine.go`, `internal/service/chat_context.go`, `internal/ai/context.go`, `internal/ai/tools_test.go`, `internal/ai/context_test.go`, `internal/service/chat_context_test.go`

**Done when.**
- `get_variable_lineage('%Result%')` and `('Result')` return the same events, and `('result')` reaches them through the case-insensitive fallback or candidate list
- A miss returns up to five candidate names from the document
- Selecting a block with an output variable renders `## Variable History` (bounded by the cap) in `PreviewContext`, or the field and the section are gone

### R-analyzer-boundary-5

**Model context lacks rule-level facts (ruleId, finding key, fix type, stage) and there is no way to explain a rule** · tier 2

**Problem.** The Analysis Summary groups findings by Title with one suggestion per title. It shows no ruleId, finding key, stage, confidence or fix type, and its lines follow map iteration order. Only findings on the selected block carry a finding key, description and metadata. Even there, the key is the Fingerprint when one exists, so the ruleId that `propose_fix` needs is not shown. The system prompt asks the model to emit `finding:` links "only for finding ids present in the context". Without tools, the model therefore cannot link any finding outside the selected block or name the auto-fix. With tools, it must spend one of six iterations on `list_findings` before `propose_fix`. `AutoFixHint` reaches the model only through the client-staged "Fix with AI" prompt (`frontend/src/lib/fixWithAI.ts:10`). The rule catalog that `GetRulesFor` serves to the UI holds each rule's description, stage, kind and custom-rule origin (`/api/analysis/rules`, `internal/api/analysis_rules.go:18-31`). No tool makes it available to the model.

**Evidence.**
- `internal/ai/context.go:321` — `writeFindingsSummary` counts by Title and prints one suggestion per title (321-361); the order follows map iteration (351)
- `internal/ai/context.go:93` — finding key, description and metadata only for the selected block (93-105); the key is the Fingerprint when present (194-198)
- `internal/ai/context.go:24` — prompt: "Only use finding ids present in the context"
- `internal/ai/tools.go:520` — `list_findings` is the only surface for ruleId, key, fix and stage (520-531)
- `internal/service/chat.go:42` — `maxToolIterations = 6`
- `internal/service/analysis.go:803` — `GetRulesFor(ctx, orgID)` exists on `AnalysisService` but is not on the `ToolAnalysis` interface

**Change.** Rewrite the summary to group by ruleId, one line per rule: `- <ruleId> (<severity>, <count>, stage, fix=<type>): <rule description>`. Follow each line with up to N finding keys with block id and line, sorted deterministically, replacing today's map-iteration order. Include `AutoFixHint` for selected-block findings. Add a `get_rule` tool, with a prompt-loop entry, that returns the catalog entry: description, why it matters, fix types and flags, options and custom-rule origin. Back it with a new `ToolAnalysis` method that calls `AnalysisService.GetRulesFor`.

**Touches.** `internal/ai/context.go`, `internal/ai/tools.go`, `internal/ai/prompt_tools.go`, `internal/service/chat.go`, `internal/service/analysis.go`, `internal/ai/context_test.go`, `internal/ai/testdata/tools.golden`

**Done when.**
- `context_test`: for a rule whose findings are not on the selected block, the summary lines contain the ruleId, count, fix type and at least one finding key.
- A turn without tools can produce `finding:<key>` links for non-selected blocks from the context alone.
- The `get_rule` tool spec is in the tools golden, and the tool returns an org custom rule's description for that rule's id.

### R-analyzer-boundary-6

**No subflow call-graph tool although the analyzer computes callers, callees and RunDesktopFlow targets** · tier 2

**Problem.** `BuildExecutionGraph` resolves Call/RunSubflow targets through a name index and counts calls and disabled calls per edge. The flow rules UncalledSubflow and CircularSubflow use `buildCallGraph`, which shares the same resolver (`core/analyzer/metrics.go:162-180`, via `core/analyzer/facts.go:468`). The parser tags RunSubflow and RunDesktopFlow targets as subflow tokens, but the analyzer does not compute desktop-flow targets. RunDesktopFlow is classified `BlockTypeSubflow` (`core/parser/classifier.go:78`), so `isSubflowCall` is true. `resolveCallTargetID` then looks the DesktopFlow name up in the local subflow index (`core/analyzer/graph.go:38-53`). It drops the name, or links it to a local subflow that happens to share it. The chat's `ToolAnalysis` interface exposes only `AnalyzeFlow` and `GetVariableLineage`; `AnalysisService.GetExecutionGraph` is not on it. Chat therefore cannot answer "who calls this subflow", "what does Main call" or "which desktop flows does this flow run" without scanning `search_flow` results for "Call ". Reachability is partly answerable today: `list_findings` lists uncalled-subflow and circular-subflow findings with their subflow ids.

**Evidence.**
- `internal/ai/tools.go:40` — `ToolAnalysis`: `AnalyzeFlow` and `GetVariableLineage` only (40-43)
- `core/analyzer/graph.go:88` — edges with `CallCount`/`DisabledCount` per `sf.ID -> targetID` (88-110)
- `core/analyzer/graph.go:24` — `isSubflowCall` and `resolveCallTargetID` hold the call heuristics in one place (24-54)
- `core/parser/parser.go:582` — `FlowControl.RunSubflow` and `RunDesktopFlow` targets become subflow tokens (582-593)
- `internal/service/analysis.go:710` — `GetExecutionGraph` exists on `AnalysisService` but is not on the tool interface
- `core/analyzer/registry.go:12` — `UncalledSubflowRule` and `CircularSubflowRule` are flow rules (12-19)

**Change.** Add `get_subflow_calls(subflow id or name)`, backed by `BuildExecutionGraph` through a new `ToolAnalysis` method. It returns callers and callees with call and disabled counts. It also lists RunDesktopFlow targets. Those need their own scan of `Properties["DesktopFlow"]` or the subflow tokens, because `BuildExecutionGraph` drops or mislinks them. Append a compact "Calls" line per subflow in the Flow Overview when it fits the budget.

**Touches.** `internal/ai/tools.go`, `internal/ai/testdata/tools.golden`, `internal/service/chat.go`, `internal/ai/context.go`, `internal/ai/tools_test.go`

**Done when.**
- For Main calling A twice (once disabled), `get_subflow_calls('A')` reports Main as a caller with count 2 and disabled 1.
- An uncalled subflow reports zero callers.
- RunDesktopFlow targets are listed by name.
