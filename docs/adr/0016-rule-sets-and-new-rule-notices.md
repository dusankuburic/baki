# 16. Analyses record their rule set; new rules give a notice, preview rules never gate

Date: 2026-10-08. Status: accepted. The core half (Phase 4a wave 2): the gating predicate, the rule-set record and the
comparisons (1-7). Wave 3: storage (migration 0037), the scanner, the notices and the bell (8-13), and the CLI
baseline file.

## Context

A baseline stores finding keys only (`flow_baselines.keys`), and a health snapshot only its score. When a release
adds or revises a rule, every finding it produces is "new since baseline": the scanner raises a drift alert per
baselined flow (each one fanned out to the inbox, SSE and every channel, a Jira bug per flow included) and a health
regression per flow with a snapshot, and repeats them after a restart. Nothing recorded which rules a baseline saw,
so a new rule cannot be told from new code. The same is true of org rule toggles and custom-rule edits.

ADR 0014 adds rule metadata: a stage (`stable`, `preview`), the rule-set version a rule shipped in (`IntroducedIn`)
and a `Revision`. Phase 4a also decided that a preview rule runs and reports by default but never gates (D3).

## Decision

1. **One gating predicate.** `analyzer.Gates(f)` is false only for a finding known to come from a preview rule:
   stamped `Finding.Stage = "preview"`, or (whatever its stamp) of a rule that is a preview built-in in this binary,
   which covers JSON saved before stages. Anything else gates, an unknown stage or an unregistered rule included
   (fail closed). Every gate uses it: `EvaluatePolicy` sets preview findings of its rules aside and counts them in
   `PolicyResult.Preview` (`omitempty`; never evaluated, waived or a violation; a policy cannot opt in), the health
   score leaves them out, SARIF reports them at `level: "note"` with `properties.stage: "preview"` (the rule's
   `defaultConfiguration` follows the effective level), and JUnit as a passing testcase whose `<system-out>` says
   why. `GatingFindings` and `CountGating` serve the bakicli exit code and the CI webhook verdict. Preview findings
   stay in the findings list and in `Stats`: dashboards and stored counts keep their meaning.
2. **A rule reference is `{ID, Rev, Stage}`** (`models.RuleRef`). A built-in's `Rev` is empty at Revision 1 and
   `r<N>` at Revision N > 1; a custom rule's is its `MatchDigest()` (ADR 0015: what it matches and fixes, not its
   name, description, severity or category). `Stage` is `preview` or empty. Severities and options are not part of
   a reference: they change what a finding weighs, not which rules ran.
3. **A rule set is a sorted, de-duplicated list of references with a digest** (`models.RuleSet{Rules, Digest}`,
   `analyzer.NewRuleSet`). The digest is persisted data: `"rs1:"` + hex SHA-256 over every reference in order, each
   of its three fields written as its decimal byte length, `:` and the bytes. The empty set is
   `rs1:e3b0c442…b855`; `TestRuleSetDigest_Pinned` pins independently derived values. A new encoding needs a new
   prefix.
4. **The engine records the rules it ran.** `runAnalysisWalk` stamps `AnalysisReport.RuleSet =
   RecordRuleSet(enabledRules)` before the report is cached, so a cached report carries the record of the walk that
   produced it. It is not serialized (`json:"-"`) and, like the report, never mutated. The parse-error fallback can
   emit findings of a rule that is not in the list; such findings are treated as drift (5).
5. **"New" is set-based.** `NewRuleAge(recorded, current)` marks a rule new when its current reference is not in the
   recorded set: an added rule (a release or an org toggle), a revision bump, a custom rule whose matcher changed, a
   promotion from preview. A record with the current digest saw every current rule, even when only its digest was
   read. A record without a usable rule set (none, rewritten by the previous release, or one whose rules do not hash
   to its digest: missing, partial, or another digest prefix) is **legacy**: it saw every custom rule, every rule
   that is not a built-in, and the built-ins with `IntroducedIn <= 1 && Revision <= 1` (`KnownToLegacyRecords`).
   So after Phase 4a the eight rules revised by the determinism fixes are new to legacy baselines: one notice
   instead of a drift flood. Toward drift, the status quo, wherever the answer is unclear: a finding whose rule the
   current set does not name, a report without a rule set, a recorded set that does not match its digest, and an
   ID with two references (a custom rule reusing a built-in's ID) unless both are new. A batch read may load only
   the digest: equal to the current one it answers "nothing new"; otherwise the caller must load the rules, or the
   record reads as legacy.
6. **Drift is split, not redefined.** `ComputeDrift` is unchanged: `New` is every finding not in the baseline, which
   is what the toolbar badge, the CLI ratchet and the tests mean. `SplitDrift(new, age)` partitions it into
   `Drift` (alerts), `NewRule` (one notice per scope and rule-set digest, wave 3) and `Preview` (nothing), with the
   sorted `NewRuleIDs`. `BaselineDrift` gains `newRuleIds`, `newFromNewRules` and `preview` (`omitempty`), filled by
   the server's drift endpoint through `DriftSplit.Annotate`.
7. **Regression is like-for-like.** `HealthOf(findings, metrics)` is the formula behind `FlowMetrics.HealthScore`
   (`HealthOf(r.Findings, r.Metrics)` equals it, tested over the corpus), so the scanner compares a snapshot with the
   score of the findings without those of the rules the snapshot's score did not count (10).

### Storage, scanner and notices (wave 3)

8. **Records (migration 0037, `expand`).** `flow_baselines` and `flow_analysis` gain `rule_set` (JSONB, the
   `models.RuleSet`), `rule_set_digest` and `rule_set_at`. `SetFlowBaseline` and `SaveFlowAnalysis` write
   `rule_set_at` in the same statement as `created_at` / `analyzed_at` and with the same value, and a reader takes the
   set only while the two are equal. The release before writes both tables with explicit column lists, so a row it
   inserts has no set and a row it rewrites keeps the old `rule_set_at` next to a new `created_at`: both read as
   legacy (5) without any change to that release. NULL is never backfilled. `handleSetBaseline` stores
   `report.RuleSet`, `RecordAnalysis` stores it on the snapshot, and the drift endpoint annotates with the baseline's
   set.
9. **Reads stay light.** A single baseline read returns the set (`FlowBaseline.RuleSet`; the API sends only the
   additive `ruleSetDigest`, as the set names every enabled org custom rule and a flow's readers need not be the
   org's members); the batch reads the scanner pages with (`GetFlowBaselinesBatch`, and `LoadFlowHealthBatch`,
   which is also the portfolio's) return the digest only. `RuleSetsByDigest` resolves digests from whichever baseline
   or snapshot still carries them, one index probe per digest; it is a system read pinned to the scanner
   (`TestArch_SystemReadsPinned`). The scanner compares a stored digest with the report's first (equal: nothing new)
   and resolves the page's other digests in at most one read per page, cached for the sweep. A read that fails leaves
   the flow uncompared (no alert, no notice) and unrecorded, so a later sweep compares it; it never falls back to
   legacy, which could silence or flood.
10. **Scanner.** Drift alerts come from `SplitDrift(...).Drift` only (counts and signature too). Regression compares
    the snapshot's score with `HealthOf` over the findings without those of the rules its score did not count: rules
    its set lacks by ID or has as preview (a legacy record: built-ins introduced after rule-set version 1). A revised
    rule or an edited custom rule stays in, since the snapshot counted its earlier revision: leaving it out of the
    current score only would hide a known rule's regression by up to that penalty. That like-for-like score is the
    alert's (the message adds the full score when it differs). The scan key includes both recorded digests. The rules
    newer than a flow's baseline or snapshot that report findings are noted per scope (the flow's organisation,
    else its owner; every flow of a deployment without accounts is the deployment's `app` scope) and current
    rule-set digest, and flushed after every page, and once more when the sweep ends early, through
    `UpsertRuleNotices`: one statement that inserts new notices, merges known ones (union of rule ids, the larger
    flow count, read and dismissed stamps untouched) and returns the inserted ids. Only an inserted notice is
    announced (SSE `governance:alert` with `type: "new_rules"` to the org's members or the user; nobody without
    accounts), so a notice is announced once across sweeps, restarts and replicas. A notice whose user or
    organisation no longer exists is skipped rather than failing its page. A failed flush keeps the notes for the
    next flush and forgets the flows' scans. A flow whose inputs are unchanged is skipped, and notes its last scan's
    rules again: the sweep's last flush touches every notice a flow still raises once, so the retention sweep (12)
    never removes one. `notify.EventNewRules` exists; no channel delivers it until Phase 4c.
11. **One delivery per alert.** `InsertGovernanceAlert` reports whether the inbox row is new, and the scanner pushes
    and dispatches only an inserted alert. After a restart or a failover the in-memory dedup is empty, and a
    persisting drift or regression used to be sent to every channel again (a Jira bug each time); now the row
    decides. An alert the inbox could not record is not delivered, and the next sweep scans its flow again. A
    signature that returns to an earlier value (the same row id) is not sent again either.
12. **`rule_notices`.** Primary key `new_rules|scope|scope_id|digest`; scope `org`, `user` or `app` (empty scope
    id); generated `user_id` / `org_id` columns with `ON DELETE CASCADE` foreign keys, so account erasure and
    organisation deletion remove notices, the previous release's included. Row-level security admits an org notice
    to the org's members and a user notice to that user; the application repeats the predicate
    (`ruleNoticeVisible`) and also shows `app` notices, which only a deployment without accounts writes, to its
    caller. Not exported (derived from analysis); the retention sweep removes a notice no sweep touched within the
    audit window (`updated_at`).
13. **The bell, without new routes.** `GovernanceAlertService` lists the caller's notices that are not dismissed (at
    most 10) ahead of the first page of alerts, as `type: "new_rules"` entries with `flowId: ""` and additive
    `ruleIds`, `scope` and `flowCount`; adds unread notices to the badge; marks them read with the alerts; dismisses
    one by its `new_rules|` id. Clear deletes alerts only: a dismissed notice is never listed again, and deleting it
    would let the next full sweep insert and announce it once more. The client opens the analysis rule settings from
    a notice instead of a flow.

## Consequences

- A preview rule raises no drift, no regression, no notice, never fails `bakicli`, the CI webhook or a policy, and
  never moves health. A preview rule promoted to stable is new to every record that saw it as a preview.
- New stable rules still fail CI against an older baseline file (the CLI keeps the v1 array; D5): only preview is
  exempt there.
- Every uncached analysis does one extra sort and SHA-256 over about 45 references.
- After the release, the first sweep re-analyzes every flow (the rule-set digests change) and every baseline and
  snapshot is legacy: the findings of the eight revised rules become one notice per scope (a legacy record cannot
  tell an earlier org toggle, so those stay drift). A notice is per rule set: after the next rule change, the rules
  still newer than the baselines are announced again with the new ones, until the flows are re-baselined.
- `flow_analysis` rows grow by the rule set (about 1-2 KB of JSONB, TOASTed); the portfolio's read does not load it.
- A legacy health snapshot is not compared: its release graded severities differently (Phase 4a: the
  `hardcoded-credential` default, parse errors and tainted SQL under settings) without a revision, and the first
  sweep raised a regression for unchanged flows. The sweep that skips it records its own analysis as the flow's
  snapshot (`Scanner.SetHealthRecorder`, `DashboardService.RecordAnalysis`), so the next change is compared like for
  like; a regression made between the last snapshot before the upgrade and that sweep goes unreported.
- A regression leaves out only the rules the snapshot did not score. A revised rule or an edited custom rule counts
  on both sides, so one that now finds more or less than its earlier revision moves the comparison by the
  difference (as every rule change did before rule sets); a rule disabled or demoted to preview since the snapshot
  still lowers the snapshot's score only.
- An old client lists a `new_rules` entry (tolerant enum) and opens no flow for it.
- `SetRuleMetaForTesting` changes `Gates` and the references at once; a cached report keeps the stage it was
  stamped with, so the report cache must fold each rule's stage and revision into its key (ADR 0017).

## Alternatives considered

- **A version-only record** (`IntroducedIn > recorded version`). Smaller, but blind to org toggles, custom-rule edits
  and promotions, which would keep producing drift floods.
- **Deriving the record from `RuleProfiles`.** It lists the enabled rules, but not custom-rule revisions or stages,
  and it is a timing diagnostic.
- **Excluding preview findings from `Stats`.** Every Stats-based gate would follow for free, but dashboards,
  `flow_analysis` and CI summaries would disagree with the findings list.
- **Folding severities and options into references.** Changing a severity would then silence that rule's drift
  until re-baseline.
- **A shared `rule_sets(digest, rules)` table.** Smaller rows, but a cross-tenant table of org custom-rule ids that
  needs its own row-security exemption, lifecycle claim and orphan purge.
- **Notices in `gov_alerts`.** `flow_id` is `NOT NULL` with a foreign key, and an alert's visibility comes from its
  flow: a representative flow would show the notice to its non-member collaborators and take it along when deleted.
- **Notice routes of their own** (`/api/governance/notices`). Cleaner typing, but three route goldens and a second
  fetch for the bell.
- **Clear deleting dismissed notices.** Consistent with alerts, but the next full sweep (every restart) would insert
  and announce them again.

## CLI baseline file (wave 3, bakicli)

- `bakicli -update-baseline` keeps writing version 1, a bare JSON array of finding keys, which every bakicli reads.
- The reader also accepts version 2, `{"schemaVersion": 2, "keys": [...], "ruleSet": {...}}`, so a later release may
  write it (expand step). The version is read first: a higher one exits 2 ("written by a newer bakicli"), an object
  without `"schemaVersion": 2` is invalid, and unknown fields are ignored.
- New stable rules keep gating against any baseline file (D5); preview findings never gate. With a version 2 file
  the run also prints how many new findings come from rules newer than the baseline (`SplitDrift(...).NewRule`):
  on stdout beside text output, on stderr for every other format.
- An unreadable or invalid baseline exits 2 before anything is written, in analyze and watch alike (analyze used to
  exit 1 after writing its report). `-update-baseline` does not read `-baseline`, so it can rewrite a broken file.
- With several path arguments each key is written as `<path>#<key>` (the path relative to the working directory
  when inside it, slash-separated), and each flow is checked against its own keys only: a fingerprint names no
  flow, so a union would let one flow's baselined finding hide the same new finding in another. A one-path file is
  unchanged; a one-path file used with several paths, or the reverse, matches nothing and gates on every finding.
