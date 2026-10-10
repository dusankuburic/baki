# 14. Rules declare their metadata at registration; preview stage and rule-set versions

Date: 2026-10-08. Status: accepted.

## Context

A built-in rule was a `Rule` (ID, name, description, default severity, category, `Check`) plus entries in side
tables keyed by its ID, none of them checked against the others:

- in the engine: confidence (19 rules listed, the other 26 Medium by omission), the engine-stamped auto-fix (31),
  and the one rule that stamps its fix in `Check` (`subflow-mismatch`);
- the editorial related-rules list behind `GET /api/analysis/dependencies` (9 pairs, `incremental.go`);
- the fix dispatch: a 19-case `PatchForFix` switch, a behaviour-changing set beside it, and `isValidFixType`, a
  hand-kept copy of the switch for custom rules;
- option defaults as literals in four rules' `Check`, and `models.DefaultSettings()`'s 45-entry rule map.

Adding a rule touched up to six places. The copies had already drifted once: `hardcoded-credential` was `warning`
in the rule and `error` in `DefaultSettings`, so `bakicli` without a config (nil settings) and the server graded the
same finding differently.

Phase 4a also needs two things the catalog could not say: that a rule is a **preview** (visible, never gating), and
which rules are **new** relative to a recorded baseline. The repository has no release tags, deploy versions are
commit SHAs, and the CLI and the server are built separately from the same `core` module.

## Decision

1. **Metadata is declared where the rule is.** Each rule file's `init` calls `registerRule(r, RuleMeta{...})`:
   `Confidence` (required; no implicit Medium), `AutoFix` (stamped by the engine), `DirectFixes` (stamped by
   `Check`), `DisabledByDefault`, `Options` (`OptionSpec{Key, Default, Min}`, the same value the rule reads through
   `ruleOption`), `Related`, `IntroducedIn`, `Revision`, `Stage`. The side tables are gone. Readers:
   `LookupMeta(id)`, `MetaOf(r)` (a custom rule's own `Meta()`, else the built-in by `r.ID()`, so test wrappers that
   embed a rule keep its metadata, else Medium and stable), `IsPreview`, `RuleEnabled`, `DefaultRuleConfigs`,
   `RelatedRules`, and the existing `RuleConfidence` / `RuleAutoFix`. The engine stamps findings by `RuleID` with
   one table lookup, as before.
2. **The `Rule` interface keeps ID, Name, Description, DefaultSeverity and Category.** Seven test fakes and wrappers
   and `CustomRule` implement it, and the rule-kinds work of this phase reshapes the same interface. A `Meta()`
   method would invite per-instance metadata, and moving the four methods into `RuleMeta` would touch every rule
   and fake without changing behaviour. Default severity stays in one place, the method; `RuleMeta` can absorb the
   rest later.
3. **Stages.** `Stage` is `stable` or `preview`. A preview rule runs by default (unless settings disable it). Its
   findings are stamped `Finding.Stage = "preview"`; a stable finding has no stage (`omitempty`), so stable output
   stays byte-identical. Preview findings stay in the findings list and in `Stats`, are left out of the health
   score, and never gate: not the `bakicli` exit code, not the CI webhook verdict, not policy evaluation (no
   opt-in), never a drift or regression alert. SARIF reports them at `level: "note"` with `properties.stage`, JUnit
   as a testcase without `<failure>`. A finding without a stage whose rule ID is a preview built-in (older stored
   JSON) counts as preview; anything else gates, so an unknown rule fails closed.
4. **An absent settings entry means the rule's own default.** Every persisted rule map (deployment, user, org, the
   desktop file) was seeded from `DefaultSettings()` and is never rewritten, so a rule shipped later is absent from
   it. `RuleEnabled(r, s)` answers the settings entry when there is one, else `!DisabledByDefault`; the engine, the
   suppression inventory and the parse-error fallback use it.
5. **Rule-set versions are integers in `core`.** `RuleSetVersion` is a constant (1); a release that adds a built-in
   rule bumps it and gives the rule `IntroducedIn` = the new value. Every rule shipped before Phase 4a is version 1:
   no baseline recorded which rules it saw, so backfilling older numbers from history would claim a precision we do
   not have. `Revision` is bumped when a change can move fingerprints or which findings a rule reports, not for a
   severity alone (a revised rule's new findings are a notice against older baselines, not drift); the Phase 4a
   determinism fixes put eight rules at Revision 2 (`circular-subflow-dependency` and the seven property rules whose
   fingerprint can change). `hardcoded-credential` only reorders findings inside a block and `parse-error` only
   keeps its parser-graded severities under settings, so both stay at 1. A
   rule reference is `{ID, Rev, Stage}`: `Rev` is empty at Revision 1 and `r<N>` above it; a custom rule's is its
   match digest (ADR 0015). A rule is new to a recorded set when it is absent from it, revised, or promoted from
   preview. A legacy record (none, or made stale by the previous release) counts as having seen the built-ins with
   `IntroducedIn <= 1 && Revision <= 1` and every custom rule (ADR 0016). App versions were rejected (no tags, SHAs
   do not order, the two binaries are stamped separately), dates too (not reviewable as a set), and a digest alone
   says that the set changed but not which rules are new.
6. **Fixes register next to their builders.** `autofix.go` registers each fix type beside its builder as a
   package-variable initialiser (`registerFix(type, build, flags)`), so the registry is complete before any `init`
   registers a rule. `FixSpec{Type, Build, Flags}` carries `FixBehaviourChanging` (the opt-in set), `FixUses*` (which
   finding metadata the builder reads) and `FixStandalone` (`suppress`, offered for any finding). `PatchForFix`,
   `IsBehaviourChangingFix` and custom-rule validation read the registry; `LookupFix` and `FixTypes` are exported,
   registration is not: no package outside the analyzer needs to add a fix, and a registration after `init` would
   race the readers. Fix types, like rule IDs, are stored data and are never renamed.
7. **`DefaultSettings` stays a literal, checked as a mirror.** `core/models` cannot import the analyzer, and an
   init-time hook would make the defaults depend on which packages a binary links. `TestDefaultSettings_MirrorsRuleMeta`
   asserts `DefaultSettings().Analysis.Rules == DefaultRuleConfigs()`. The one mismatch is resolved toward what a
   server with seeded settings (desktop, local, any deployment that saved its settings) already showed:
   `hardcoded-credential` defaults to `error`.
8. **Completeness is tested, not checked at init.** `meta_test.go` and `registry_pin_test.go` check: stage, version,
   revision and confidence on every rule; the highest `IntroducedIn` equals `RuleSetVersion`; every referenced fix
   is registered and every registered fix is referenced or standalone; every fix type has a `TestPatchForFix` case
   and honest `FixUses*` flags; related rules name distinct, registered rules with a reason and stay acyclic;
   option keys are unique with `Default >= Min`; every finding over the sample corpus carries a declared fix;
   categories stay in the known seven; and a pin of every rule's `{IntroducedIn, Revision, Stage}`, so a new rule,
   a revision bump or a promotion is a reviewed diff.

## Consequences

- Adding a rule is one file plus test-checked edits: the pins in `registry_pin_test.go` and
  `registry_order_test.go`, the `DefaultSettings` literal and the findings goldens
  ([analyzer.md](../architecture/analyzer.md), "Adding a rule").
- `bakicli` without a config now reports `hardcoded-credential` as an error, so the default `-fail-on error` fails
  on hardcoded secrets, as the server's gates already did where its settings were seeded. A cloud deployment that
  never saved its settings served an empty rule map, so its gates graded the finding `warning`: there the CI
  webhook (`failOn=error`), policies with an error gate, drift error counts and health now count it as an error.
  Its health snapshots from before the upgrade are not compared (ADR 0016), so the change raises no regression.
- `/api/analysis/dependencies` lists the related rules sorted by (from, to) instead of in table order.
- `SetRuleMetaForTesting` swaps metadata process-wide (not for parallel tests). Cached reports need no reset: the
  analyzer cache's rule-profile part (`ProfileDigest`, ADR 0017) folds each rule's stage and revision, so a flip
  misses.
- Metadata is looked up by `RuleID`, so a custom rule reusing a built-in's ID would take its stage (and never
  gate under a preview ID) and its autoFix. The engine stamps such a rule's findings `Stage = "stable"`, which
  stamping and `Gates` trust over the ID; saves reject such IDs (ADR 0015) and reads report them.
- Every reader of a rule's enabled state goes through `RuleEnabled`: besides the engine, the inventory and the
  parse-error fallback, the library runner and the rule catalog (`GET /api/analysis/rules`, whose option defaults
  come from `DefaultRuleConfigs`). The analyzer cache key and the scanner's rule-set digest go through `ProfileDigest`, which
  folds `RuleEnabled` (ADR 0017).

## Library rules

Date: 2026-10-08 (Phase 4a, wave 2). Status: accepted.

Some problems show only across flows (the same subflow copied into several flows). A per-flow rule cannot see
them, and putting cross-flow results into per-flow reports would make a report depend on other flows: its cache
identity, baselines and drift would go stale whenever another flow changed.

1. **A separate kind with its own registry.** `LibraryRule` is a `Rule` plus `FactsVersion()`,
   `Extract(*RuleContext) (json.RawMessage, error)` and `CheckLibrary(ctx, Library) ([]models.LibraryFinding,
   error)`. `registerLibraryRule(r, meta)` fills `LibraryRules()` and the same metadata table, so `LookupMeta`,
   `MetaOf`, `IsPreview` and `RuleEnabled` answer for library rules. They are never in `AllRules()`, so the per-flow
   walk, rule profiles, rule-set records (ADR 0016) and `RuleSetVersion` do not change when one is added. The
   catalog lists them (kind `library`) only where they run: `bakicli rules`, and the server's catalog in a
   deployment without accounts, whose folder batch runs them. A library rule takes no options, because facts that
   depended on settings would go stale once stored; `Enabled` and `Severity` come from the settings entry under its
   ID.
2. **Two phases, documents read once.** `ExtractFlowSummary(ctx, doc, settings, rules)` runs each enabled rule's
   `Extract` outside the per-flow walk, on a `RuleContext` of its own, and returns a `models.FlowSummary` (FlowID,
   StableID, FlowName, ContentHash, facts by rule ID at the rule's `FactsVersion`). Facts are JSON, at most
   `MaxLibraryFactsBytes` (8 KiB) per rule and flow; a rule that fails, panics, returns invalid JSON or too much is
   listed in `Skipped` and the others still run. `RunLibraryRules(ctx, rules, lib, settings)` then runs the enabled
   rules over a `Library` (`Len`, `Each` in FlowID order). Each rule sees only its own facts, and only at its
   current `FactsVersion` (stale stored facts are invisible, not misread). Panics and errors are recovered per
   rule (`RulesSkipped`), the context is checked between rules, and the pass reads at most `MaxLibraryFlows`
   (10,000) flows (`Truncated`). `AnalyzeLibrary(ctx, docs, rules, settings)` does both for documents in memory.
3. **The runner owns identity.** It sets the rule ID, the default severity, category, confidence and stage, and
   the fingerprint `ruleID + ":" + hex(sha256(StableID \0 SubflowName \0 Anchor \0))[:16]`, where `Anchor` is a
   content key the rule supplies (never a parser ID). Findings are sorted (rule order, flow name, StableID, subflow
   name, fingerprint), equal fingerprints fold, `Related` is sorted and capped at 20, a rule keeps at most 10,000
   findings, and IDs run `L-001` on. A finding naming a flow the rule did not read is dropped. A configured
   severity overrides a finding only when it differs from the rule's default (as for per-flow rules, D13).
4. **Library findings stay out of everything per-flow.** They are a `models.LibraryReport`, never in
   `AnalysisReport.Findings`, the report caches, baselines, drift, policy or CI gates. Library rules ship as
   preview. The desktop batch returns the report as the additive `BatchAnalysis.library`.
5. **Reference rule: `duplicate-subflow-across-flows`** (preview, info, Style, High confidence). Extract records
   each non-trivial subflow's body hash: SHA-256 (128 bits kept) over its blocks in pre-order, each with its depth,
   type, raw type, name and sorted properties, leaving out comments, END blocks and disabled blocks, IDs, line
   numbers and the subflow's name. A subflow is trivial with fewer than 3 such blocks. Over the 8 KiB cap the
   largest subflows are kept. Check reports every copy in a group that spans two or more flows (copies inside one
   flow alone are not reported); the anchor separates same-named copies inside one flow. Changing the hash needs a
   `FactsVersion` bump; the hash and the fingerprint formula are pinned in tests.

Consequences: no cloud persistence or route in 4a; a stored-facts column, a paged `Library` over it and an
insights endpoint can come later without changing the interface. Extraction builds a `RuleContext`
whose rule-only facts are lazy, so only the structural index and the facts a library rule reads are built.
