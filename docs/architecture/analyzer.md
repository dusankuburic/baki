# The analyzer

How `core/analyzer` turns a parsed `models.FlowDocument` into an `AnalysisReport`, and how to add a rule. Rule metadata, stages and rule-set versions are ADR 0014; the custom-rule JSON is ADR 0015.

## Rule kinds (`core/analyzer/rule.go`)

Every rule implements `Rule` (the catalog identity: `ID`, `Name`, `Description`, `DefaultSeverity`, `Category`) plus a kind:

| Kind | Method | Called | Use it for |
|---|---|---|---|
| `BlockRule` | `Check(block, ctx)` | once per non-END block, in parallel shards | anything decided by one block and the shared facts |
| `FlowRule` | `CheckFlow(ctx)` | once per document, on the walk's calling goroutine | a finding per subflow or per document |

A type implementing both is a `FlowRule`; a `Rule` that is neither is skipped (counted in `Stats.RulesSkipped`, `TestAllRules_HaveAKind`); a wrapper must embed the kind interface (`BlockRule`), not `Rule`, or it loses the method. Flow-rule findings anchor on blocks of `ctx.Flow` or are block-less; the converted rules keep the anchors they had as block rules (`subflowAnchor`, `actionableAnchor`), because the anchor decides the finding's fingerprint, stored in baselines, triage state and comments. `LibraryRule` (cross-flow) is a separate registry, never part of a per-flow walk.

## The walk (`core/analyzer/engine.go`)

1. **Enabled rules** — `RuleEnabled(r, settings)`: the settings entry when there is one, else `DisabledByDefault`. Rule order is file-name order (`TestAllRules_OrderPinned`); it decides finding order, `F-nnn` ids and which fix the fix loop applies first.
2. **Severity overrides** apply only when they differ from `DefaultSeverity()`; a rule-graded severity (parse-error from the parser, sql-injection-risk's taint escalation) survives.
3. **Dispatch**: the non-END blocks in document pre-order split into up to 8 contiguous shards (1 below 256 blocks); each shard runs every rule's block rules while the calling goroutine runs each flow rule once and stable-sorts its findings into document order (block-less last). A flow rule counts one check per block for progress (`total = blocks × rules`); panics are recovered into `RulesSkipped`; cancellation is checked per block and before each flow rule — a cancelled walk returns `complete = false` and is never cached.
4. **Aggregation** is rule-major: each enabled rule's findings in document order; the engine fills the category on findings that carry none.
5. **Parse-error fallback**: a flow with parse errors whose first block is missing or END gets them block-less, with parse-error's configured severity and category, whenever that rule is enabled in settings — even outside `rules`.
6. Inline `# pad-ignore` suppression, dedup (first finding per block, title and subject), stamping (`F-nnn`, fingerprint, confidence, auto-fix, preview stage), stats, metrics and rule profiles. `SuppressionInventory` runs the same dispatch and records each finding on the block it names, so it agrees with inline suppression.

## Facts (`core/analyzer/facts.go`)

`RuleContext` builds the structural indexes eagerly (`AllBlocks`, `ParentMap`, `SiblingMap`/`SiblingKey`, `BlockIndex`, `TerminatorIndex`, `BlockDepth`, `WritersByVar`/`ReadersByVar`/`FirstReaderByVar`, `SubflowByID`/`ByName`, `DescendantCount`, `GroupHasHandler`); everything else is a fact behind an accessor, built on first use under its own `sync.Once` (`UsedVariables`, `blockSig`, `HasErrorHandler`, `InsideRetryLoop`, `AllReadersDead`, `LabelByName`, `ClosedResourceVars`, `TaintedVars`, the subflow graph, `docPosition`). A fact is a pure function of the document, read-only after the first build; a walk whose rules never ask never builds it (`TestLazyFacts_MatchEagerBuild`, `TestLazyFacts_NotBuiltWhenNoRuleNeedsThem`, run with `-race`). A new fact: an unexported `lazy[T]` field, an accessor calling `get` with a builder, a row in `factGroups`, the eager reference in `lazy_facts_test.go`, readers in `ruleFacts`. With duplicate ids (which the parser never emits) a rule reading neighbours asks `siblingPosition`, which refuses a block the indexes do not hold. `ctx.Err()` is the walk's cancellation.

## Adding a rule

1. A new file `core/analyzer/rule_<name>.go` — the file name decides the rule's position in `AllRules()` and so the order of every report (a one-time order change, pinned).
2. The type with the identity methods and `Check` or `CheckFlow`; findings carry `RuleID`, `Severity` (normally `DefaultSeverity()`), `Title`, `Description`, `BlockID`, `SubflowID`, `Suggestion`, and the subject in `Metadata` (`variable`/`property`/`resource`) when one block can carry several findings of the rule (it is part of the fingerprint); iterate maps in sorted key order.
3. Block rules read only the block and context, write nothing, and stay O(1)/O(block) per block — they run in parallel; a per-subflow or per-document finding is a flow rule.
4. `init()` calls `registerRule(&YourRule{}, RuleMeta{…})` with confidence, auto-fix, options, `IntroducedIn: RuleSetVersion` (bump `RuleSetVersion` in the release that adds it) and the stage (a new rule may ship as `StagePreview`, which runs but never gates); add the rule's entry to `models.DefaultSettings` (`TestDefaultSettings_MirrorsRuleMeta`) and the `registry_pin_test.go` pin.
5. Unit tests on hand-built documents, a `ruleFacts` row if it reads a lazy fact, and the findings goldens: `go test ./core/analyzer/ -run TestFindingsGolden -update-findings` — explain every changed line.

Changing what an existing rule reports (a new finding, anchor or subject, another severity) changes stored fingerprints or verdicts: bump the rule's `Revision` in its `RuleMeta` and refresh the goldens.

## Library rules (`core/analyzer/library.go`)

Cross-flow checks are `LibraryRule`s in a separate registry (`LibraryRules()`, never in `AllRules()`): an `Extract` step turns one flow into bounded JSON facts, a `CheckLibrary` step checks across all flows' facts. Library findings live in a `models.LibraryReport` and never enter per-flow reports, caches, baselines or gates (the reference rule is `duplicate-subflow-across-flows`, preview; shown by the desktop's folder batch as "Across flows").
