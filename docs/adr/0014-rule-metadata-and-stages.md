# 14. Rules declare their metadata at registration; preview stage and rule-set versions

Date: 2026-10-08. Status: accepted.

## Context

A rule's confidence, auto-fix, related rules, option defaults and fix dispatch lived in side tables keyed by id, none checked against the others (they drifted: `hardcoded-credential` graded `warning` in `DefaultSettings` but `error` in the rule). Phase 4a also needed a **preview** stage (visible, never gating) and a notion of which rules are **new** relative to a baseline.

## Decision

1. Each rule file's `init` calls `registerRule(r, RuleMeta{...})`: `Confidence` (required; no implicit Medium), `AutoFix`, `DirectFixes`, `DisabledByDefault`, `Options` (`OptionSpec{Key, Default, Min}`), `Related`, `IntroducedIn`, `Revision`, `Stage`. Readers: `LookupMeta(id)`, `MetaOf(r)`, `IsPreview`, `RuleEnabled`, `DefaultRuleConfigs`, `RelatedRules`. The `Rule` interface keeps ID, Name, Description, DefaultSeverity, Category.
2. `Stage` is `stable` or `preview`. A preview rule runs by default; its findings are stamped `Finding.Stage = "preview"` (stable is `omitempty`). Preview findings stay in the findings list and `Stats`, are left out of the health score, and never gate: not the `bakicli` exit code, the CI webhook verdict, policy evaluation, drift or regression alerts; SARIF reports them at `level: "note"`, JUnit as a passing testcase. An unknown rule or stage gates (fail closed); a stored finding without a stage whose rule is a preview built-in counts as preview.
3. An absent settings entry means the rule's own default: `RuleEnabled(r, s)` answers the entry when present, else `!DisabledByDefault`.
4. `RuleSetVersion` is an integer constant; a release adding a rule bumps it and sets `IntroducedIn`. `Revision` bumps when a change can move fingerprints or which findings a rule reports, not for a severity alone. A rule reference is `{ID, Rev, Stage}` (`Rev` empty at Revision 1, `r<N>` above; a custom rule's is its `MatchDigest`, ADR 0015). A legacy record counts as having seen built-ins with `IntroducedIn <= 1 && Revision <= 1` and every custom rule.
5. Fixes register beside their builders as package-variable initialisers (`registerFix(type, build, flags)`; `FixSpec` flags `FixBehaviourChanging`, `FixUses*`, `FixStandalone`). Fix types and rule IDs are stored data and are never renamed.
6. `DefaultSettings` stays a literal (pad-core cannot import the analyzer), mirrored by `TestDefaultSettings_MirrorsRuleMeta`; `hardcoded-credential` defaults to `error`. Completeness tests (`meta_test.go`, `registry_pin_test.go`) check stage/version/revision/confidence on every rule, that the highest `IntroducedIn` equals `RuleSetVersion`, fix coverage and honest flags, and pin every rule's `{IntroducedIn, Revision, Stage}` as a reviewed diff.

### Library rules

`LibraryRule` is a `Rule` plus `FactsVersion()`, `Extract(*RuleContext)` and `CheckLibrary(ctx, Library)`, registered with `registerLibraryRule`; never in `AllRules()` (per-flow walks, rule profiles, rule-set records and `RuleSetVersion` do not change). Library rules take no options. `ExtractFlowSummary` builds `FlowSummary` facts (JSON, at most `MaxLibraryFactsBytes` = 8 KiB per rule and flow; a rule that fails, panics or exceeds is listed in `Skipped`); `RunLibraryRules` reads at most `MaxLibraryFlows` = 10,000 flows (`Truncated`), each rule seeing only its own facts at its current `FactsVersion`. The runner owns identity: fingerprint `ruleID + ":" + hex(sha256(StableID \0 SubflowName \0 Anchor))[:16]`, findings sorted and folded, `Related` capped at 20, at most 10,000 findings per rule, ids `L-001` on. Library findings are a `models.LibraryReport`, never in per-flow reports, baselines, drift, policy or CI gates; library rules ship as preview. Reference rule: `duplicate-subflow-across-flows`.

## Consequences

Adding a rule is one file plus test-checked edits (the registry pins, the `DefaultSettings` literal, findings goldens). `bakicli` without a config now fails on hardcoded secrets under `-fail-on error`. A custom rule reusing a built-in's ID would take its stage and autoFix (saves reject such IDs). `SetRuleMetaForTesting` swaps metadata process-wide (not for parallel tests).
