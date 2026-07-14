// bakicli is the headless CLI for PAD Analyzer. It parses one or more Power
// Automate Desktop flow files, runs static analysis, and exits with a
// non-zero code when findings at or above the specified severity threshold
// are present. This makes it suitable for use in CI/CD pipelines.
//
// Usage:
//
//	bakicli [flags] <file.txt | folder>
//
// Flags:
//
//	-fail-on string   Minimum severity that causes a non-zero exit: error, warning, info (default "error")
//	-format  string   Output format: text, json, sarif (default "text")
//	-rules   string   Comma-separated list of rule IDs to enable (empty = all enabled)
//	-policy  string   Path to a policy JSON file; gates on the policy's rules +
//	                  GateSeverity instead of -fail-on (a named, shareable ruleset)
//	-baseline string  Baseline JSON file (finding fingerprints). When set, the gate
//	                  fails only on NEW findings since the baseline (the ratchet),
//	                  not pre-existing ones — so a team can adopt the gate without
//	                  fixing everything first. Commit the baseline file alongside
//	                  the flow. A missing baseline gates on everything; capture
//	                  a clean run with -update-baseline.
//	-update-baseline string  Write the current run's findings as the baseline to
//	                  this file and exit 0 (accept the current state).
//	-quiet            Suppress informational output; only print findings
//
// A policy file is a JSON models.Policy, e.g.:
//
//	{
//	  "name": "Security baseline",
//	  "gateSeverity": "warning",
//	  "rules": [
//	    {"ruleId": "hardcoded-credential", "enabled": true, "severity": "error"},
//	    {"ruleId": "sql-injection",        "enabled": true}
//	  ]
//	}
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pad-core/analyzer"
	"pad-core/export"
	"pad-core/models"
	"pad-core/parser"
)

func main() {
	// Subcommand dispatch (backward-compatible): "fix" / "diff" route to their
	// own flagsets; anything else (-flags, a bare file/folder) runs the legacy
	// analyze flow unchanged so existing CI invocations keep working.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "fix":
			runFix(os.Args[2:])
			return
		case "diff":
			runDiff(os.Args[2:])
			return
		}
	}
	runAnalyze()
}

func runAnalyze() {
	failOn := flag.String("fail-on", "error", "minimum severity that causes exit 1: error, warning, info")
	format := flag.String("format", "text", "output format: text, json, sarif")
	rulesFlag := flag.String("rules", "", "comma-separated rule IDs to run (empty = all)")
	policyFlag := flag.String("policy", "", "policy JSON file; gate on its rules + gateSeverity (overrides -fail-on)")
	baselineFlag := flag.String("baseline", "", "baseline JSON file (finding fingerprints); gate on NEW findings only (ratchet)")
	updateBaseline := flag.String("update-baseline", "", "write the current run's findings as the baseline to this file and exit 0 (accept current state)")
	quiet := flag.Bool("quiet", false, "suppress informational output")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: bakicli [flags] <file.txt|folder>")
		os.Exit(2)
	}

	validSeverities := map[string]bool{"error": true, "warning": true, "info": true}
	if !validSeverities[*failOn] {
		fmt.Fprintf(os.Stderr, "bakicli: unknown -fail-on value %q (must be error, warning, or info)\n", *failOn)
		os.Exit(2)
	}

	target := flag.Arg(0)

	if !*quiet {
		fmt.Fprintf(os.Stderr, "bakicli: analyzing %s\n", target)
	}

	doc, err := load(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli: parse error: %v\n", err)
		os.Exit(2)
	}

	rules := selectRules(*rulesFlag)
	report := analyzer.RunAnalysis(doc, rules, nil, nil)

	// A policy, when supplied, is the authoritative gate (its own rule set +
	// GateSeverity), replacing -fail-on. Load it up front so a bad file fails fast.
	var policyResult *models.PolicyResult
	if *policyFlag != "" {
		policy, err := loadPolicy(*policyFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: read policy: %v\n", err)
			os.Exit(2)
		}
		policyResult = analyzer.EvaluatePolicy(report, policy)
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
	case "sarif":
		out, err := export.ReportToSARIF(report, doc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "%s\n", out)
	default:
		printText(report, *quiet)
		if policyResult != nil {
			printPolicyResult(policyResult, *quiet)
		}
	}

	if policyResult != nil {
		if !policyResult.Passed {
			os.Exit(1)
		}
		return
	}

	// -update-baseline: ratchet the current run's findings into the baseline
	// file and exit 0. Use after accepting the current state (e.g. a green run).
	if *updateBaseline != "" {
		if err := writeBaseline(*updateBaseline, report); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: failed to write baseline: %v\n", err)
			os.Exit(1)
		}
		if !*quiet {
			fmt.Printf("baseline written: %d findings → %s\n", len(report.Findings), *updateBaseline)
		}
		return
	}

	// -baseline: gate on NEW findings only (the ratchet). A missing baseline
	// file is treated as "no baseline yet" — gate on everything so the first
	// run is green only when clean, then capture it with -update-baseline.
	if *baselineFlag != "" {
		keys, hadBaseline, err := loadBaseline(*baselineFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: %v\n", err)
			os.Exit(1)
		}
		if !hadBaseline {
			if !*quiet {
				fmt.Printf("no baseline at %s — gating on all findings; run with -update-baseline %s to accept the current state\n", *baselineFlag, *baselineFlag)
			}
			if shouldFail(report.Findings, *failOn) {
				os.Exit(1)
			}
			return
		}
		drift := analyzer.ComputeDrift(report.FlowID, report, keys)
		if !*quiet {
			fmt.Printf("baseline drift: %d new (%d errors, %d warnings, %d info) out of %d total\n",
				len(drift.New), drift.NewErrors, drift.NewWarnings, drift.NewInfo, len(report.Findings))
		}
		if shouldFail(drift.New, *failOn) {
			os.Exit(1)
		}
		return
	}

	if shouldFail(report.Findings, *failOn) {
		os.Exit(1)
	}
}

func loadPolicy(path string) (models.Policy, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- policy path is a CLI argument supplied by the operator
	if err != nil {
		return models.Policy{}, err
	}
	var p models.Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return models.Policy{}, fmt.Errorf("invalid policy JSON: %w", err)
	}
	return p, nil
}

func printPolicyResult(res *models.PolicyResult, quiet bool) {
	verdict := "PASS"
	if !res.Passed {
		verdict = "FAIL"
	}
	name := res.PolicyName
	if name == "" {
		name = "policy"
	}
	fmt.Printf("policy %q: %s — %d violation(s) (errors: %d  warnings: %d  info: %d)\n",
		name, verdict, len(res.Violations), res.Errors, res.Warnings, res.Info)
	if quiet {
		return
	}
	for _, v := range res.Violations {
		fmt.Printf("  [%s] %s (rule: %s)\n", strings.ToUpper(string(v.Severity)), v.Title, v.RuleID)
	}
}

func load(target string) (*models.FlowDocument, error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return parser.ParseFolder(target)
	}
	data, err := os.ReadFile(target) // #nosec G304 -- target is a CLI argument supplied by the operator
	if err != nil {
		return nil, err
	}
	return parser.ParseText(string(data), filepath.Base(target), info.Size())
}

func selectRules(rulesFlag string) []analyzer.Rule {
	all := analyzer.AllRules()
	if rulesFlag == "" {
		return all
	}
	wanted := make(map[string]bool)
	for _, id := range strings.Split(rulesFlag, ",") {
		wanted[strings.TrimSpace(id)] = true
	}
	var out []analyzer.Rule
	for _, r := range all {
		if wanted[r.ID()] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		fmt.Fprintf(os.Stderr, "bakicli: -rules %q matched no known rules\n", rulesFlag)
		os.Exit(2)
	}
	return out
}

func shouldFail(findings []models.Finding, threshold string) bool {
	rank := map[string]int{
		string(models.SeverityInfo):    1,
		string(models.SeverityWarning): 2,
		string(models.SeverityError):   3,
	}
	min := rank[threshold]
	if min == 0 {
		min = rank[string(models.SeverityError)]
	}
	for _, f := range findings {
		if rank[string(f.Severity)] >= min {
			return true
		}
	}
	return false
}

// loadBaseline reads a baseline JSON file (a `[]string` of content-stable
// finding fingerprints). A missing file returns (nil, false, nil) — i.e. "no
// baseline established yet" — so the caller can gate on all findings and prompt
// for -update-baseline. Any other read/parse error is returned.
func loadBaseline(path string) (keys []string, hadBaseline bool, err error) {
	data, readErr := os.ReadFile(path) // #nosec G304 -- baseline path is a CLI argument supplied by the operator
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read baseline %s: %w", path, readErr)
	}
	var ks []string
	if err := json.Unmarshal(data, &ks); err != nil {
		return nil, false, fmt.Errorf("invalid baseline JSON %s: %w", path, err)
	}
	return ks, true, nil
}

// writeBaseline captures the current run's finding fingerprints as the accepted
// baseline (a JSON `[]string`). The keys are content-stable (rule + subflow/
// name/line/subject), so the baseline survives re-imports and CLI re-runs —
// only genuinely new findings trip the gate afterwards.
func writeBaseline(path string, report *models.AnalysisReport) error {
	keys := make([]string, 0, len(report.Findings))
	seen := make(map[string]bool, len(report.Findings))
	for _, f := range report.Findings {
		k := f.Fingerprint
		if k == "" {
			k = f.Key()
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644) // #nosec G306 -- baseline is a shared, repo-committed artifact; group-readable is intended
}

func printText(report *models.AnalysisReport, quiet bool) {
	if !quiet {
		fmt.Printf("Analysis complete in %dms | blocks: %d | health: ",
			report.DurationMs, report.Stats.BlocksAnalyzed)
		if report.Metrics != nil {
			fmt.Printf("%d/100", report.Metrics.HealthScore)
		} else {
			fmt.Printf("n/a")
		}
		fmt.Printf(" | errors: %d  warnings: %d  info: %d",
			report.Stats.Errors, report.Stats.Warnings, report.Stats.Info)
		if report.Stats.Suppressed > 0 {
			fmt.Printf("  (suppressed: %d)", report.Stats.Suppressed)
		}
		fmt.Print("\n\n")
	}

	for _, f := range report.Findings {
		sev := strings.ToUpper(string(f.Severity))
		fmt.Printf("[%s] %s\n", sev, f.Title)
		fmt.Printf("  subflow: %s  block: %s\n", f.SubflowID, f.BlockID)
		if f.Description != "" {
			fmt.Printf("  %s\n", f.Description)
		}
		if f.Suggestion != "" {
			fmt.Printf("  suggestion: %s\n", f.Suggestion)
		}
		if f.AutoFixHint != "" {
			fmt.Printf("  fix: %s\n", f.AutoFixHint)
		}
		fmt.Println()
	}

	if len(report.Findings) == 0 && !quiet {
		fmt.Println("No findings.")
	}
}

// ---- bakicli fix ----

// runFix applies deterministic auto-fixers headlessly to a single flow file.
// Iterative: parse → analyze → apply the first auto-fixable finding's patch →
// re-parse → repeat until no fixable finding remains, a fixer declines, or
// --limit is hit. Dry-run by default (prints patched source to stdout); --apply
// writes the file back.
func runFix(args []string) {
	fs := flag.NewFlagSet("fix", flag.ExitOnError)
	apply := fs.Bool("apply", false, "write the fixed source back to the file (default: dry-run, print to stdout)")
	rule := fs.String("rule", "", "comma-separated rule IDs to fix (empty = all auto-fixable)")
	limit := fs.Int("limit", 50, "maximum fix iterations (safety cap against a fixer that doesn't resolve)")
	format := fs.String("format", "text", "summary format: text, json")
	quiet := fs.Bool("quiet", false, "suppress per-fix log")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli fix [flags] <file.txt>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError exits on -h/bad flags; return is always nil
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	file := fs.Arg(0)

	data, err := os.ReadFile(file) // #nosec G304 -- path is a CLI argument supplied by the operator
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli fix: read %s: %v\n", file, err)
		os.Exit(2)
	}
	source := string(data)
	ruleFilter := parseRuleSet(*rule)

	fixed, err := analyzer.ApplyFixesToSource(&source, filepath.Base(file), ruleFilter, *limit, func(ruleID, fixType string, line int) {
		if !*quiet {
			fmt.Fprintf(os.Stderr, "fix: [%s] %s (line %d)\n", ruleID, fixType, line)
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli fix: %v\n", err)
		os.Exit(1)
	}

	// Output: dry-run prints the patched source to stdout; --apply writes it.
	if *apply {
		if err := os.WriteFile(file, []byte(source), 0o600); err != nil { // flow source — owner read/write; existing files keep their perms
			fmt.Fprintf(os.Stderr, "bakicli fix: write %s: %v\n", file, err)
			os.Exit(1)
		}
	} else {
		fmt.Print(source)
	}

	printFixSummary(*format, *quiet, fixed, *apply, file)
}

// printFixSummary emits the fix-run summary (to stderr so it doesn't mix with
// the patched source on stdout in dry-run mode).
func printFixSummary(format string, quiet bool, fixed int, applied bool, file string) {
	if quiet && format == "text" {
		return
	}
	switch format {
	case "json":
		out := map[string]any{"fixed": fixed, "applied": applied, "file": file}
		enc := json.NewEncoder(os.Stderr)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	default:
		verb := "would fix"
		if applied {
			verb = "fixed"
		}
		fmt.Fprintf(os.Stderr, "bakicli fix: %s %d finding(s)%s\n", verb, fixed, applySuffix(applied, file))
	}
}

func applySuffix(applied bool, file string) string {
	if applied {
		return " → wrote " + file
	}
	return " (dry-run; use --apply to write)"
}

// parseRuleSet parses a comma-separated rule-ID list into a set, returning nil
// for an empty list (meaning "all rules"). Shared by runFix's --rule.
func parseRuleSet(rulesFlag string) map[string]bool {
	if rulesFlag == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, id := range strings.Split(rulesFlag, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// ---- bakicli diff ----

// runDiff compares two flow files and prints the structural (DiffFlows) or
// semantic (CompareFlows) difference. Default output is human-readable text;
// --format json emits the model.
func runDiff(args []string) {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json")
	semantic := fs.Bool("semantic", false, "semantic comparison (added/removed/modified + similarity) instead of structural per-block diff")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli diff [flags] <old.txt> <new.txt>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError exits on -h/bad flags; return is always nil
	if fs.NArg() < 2 {
		fs.Usage()
		os.Exit(2)
	}
	oldDoc, err := load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli diff: load %s: %v\n", fs.Arg(0), err)
		os.Exit(2)
	}
	newDoc, err := load(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli diff: load %s: %v\n", fs.Arg(1), err)
		os.Exit(2)
	}

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if *semantic {
			_ = enc.Encode(analyzer.CompareFlows(oldDoc, newDoc))
		} else {
			_ = enc.Encode(parser.DiffFlows(oldDoc, newDoc))
		}
		return
	}

	if *semantic {
		printComparisonText(analyzer.CompareFlows(oldDoc, newDoc), fs.Arg(0), fs.Arg(1))
	} else {
		printDiffText(parser.DiffFlows(oldDoc, newDoc), fs.Arg(0), fs.Arg(1))
	}
}

// printDiffText renders a structural FlowDiff as +/-/~ block lines per subflow.
func printDiffText(d *models.FlowDiff, oldPath, newPath string) {
	fmt.Printf("diff %s → %s\n", oldPath, newPath)
	if d == nil || len(d.Subflows) == 0 {
		fmt.Println("no structural changes")
		return
	}
	for _, sf := range d.Subflows {
		fmt.Printf("\nsubflow %s (%s)\n", sf.Name, sf.Change)
		for _, b := range sf.Blocks {
			if b.Change == models.ChangeNone || b.Change == "" {
				continue // skip unchanged blocks — show only real changes
			}
			label := blockDiffLabel(b)
			fmt.Printf("  %s %s\n", changeSymbol(b.Change), label)
		}
	}
}

// printComparisonText renders a semantic FlowComparison as a one-line summary
// per subflow plus totals.
func printComparisonText(c *models.FlowComparison, oldPath, newPath string) {
	if c == nil {
		fmt.Println("no comparison")
		return
	}
	fmt.Printf("compare %s → %s\n", oldPath, newPath)
	fmt.Printf("shared: %d  added: %d  removed: %d  similarity: %.0f%%\n",
		c.SharedBlocks, c.AddedBlocks, c.RemovedBlocks, c.Similarity*100)
	for _, sf := range c.SubflowDiff {
		fmt.Printf("\nsubflow %s → %s (similarity %.0f%%)\n", sf.SubflowA, sf.SubflowB, sf.Similarity*100)
		for _, b := range sf.BlockDiffs {
			if b.Change == "unchanged" || b.Change == "" {
				continue
			}
			fmt.Printf("  %s %s\n", comparisonSymbol(b.Change), blockName(b))
		}
	}
}

func changeSymbol(c models.ChangeType) string {
	switch c {
	case models.ChangeAdded:
		return "+"
	case models.ChangeRemoved:
		return "-"
	case models.ChangeModified:
		return "~"
	default:
		return " "
	}
}

func comparisonSymbol(change string) string {
	switch change {
	case "added":
		return "+"
	case "removed":
		return "-"
	case "modified":
		return "~"
	default:
		return " "
	}
}

// blockDiffLabel picks a human-readable name from whichever side of a
// structural BlockDiff is present.
func blockDiffLabel(b models.BlockDiff) string {
	if b.New != nil {
		return b.New.Name + " (" + b.New.RawType + ")"
	}
	if b.Old != nil {
		return b.Old.Name + " (" + b.Old.RawType + ")"
	}
	return "(empty)"
}

// blockName picks a name from either side of a semantic BlockComparison.
func blockName(b models.BlockComparison) string {
	if b.BlockB != nil {
		return b.BlockB.Name
	}
	if b.BlockA != nil {
		return b.BlockA.Name
	}
	return "(unknown)"
}
