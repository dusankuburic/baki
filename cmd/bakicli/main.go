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
