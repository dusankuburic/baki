// bakicli is the headless CLI for PAD Analyzer. It parses one or more Power
// Automate Desktop flow files, runs static analysis, and exits with a
// non-zero code when findings at or above the specified severity threshold
// are present. This makes it suitable for use in CI/CD pipelines.
//
// Usage:
//
//	bakicli [flags] <file.txt | folder>          Analyze (default)
//	bakicli fix [flags] <file.txt>               Apply auto-fixers
//	bakicli diff [flags] <old.txt> <new.txt>     Compare two flows
//	bakicli rules [rule-id]                      List or describe rules
//	bakicli init [-o .bakirc.json]               Generate a starter config
//	bakicli --version                            Print version
//
// Flags (analyze):
//
//	-fail-on string   Minimum severity that causes a non-zero exit: error, warning, info (default "error")
//	-format  string   Output format: text, json, sarif, junit, csv (default "text")
//	-rules   string   Comma-separated list of rule IDs to enable (empty = all enabled)
//	-config  string   Config file path for defaults (default ".bakirc.json"; set to "" to skip)
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
// A config file (.bakirc.json) is auto-discovered from the current directory.
// CLI flags override config values. Generate a starter with `bakicli init`.
//
// # Exit Codes
//
//	0  Analysis passed (no findings at or above the threshold)
//	1  Analysis gate failed (findings at or above -fail-on threshold)
//	2  Usage error, parse error, or file not found
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pad-core/analyzer"
	"pad-core/export"
	"pad-core/models"
	"pad-core/parser"
)

// version is set via -ldflags "-X main.version=..." at build time. "dev" when
// built without ldflags (go run / go build without flags).
var version = "dev"

func main() {
	// --version / -v short-circuits before any subcommand dispatch.
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("bakicli %s\n", version)
		return
	}

	// Subcommand dispatch (backward-compatible): "fix" / "diff" / "rules" /
	// "init" route to their own flagsets; anything else (-flags, a bare
	// file/folder) runs the legacy analyze flow unchanged so existing CI
	// invocations keep working.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "fix":
			runFix(os.Args[2:])
			return
		case "diff":
			runDiff(os.Args[2:])
			return
		case "rules":
			runRules(os.Args[2:])
			return
		case "init":
			runInit(os.Args[2:])
			return
		}
	}
	runAnalyze()
}

func runAnalyze() {
	failOn := flag.String("fail-on", "error", "minimum severity that causes exit 1: error, warning, info")
	format := flag.String("format", "text", "output format: text, json, sarif, junit, csv")
	rulesFlag := flag.String("rules", "", "comma-separated rule IDs to run (empty = all)")
	policyFlag := flag.String("policy", "", "policy JSON file; gate on its rules + gateSeverity (overrides -fail-on)")
	baselineFlag := flag.String("baseline", "", "baseline JSON file (finding fingerprints); gate on NEW findings only (ratchet)")
	updateBaseline := flag.String("update-baseline", "", "write the current run's findings as the baseline to this file and exit 0 (accept current state)")
	configFlag := flag.String("config", ".bakirc.json", "config file path (set to empty to skip auto-discovery)")
	quiet := flag.Bool("quiet", false, "suppress informational output")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: bakicli [flags] <file.txt|folder|->")
		os.Exit(2)
	}

	// Load config file and apply its settings for flags NOT explicitly set
	// on the command line (CLI flags override config). -config="" skips loading.
	setFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	var loadedCfg *bakiConfig
	if *configFlag != "" {
		if cfg, err := loadConfig(*configFlag); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: config: %v\n", err)
			os.Exit(2)
		} else if cfg != nil {
			loadedCfg = cfg
			if !setFlags["fail-on"] && cfg.FailOn != "" {
				*failOn = cfg.FailOn
			}
			if !setFlags["format"] && cfg.Format != "" {
				*format = cfg.Format
			}
			if !setFlags["rules"] && cfg.Rules != "" {
				*rulesFlag = cfg.Rules
			}
			if !setFlags["policy"] && cfg.Policy != "" {
				*policyFlag = cfg.Policy
			}
		}
	}

	validSeverities := map[string]bool{"error": true, "warning": true, "info": true}
	if !validSeverities[*failOn] {
		fmt.Fprintf(os.Stderr, "bakicli: unknown -fail-on value %q (must be error, warning, or info)\n", *failOn)
		os.Exit(2)
	}

	validFormats := map[string]bool{"text": true, "json": true, "sarif": true, "junit": true, "csv": true}
	if !validFormats[*format] {
		fmt.Fprintf(os.Stderr, "bakicli: unknown -format value %q (must be text, json, sarif, junit, or csv)\n", *format)
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
	settings := buildSettingsFromConfig(loadedCfg)
	report := analyzer.RunAnalysis(doc, rules, settings, nil)

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
	case "junit":
		out, err := export.ReportToJUnit(report, doc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "%s\n", out)
	case "csv":
		out, err := export.ReportToCSV(report, doc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "%s", out)
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
	if target == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return parser.ParseText(string(data), "stdin", int64(len(data)))
	}
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

// runFix applies deterministic auto-fixers headlessly to one or more flow files.
// Accepts multiple files and folders (folder mode recursively finds .txt files).
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
		fmt.Fprintln(os.Stderr, "usage: bakicli fix [flags] <file.txt> [file2.txt ...] [folder]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError exits on -h/bad flags; return is always nil
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}

	ruleFilter := parseRuleSet(*rule)
	totalFixed := 0
	filesProcessed := 0

	for i := 0; i < fs.NArg(); i++ {
		path := fs.Arg(i)
		files, err := expandFixTargets(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli fix: %s: %v\n", path, err)
			os.Exit(2)
		}
		for _, file := range files {
			n, err := fixOneFile(file, *apply, ruleFilter, *limit, *quiet)
			if err != nil {
				fmt.Fprintf(os.Stderr, "bakicli fix: %s: %v\n", file, err)
				os.Exit(1)
			}
			totalFixed += n
			filesProcessed++
			if !*quiet {
				printFixSummary(*format, false, n, *apply, file)
			}
		}
	}
	if !*quiet && filesProcessed > 1 {
		printFixSummary(*format, false, totalFixed, *apply, fmt.Sprintf("%d files", filesProcessed))
	}
}

// expandFixTargets resolves a path argument to a list of files. A directory is
// walked recursively for .txt/.pad files. A file path is returned as-is.
// Files matching .bakiignore patterns in the walked root are skipped.
func expandFixTargets(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	ignorePatterns := loadIgnorePatterns(path)
	var files []string
	err = filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(path, p)
		if shouldIgnoreFile(rel, ignorePatterns) {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext == ".txt" || ext == ".pad" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// fixOneFile loads, fixes, and optionally writes a single flow file. Returns
// the number of fixes applied.
func fixOneFile(file string, apply bool, ruleFilter map[string]bool, limit int, quiet bool) (int, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- path is a CLI argument supplied by the operator
	if err != nil {
		return 0, err
	}
	source := string(data)
	fixed, err := analyzer.ApplyFixesToSource(&source, filepath.Base(file), ruleFilter, limit, func(ruleID, fixType string, line int) {
		if !quiet {
			fmt.Fprintf(os.Stderr, "fix: [%s] %s %s (line %d)\n", ruleID, fixType, filepath.Base(file), line)
		}
	})
	if err != nil {
		return 0, err
	}
	if apply {
		if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
			return 0, err
		}
	} else {
		fmt.Print(source)
	}
	return fixed, nil
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
	failOnDiff := fs.Bool("fail-on-diff", false, "exit 1 when structural changes exist (CI gate)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli diff [flags] <old.txt> <new.txt>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
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

	var hasChanges bool

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if *semantic {
			cmp := analyzer.CompareFlows(oldDoc, newDoc)
			hasChanges = cmp.AddedBlocks > 0 || cmp.RemovedBlocks > 0 ||
				(cmp.SharedBlocks > 0 && cmp.Similarity < 1.0)
			_ = enc.Encode(cmp)
		} else {
			d := parser.DiffFlows(oldDoc, newDoc)
			hasChanges = diffHasChanges(d)
			_ = enc.Encode(d)
		}
		if *failOnDiff && hasChanges {
			os.Exit(1)
		}
		return
	}

	if *semantic {
		cmp := analyzer.CompareFlows(oldDoc, newDoc)
		hasChanges = cmp.AddedBlocks > 0 || cmp.RemovedBlocks > 0 ||
			(cmp.SharedBlocks > 0 && cmp.Similarity < 1.0)
		printComparisonText(cmp, fs.Arg(0), fs.Arg(1))
	} else {
		d := parser.DiffFlows(oldDoc, newDoc)
		hasChanges = diffHasChanges(d)
		printDiffText(d, fs.Arg(0), fs.Arg(1))
	}
	if *failOnDiff && hasChanges {
		os.Exit(1)
	}
}

// diffHasChanges returns true when the structural diff contains any
// added/removed/modified blocks.
func diffHasChanges(d *models.FlowDiff) bool {
	if d == nil {
		return false
	}
	for _, sf := range d.Subflows {
		if sf.Change != models.ChangeNone && sf.Change != "" {
			return true
		}
		for _, b := range sf.Blocks {
			if b.Change != models.ChangeNone && b.Change != "" {
				return true
			}
		}
	}
	return false
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

// ---- bakicli rules ----

// runRules lists all registered rules or describes a single rule in detail.
//
//	  //
//		bakicli rules              → table of all rules (ID, severity, category, auto-fix)
//		bakicli rules <rule-id>    → detailed description of one rule
func runRules(args []string) {
	fs := flag.NewFlagSet("rules", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json")
	_ = fs.Parse(args)
	if *format == "json" {
		printRulesJSON()
		return
	}
	if fs.NArg() == 0 {
		printRulesTable()
		return
	}
	describeRule(fs.Arg(0))
}

func printRulesTable() {
	rules := analyzer.AllRules()
	fmt.Printf("%-30s %-8s %-14s %-10s %s\n", "RULE", "SEVERITY", "CATEGORY", "AUTO-FIX", "NAME")
	fmt.Printf("%-30s %-8s %-14s %-10s %s\n", strings.Repeat("-", 30), strings.Repeat("-", 8), strings.Repeat("-", 14), strings.Repeat("-", 10), strings.Repeat("-", 30))
	for _, r := range rules {
		fix := analyzer.RuleAutoFix(r.ID())
		if fix == "" {
			fix = "-"
		}
		fmt.Printf("%-30s %-8s %-14s %-10s %s\n",
			r.ID(),
			r.DefaultSeverity(),
			r.Category(),
			fix,
			r.Name(),
		)
	}
	fmt.Printf("\n%d rules (%d with auto-fix)\n", len(rules), countAutoFixable(rules))
}

// printRulesJSON emits the full rule catalog as JSON for CI/docgen consumers.
func printRulesJSON() {
	rules := analyzer.AllRules()
	type ruleEntry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Category    string `json:"category"`
		Severity    string `json:"defaultSeverity"`
		Confidence  string `json:"confidence"`
		AutoFix     string `json:"autoFix,omitempty"`
	}
	entries := make([]ruleEntry, 0, len(rules))
	for _, r := range rules {
		entries = append(entries, ruleEntry{
			ID:          r.ID(),
			Name:        r.Name(),
			Description: r.Description(),
			Category:    r.Category(),
			Severity:    string(r.DefaultSeverity()),
			Confidence:  string(analyzer.RuleConfidence(r.ID())),
			AutoFix:     analyzer.RuleAutoFix(r.ID()),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(entries)
}

func describeRule(ruleID string) {
	for _, r := range analyzer.AllRules() {
		if r.ID() == ruleID {
			fmt.Printf("Rule:       %s\n", r.ID())
			fmt.Printf("Name:       %s\n", r.Name())
			fmt.Printf("Category:   %s\n", r.Category())
			fmt.Printf("Severity:   %s\n", r.DefaultSeverity())
			fmt.Printf("Confidence: %s\n", analyzer.RuleConfidence(r.ID()))
			fix := analyzer.RuleAutoFix(r.ID())
			if fix != "" {
				fmt.Printf("Auto-fix:   %s\n", fix)
			} else {
				fmt.Printf("Auto-fix:   (none)\n")
			}
			fmt.Printf("\n%s\n", r.Description())
			return
		}
	}
	fmt.Fprintf(os.Stderr, "bakicli rules: unknown rule %q (run 'bakicli rules' to list all rules)\n", ruleID)
	os.Exit(2)
}

func countAutoFixable(rules []analyzer.Rule) int {
	n := 0
	for _, r := range rules {
		if analyzer.RuleAutoFix(r.ID()) != "" {
			n++
		}
	}
	return n
}

// ---- bakicli init ----

// runInit generates a starter .bakirc.json config file in the current directory
// (or the path given by -o). The file contains all rules enabled with their
// default severities, ready for the user to customize.
func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("o", ".bakirc.json", "output path for the config file")
	_ = fs.Parse(args)

	config := buildDefaultConfig()
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli init: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "bakicli init: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d rules)\n", *out, len(config.Rules))
}

// bakiConfig is the .bakirc.json schema. All fields optional — missing fields
// fall back to defaults (same as running without a config).
type bakiConfig struct {
	FailOn  string                  `json:"failOn,omitempty"`
	Format  string                  `json:"format,omitempty"`
	Rules   string                  `json:"rules,omitempty"`
	Policy  string                  `json:"policy,omitempty"`
	Verbose *bool                   `json:"verbose,omitempty"`
	RuleCfg map[string]bakiRuleConf `json:"ruleConfig,omitempty"`
}

type bakiRuleConf struct {
	Enabled  *bool  `json:"enabled,omitempty"`
	Severity string `json:"severity,omitempty"`
}

func buildDefaultConfig() bakiConfig {
	cfg := bakiConfig{
		FailOn: "error",
		Format: "text",
	}
	defaults := models.DefaultSettings()
	cfg.RuleCfg = make(map[string]bakiRuleConf)
	for _, r := range analyzer.AllRules() {
		sev := r.DefaultSeverity()
		enabled := true
		if rc, ok := defaults.Analysis.Rules[r.ID()]; ok {
			if rc.Severity != "" {
				sev = models.Severity(rc.Severity)
			}
			enabled = rc.Enabled
		}
		cfg.RuleCfg[r.ID()] = bakiRuleConf{Severity: string(sev), Enabled: &enabled}
	}
	return cfg
}

// loadConfig reads a .bakirc.json from the given path. Returns nil if the file
// doesn't exist (not an error — config is optional).
func loadConfig(path string) (*bakiConfig, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- config path is a CLI argument
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg bakiConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid config JSON: %w", err)
	}
	return &cfg, nil
}

// loadIgnorePatterns reads .bakiignore from dir (if present) and returns its
// lines as glob patterns. A missing file returns nil (no exclusions).
func loadIgnorePatterns(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, ".bakiignore")) // #nosec G304 -- dir is a CLI argument supplied by the operator
	if err != nil {
		return nil
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// shouldIgnoreFile returns true if relPath matches any .bakiignore glob pattern.
// Patterns are matched with filepath.Match (gitignore-style wildcards: * and ?).
func shouldIgnoreFile(relPath string, patterns []string) bool {
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, relPath); matched {
			return true
		}
		// Also match against the base name (e.g. "*.tmp" matches "foo.tmp")
		if matched, _ := filepath.Match(p, filepath.Base(relPath)); matched {
			return true
		}
		// Directory prefix patterns (e.g. "vendor/" matches "vendor/anything")
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(relPath, p) {
				return true
			}
		}
	}
	return false
}

// buildSettingsFromConfig translates the .bakirc.json RuleCfg map into a
// models.AppSettings so that per-rule enabled/severity overrides take effect
// during analysis. Returns nil when no config is loaded (all rules default).
func buildSettingsFromConfig(cfg *bakiConfig) *models.AppSettings {
	if cfg == nil || len(cfg.RuleCfg) == 0 {
		return nil
	}
	rules := make(map[string]models.RuleConfig, len(cfg.RuleCfg))
	for id, rc := range cfg.RuleCfg {
		enabled := true
		if rc.Enabled != nil {
			enabled = *rc.Enabled
		}
		rules[id] = models.RuleConfig{Enabled: enabled, Severity: rc.Severity}
	}
	return &models.AppSettings{Analysis: models.AnalysisSettings{Rules: rules}}
}
