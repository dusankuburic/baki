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
//	-format  string   Output format: text, json (default "text")
//	-rules   string   Comma-separated list of rule IDs to enable (empty = all enabled)
//	-quiet            Suppress informational output; only print findings
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

func main() {
	failOn := flag.String("fail-on", "error", "minimum severity that causes exit 1: error, warning, info")
	format := flag.String("format", "text", "output format: text, json")
	rulesFlag := flag.String("rules", "", "comma-separated rule IDs to run (empty = all)")
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

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
	default:
		printText(report, *quiet)
	}

	if shouldFail(report.Findings, *failOn) {
		os.Exit(1)
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
	data, err := os.ReadFile(target)
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

func printText(report *models.AnalysisReport, quiet bool) {
	if !quiet {
		fmt.Printf("Analysis complete in %dms | blocks: %d | health: ",
			report.DurationMs, report.Stats.BlocksAnalyzed)
		if report.Metrics != nil {
			fmt.Printf("%d/100", report.Metrics.HealthScore)
		} else {
			fmt.Printf("n/a")
		}
		fmt.Printf(" | errors: %d  warnings: %d  info: %d\n\n",
			report.Stats.Errors, report.Stats.Warnings, report.Stats.Info)
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
