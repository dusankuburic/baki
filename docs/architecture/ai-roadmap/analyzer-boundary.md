# AI roadmap: Analyzer–assistant boundary

Items of category `analyzer-boundary` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

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
