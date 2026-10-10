# The analyzer

How `core/analyzer` turns a parsed `models.FlowDocument` into an `AnalysisReport`, and how to add a rule. Rule
metadata, stages and rule-set versions are ADR 0014; the custom-rule JSON is ADR 0015.

## Rule kinds (`core/analyzer/rule.go`)

Every rule implements `Rule`, the catalog identity: `ID`, `Name`, `Description`, `DefaultSeverity`, `Category`. The
engine dispatches a rule by its kind:

| Kind | Method | Called | Use it for |
|---|---|---|---|
| `BlockRule` | `Check(block, ctx)` | once per non-END block, in parallel shards | anything decided by one block and the shared facts (37 built-ins, every custom rule) |
| `FlowRule` | `CheckFlow(ctx)` | once per document, on the walk's calling goroutine while the shards run | a finding per subflow or per document (8 built-ins: circular-subflow-dependency, duplicate-subflow-name, empty-subflow, high-cyclomatic-complexity, large-subflow, parse-error, subflow-no-error-handler, uncalled-subflow) |

- A type that implements both is a `FlowRule`. A `Rule` that is neither (a test wrapper that embeds only the
  identity) is skipped, counted in `Stats.RulesSkipped` and logged once per rule ID. `TestAllRules_HaveAKind` pins
  the kind of every registered rule and the set of flow rules.
- A wrapper around a rule must embed the kind interface (`BlockRule`), not `Rule`, or it loses the method.
- `LibraryRule` (cross-flow, a separate registry, never part of a per-flow walk) is described in ADR 0014.

Flow-rule findings anchor on blocks of `ctx.Flow` (or are block-less). The 8 converted rules keep the anchors they
had as block rules, because the anchor block decides the finding's fingerprint, and fingerprints are stored in
baselines, triage state and comments. `subflowAnchor` (the subflow's first block) and `actionableAnchor` (its first
top-level block that is neither a comment nor END) return the block the old per-block dispatch fired on, or nil when
there is none: an empty subflow has no anchor and is not flagged.

## The walk (`core/analyzer/engine.go`)

`runAnalysisWalk` (behind `RunAnalysis*` and the analysis cache) builds the context and then:

1. **Enabled rules.** `RuleEnabled(r, settings)`: the settings entry when there is one, else the rule's
   `DisabledByDefault`. Rule order is the order of `rules`; for `AllRules()` that is file-name order (`init`
   order), pinned by `TestAllRules_OrderPinned`, and it decides finding order, `F-nnn` IDs and which fix the fix
   loop applies first.
2. **Severity overrides.** A configured severity overrides a rule's findings only when it differs from the rule's
   `DefaultSeverity()`. The seeded settings carry every rule's default, so the server (seeded settings) and
   `bakicli` without a config (nil settings) agree, and the severities a rule grades itself (parse-error from the
   parser, sql-injection-risk's taint escalation) survive (`TestDefaultSettings_MatchNilSettings`).
3. **Dispatch** (`collectFindings`, shared with `SuppressionInventory`). The non-END blocks in document pre-order
   are split into up to 8 contiguous shards (1 below 256 blocks). Each shard steps through every rule's slot per
   block and calls the block rules; meanwhile the calling goroutine runs each flow rule once and stable-sorts its
   findings into document order (block-less ones last). The order of findings on top-level blocks (every built-in
   flow rule's anchors) is read off the structural index; the `docPosition()` map is built only for findings on two
   or more blocks that anchor below the top level or arrive out of order. A flow rule still counts as one check per
   block for progress (`total = blocks x rules`) and `RuleProfile.BlocksChecked`. Panics are recovered per check
   into `RulesSkipped`. Cancellation is checked per block in the shards and before each flow rule; a cancelled walk
   returns `complete = false` and is never cached.
4. **Aggregation** is rule-major: for each enabled rule in order, its findings in document order. The engine fills
   the rule's category on findings that carry none.
5. **Parse-error fallback** (`parseErrorFallback`): a flow with parse errors whose first block is missing or END gets
   them block-less, whenever parse-error is enabled in settings, even if it is not in `rules` (the custom-rule
   tester shows them).
6. Inline `# pad-ignore` suppression, dedup (first finding per block, title and subject), stamping (`F-nnn`,
   content fingerprint, confidence, auto-fix, preview stage), stats, metrics from the context's subflow graph, and
   rule profiles.

`SuppressionInventory` runs the same dispatch without overrides or progress (`visitUnsuppressed`: findings are
handed over as they are produced, unordered and never collected into one list) and records each finding on the
block the finding names (`f.BlockID`), so it agrees with inline suppression, which matches on the same block.

## Facts (`core/analyzer/facts.go`)

`RuleContext` is what a rule sees. `buildContext` builds the structural indexes every walk needs: `AllBlocks`,
`ParentMap`, `SiblingMap`/`SiblingKey`, `BlockIndex`, `TerminatorIndex`, `BlockDepth`, `WritersByVar`/
`ReadersByVar`/`FirstReaderByVar`, `SubflowByID`/`SubflowByName`, `DescendantCount`, `GroupHasHandler`.

Everything else is a fact behind an accessor, built on first use under its own `sync.Once`:

| Accessor | Read by |
|---|---|
| `UsedVariables()` | unused-variable, data-flow dead paths |
| `blockSig(b)` (signature table) | duplicate-action |
| `HasErrorHandler()` (`HasErrorHandlerAncestor`) | file-op-no-error-handler, missing-retry, unhandled-error |
| `InsideRetryLoop()` | missing-retry |
| `AllReadersDead()` | dead-data |
| `LabelByName()`, `LabelNameCount()` (one group) | duplicate-label, goto-antipattern |
| `ClosedResourceVars()` | resource-leak |
| `TaintedVars()` | sql-injection-risk |
| `CircularSubflows()`, `SubflowCyclo()`, `SubflowFanIn()`, `DuplicateSubflowNames()` (the subflow graph) | the subflow flow rules, and the report's metrics on every walk |
| `docPosition()` | the engine, to order a flow rule's findings anchored below the top level (no built-in's) |

- A fact is a pure function of the document; the first reader builds it (other shards wait on the `Once`), and it
  is read-only afterwards. A walk whose rules never ask for a fact does not build it, so the custom-rule tester's
  single rule costs the structural index and the subflow graph only.
- `TestLazyFacts_MatchEagerBuild` compares every fact, built during a sharded walk, with an eager reference build
  over the corpus; `TestLazyFacts_NotBuiltWhenNoRuleNeedsThem` checks, rule by rule, that a walk builds only the
  facts in its `ruleFacts` row; `TestFactGroups_Complete` keeps the test's group list in step with the struct. Run
  them with `-race`.
- A new fact: an unexported `lazy[T]` field, an accessor that calls `get` with a builder taking the context, a row
  in `factGroups`, the eager reference in `lazy_facts_test.go`, and the readers in `ruleFacts`.
- `ctx.Err()` is the walk's cancellation, for a flow rule that loops over a large document.

## Adding a rule

1. A new file `core/analyzer/rule_<name>.go`. The file name decides the rule's position in `AllRules()` and so the
   order of every report: a new rule is a one-time order change, pinned by `TestAllRules_OrderPinned`.
2. The type with the identity methods and either `Check` (a block rule) or `CheckFlow` (a flow rule). Return
   findings with `RuleID`, `Severity` (normally `r.DefaultSeverity()`), `Title`, `Description`, `BlockID`,
   `SubflowID`, `Suggestion`; put the finding's subject in `Metadata` under `variable`, `property` or `resource`
   when one block can carry several findings of the rule (it is part of the fingerprint). Iterate maps in sorted
   key order when the choice reaches a finding (`sortedPropertyKeys`).
3. Block rules read only the context and the block, write nothing, and keep per-block work O(1) or O(block): the
   rule runs once per block, in parallel. A per-subflow or per-document finding is a flow rule, not a block rule
   that fires on one chosen block.
4. `init()` calls `registerRule(&YourRule{}, RuleMeta{...})` with confidence, auto-fix, options, related rules,
   `IntroducedIn: RuleSetVersion` (bump `RuleSetVersion` in the release that adds it) and the stage (a new rule
   can ship as `StagePreview`, which runs but never gates; ADR 0014). `models.DefaultSettings` gets the rule's
   entry (`TestDefaultSettings_MirrorsRuleMeta`).
5. Tests: unit tests on hand-built documents (for a flow rule, `findingsAt` keeps the per-anchor shape), the
   `expectedRules` pin in `registry_pin_test.go`, a `ruleFacts` row if it reads a lazy fact, and the findings
   goldens (`go test ./core/analyzer/ -run TestFindingsGolden -update-findings`; explain every changed line).

Changing what an existing rule reports (a new finding, another anchor or subject, another severity) changes stored
fingerprints or verdicts: bump the rule's `Revision` in its `RuleMeta` (ADR 0014) and refresh the goldens.

## Library rules (`core/analyzer/library.go`)

Cross-flow checks are `LibraryRule`s: a separate registry (`LibraryRules()`, never in `AllRules()`), an `Extract`
step that turns one flow into bounded JSON facts outside the per-flow walk, and a `CheckLibrary` step over all
flows' facts. Library findings live in a `models.LibraryReport` and never enter per-flow reports, caches,
baselines or gates. The design and the reference rule `duplicate-subflow-across-flows` are in ADR 0014, "Library
rules".
