// bakicli is the headless CLI for PAD Analyzer: parse Power Automate Desktop
// flows, run static analysis, exit non-zero when findings meet the severity
// threshold (CI/CD gate). See printUsage for subcommands, flags, exit codes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/storage/database"
	"pad-core/analyzer"
	"pad-core/export"
	"pad-core/models"
	"pad-core/parser"
)

// version is set via -ldflags "-X main.version=..."; "dev" without ldflags.
var version = "dev"

func main() {
	// Batched randomness: the parser mints one UUID per block, so avoid a
	// crypto/rand syscall per block.
	uuid.EnableRandPool()

	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("bakicli %s\n", version)
		return
	}

	// Anything unrecognized (-flags, a bare file/folder) runs the legacy
	// analyze flow so existing CI invocations keep working.
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
		case "watch":
			runWatch(os.Args[2:])
			return
		case "migrate":
			runMigrate(os.Args[2:])
			return
		case "diff-reports":
			runDiffReports(os.Args[2:])
			return
		case "suppressions":
			runSuppressions(os.Args[2:])
			return
		case "report":
			runReport(os.Args[2:])
			return
		}
	}
	runAnalyze()
}

func printUsage() {
	w := flag.CommandLine.Output()
	fmt.Fprintf(w, `bakicli %s — static analysis for Power Automate Desktop flows

Usage:
  bakicli [flags] <file.txt | folder | ->        Analyze (default) and gate
  bakicli fix [flags] <file|folder> ...          Apply auto-fixers (dry-run unless -apply)
  bakicli diff [flags] <old.txt> <new.txt>       Compare two flows structurally
  bakicli diff-reports [flags] <old> <new>       Compare two saved JSON reports
  bakicli rules [-format json] [rule-id]         List or describe rules
  bakicli rules test [flags] <rules.json> <flow> Try custom rules against a flow
  bakicli suppressions [flags] <file|folder|->   List inline pad-ignore directives, flag stale ones
  bakicli report [flags] [report.json]           Record a run on a PAD Analyzer server (CI webhook)
  bakicli init [-o .bakirc.json]                 Generate a starter config
  bakicli watch [flags] <file|folder>            Re-analyze on every save (Ctrl-C to stop)
  bakicli migrate down [flags] <version>         Roll the server schema back (operator-only)
  bakicli --version                              Print version
  bakicli <subcommand> -h                        Flags of a subcommand

Output formats (-format): text, json, sarif, junit, csv, html
  -report-json <path> additionally writes the JSON report from the same run.

Exit codes:
  0  gate passed
  1  gate failed: findings at/above -fail-on, policy violated, stale suppressions
     with -fail-on-stale, or the output could not be written
  2  usage error, invalid config/policy, parse error, or file not found
     (no report is produced — do not upload the output file)

Analyze flags:
`, version)
	flag.PrintDefaults()
}

func runAnalyze() {
	failOn := flag.String("fail-on", "error", "minimum severity that causes exit 1: error, warning, info")
	format := flag.String("format", "text", "output format: text, json, sarif, junit, csv, html")
	rulesFlag := flag.String("rules", "", "comma-separated rule IDs to run (empty = all)")
	customRulesFlag := flag.String("custom-rules", "", "path to custom rules JSON file")
	policyFlag := flag.String("policy", "", "policy JSON file; gate on its rules + gateSeverity (overrides -fail-on)")
	baselineFlag := flag.String("baseline", "", "baseline JSON file (finding fingerprints); gate on NEW findings only (ratchet)")
	newFromRev := flag.String("new-from-rev", "", "git revision (e.g. origin/main); gate only on findings NOT present at that revision")
	updateBaseline := flag.String("update-baseline", "", "write the current run's findings as the baseline to this file and exit 0 (accept current state)")
	configFlag := flag.String("config", ".bakirc.json", "config file path (set to empty to skip auto-discovery)")
	// One run, two outputs: CI derives its summary from the SAME run as the
	// gate, not a diverging second `-format json` invocation.
	reportJSON := flag.String("report-json", "", "also write the JSON report (with the policy verdict) to this file, whatever -format is")
	quiet := flag.Bool("quiet", false, "suppress informational output")
	flag.Usage = printUsage
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: bakicli [flags] <file.txt|folder|->")
		os.Exit(2)
	}

	// -policy short-circuits the baseline ratchet; reject the combination up front.
	if *newFromRev != "" && (*baselineFlag != "" || *updateBaseline != "" || *policyFlag != "") {
		fmt.Fprintln(os.Stderr, "bakicli: -new-from-rev cannot be combined with -baseline, -update-baseline or -policy "+
			"(they are three different ways to decide what counts as a violation; pick one)")
		os.Exit(2)
	}
	if *policyFlag != "" && (*baselineFlag != "" || *updateBaseline != "") {
		fmt.Fprintln(os.Stderr, "bakicli: -policy cannot be combined with -baseline or -update-baseline (the policy gate short-circuits the baseline ratchet); run them as separate invocations")
		os.Exit(2)
	}

	// Apply the config's settings only for flags NOT set on the command line.
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
			if !setFlags["custom-rules"] && cfg.CustomRules != "" {
				*customRulesFlag = cfg.CustomRules
			}
			if !setFlags["policy"] && cfg.Policy != "" {
				*policyFlag = cfg.Policy
			}
		}
	}

	if !isValidFailOn(*failOn) {
		fmt.Fprintf(os.Stderr, "bakicli: unknown -fail-on value %q (%s)\n", *failOn, failOnUsage)
		os.Exit(2)
	}

	validFormats := map[string]bool{"text": true, "json": true, "sarif": true, "junit": true, "csv": true, "html": true}
	if !validFormats[*format] {
		fmt.Fprintf(os.Stderr, "bakicli: unknown -format value %q (must be text, json, sarif, junit, csv, or html)\n", *format)
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

	rules := selectRules(*rulesFlag, *customRulesFlag, inlineRulesOf(loadedCfg))
	settings := buildSettingsFromConfig(loadedCfg)
	report := analyzer.RunAnalysis(doc, rules, settings, nil)

	// A policy is the authoritative gate (its rules + GateSeverity), replacing
	// -fail-on; load up front so a bad file fails fast.
	var policyResult *models.PolicyResult
	if *policyFlag != "" {
		policy, err := loadPolicy(*policyFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: read policy: %v\n", err)
			os.Exit(2)
		}
		policyResult = analyzer.EvaluatePolicy(report, policy)
	}

	// Structured formats carry the policy verdict and analysed root so SARIF/JUnit/JSON can say why the gate failed.
	exportOpts := export.Options{Policy: policyResult}
	exportOpts.SourceRoot, exportOpts.SourceRootURI = sarifSourceRoot(target)
	jsonReport := cliReport{AnalysisReport: report, Policy: policyResult}

	if *reportJSON != "" {
		if err := writeJSONFile(*reportJSON, jsonReport); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: write -report-json: %v\n", err)
			os.Exit(1)
		}
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(jsonReport); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
	case "sarif":
		out, err := export.ReportToSARIFWithOptions(report, doc, exportOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "%s\n", out)
	case "junit":
		out, err := export.ReportToJUnitWithOptions(report, doc, exportOpts)
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
	case "html":
		fmt.Fprintf(os.Stdout, "%s", export.ReportToHTML(report, doc))
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

	// -new-from-rev: the ratchet without a committed artifact; reuses
	// ComputeDrift so "new" means what it means for -baseline.
	if *newFromRev != "" {
		oldFiles, err := flowFilesAtRev(target, *newFromRev)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: -new-from-rev: %v\n", err)
			os.Exit(2)
		}
		var keys []string
		if len(oldFiles) > 0 {
			oldDoc, perr := parser.ParseFiles(oldFiles, filepath.Base(target))
			if perr != nil {
				fmt.Fprintf(os.Stderr, "bakicli: -new-from-rev: parse at %s: %v\n", *newFromRev, perr)
				os.Exit(2)
			}
			oldReport := analyzer.RunAnalysis(oldDoc, rules, settings, nil)
			seen := map[string]bool{}
			for _, f := range oldReport.Findings {
				k := f.Fingerprint
				if k == "" {
					k = f.Key()
				}
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
		}
		drift := analyzer.ComputeDrift(report.FlowID, report, keys)
		if !*quiet {
			fmt.Printf("new since %s: %d (%d errors, %d warnings, %d info) out of %d total\n",
				*newFromRev, len(drift.New), drift.NewErrors, drift.NewWarnings, drift.NewInfo, len(report.Findings))
		}
		if shouldFail(drift.New, *failOn) {
			os.Exit(1)
		}
		return
	}

	// -baseline: gate on NEW findings only (the ratchet); a missing baseline
	// gates on everything.
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

// cliReport is what -format json/-report-json emit: an AnalysisReport plus the policy verdict.
type cliReport struct {
	*models.AnalysisReport
	Policy *models.PolicyResult `json:"policy,omitempty"`
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600) // #nosec G306 -- report file, operator path
}

// sarifSourceRoot derives SourceRoot/SourceRootURI: the analysed root relative
// to the working directory (what CI %SRCROOT% denotes); a target outside cwd
// anchors %SRCROOT% at the analysed root; stdin has none.
func sarifSourceRoot(target string) (root, rootURI string) {
	if target == "-" {
		return "", ""
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", ""
	}
	if info, serr := os.Stat(abs); serr == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		return "", fileURI(abs)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fileURI(abs)
	}
	return rel, fileURI(cwd)
}

func fileURI(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + (&url.URL{Path: p}).EscapedPath()
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
	extra := ""
	if res.Waived > 0 {
		extra = fmt.Sprintf("  waived: %d", res.Waived)
	}
	fmt.Printf("policy %q: %s — %d violation(s) (errors: %d  warnings: %d  info: %d)%s\n",
		name, verdict, len(res.Violations), res.Errors, res.Warnings, res.Info, extra)
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
		return loadFolder(target)
	}
	data, err := os.ReadFile(target) // #nosec G304 -- target is a CLI argument supplied by the operator
	if err != nil {
		return nil, err
	}
	return parser.ParseText(string(data), filepath.Base(target), info.Size())
}

// loadFolder loads every .txt/.pad flow under dir recursively, honoring
// .bakiignore; keys are slash-paths relative to dir so same-named flows stay distinct.
func loadFolder(dir string) (*models.FlowDocument, error) {
	ignorePatterns := loadIgnorePatterns(dir)
	files := make(map[string]string)

	// Read via os.Root, never the walked path: ParseError.Snippet embeds
	// verbatim source into the uploaded SARIF — a symlink must not escape dir (TOCTOU, gosec G122).
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open folder: %w", err)
	}
	defer func() { _ = root.Close() }()

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if rel == "." {
				return nil
			}
			// Skip dot-directories (.git, .github, …): never operator-intended.
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			// Prune ignored directories so "vendor/" costs nothing on a large tree.
			if shouldIgnoreFile(rel+"/", ignorePatterns) || shouldIgnoreFile(rel, ignorePatterns) {
				return fs.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".txt" && ext != ".pad" {
			return nil
		}
		if shouldIgnoreFile(rel, ignorePatterns) {
			return nil
		}
		// Symlinks are skipped: WalkDir reports them via Lstat.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		f, readErr := root.Open(rel)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", rel, readErr)
		}
		data, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr != nil {
			return fmt.Errorf("read %s: %w", rel, readErr)
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read folder: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .txt or .pad files found in %s", dir)
	}
	doc, perr := parser.ParseFiles(files, filepath.Base(dir))
	if perr != nil {
		return nil, perr
	}
	// ParseFiles is path-less; record the folder so output paths read naturally.
	doc.FilePath = dir
	return doc, nil
}

func selectRules(rulesFlag, customRulesPath string, inline []analyzer.CustomRuleConfig) []analyzer.Rule {
	all := analyzer.AllRules()
	if customRulesPath != "" {
		custom, warnings, err := analyzer.LoadCustomRules(customRulesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: -custom-rules: %v\n", err)
			os.Exit(2)
		}
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "bakicli: -custom-rules: warning: %s\n", w)
		}
		all = append(all, custom...)
	}
	// Inline rules from the config file. A bad entry is SKIPPED WITH A WARNING,
	// matching LoadCustomRules — never silently.
	for i, cfg := range inline {
		r, err := analyzer.NewCustomRule(cfg)
		if err != nil {
			id := cfg.ID
			if id == "" {
				id = fmt.Sprintf("#%d (no id)", i)
			}
			fmt.Fprintf(os.Stderr, "bakicli: config customRulesInline: entry %d (rule %q) skipped: %v\n", i, id, err)
			continue
		}
		all = append(all, r)
	}
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

// isValidFailOn reports whether s is a gate threshold; validated everywhere
// because shouldFail silently falls back to error-gating on a typo. "none" is
// rejected — the GitHub Action maps it to info itself.
func isValidFailOn(s string) bool {
	switch s {
	case string(models.SeverityError), string(models.SeverityWarning), string(models.SeverityInfo):
		return true
	default:
		return false
	}
}

const failOnUsage = "must be error, warning, or info"

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
		r, known := rank[string(f.Severity)]
		if !known {
			// Fail CLOSED on an unrecognised severity: a plain lookup would rank
			// it below every threshold and never trip the gate.
			return true
		}
		if r >= min {
			return true
		}
	}
	return false
}

// ---- bakicli watch ----

// runWatch re-analyzes on every change via mtime-polling with a short debounce
// (one save → one analysis); exits 0 on Ctrl-C.
func runWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	failOn := fs.String("fail-on", "error", "minimum severity flagged in each run's summary: error, warning, info")
	rulesFlag := fs.String("rules", "", "comma-separated rule IDs to run (empty = all)")
	customRulesFlag := fs.String("custom-rules", "", "path to custom rules JSON file")
	baselineFlag := fs.String("baseline", "", "baseline JSON file; gate each run on NEW findings only (the ratchet), like analyze")
	policyFlag := fs.String("policy", "", "policy JSON file; gate each run on its rules + gateSeverity (overrides -fail-on)")
	configFlag := fs.String("config", ".bakirc.json", "config file path (set to empty to skip auto-discovery)")
	interval := fs.Duration("interval", 500*time.Millisecond, "poll interval for file changes")
	quiet := fs.Bool("quiet", false, "suppress per-run output; only print when findings change")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli watch [flags] <file.txt|folder>")
		fmt.Fprintln(os.Stderr, "  re-analyze on every save (Ctrl-C to stop)")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	target := fs.Arg(0)

	// Same config merge as analyze: CLI flags win, .bakirc.json fills unset
	// ones — watch must preview the same gate, not a weaker one.
	if *policyFlag != "" && *baselineFlag != "" {
		fmt.Fprintln(os.Stderr, "bakicli watch: -policy cannot be combined with -baseline (same restriction as analyze)")
		os.Exit(2)
	}
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	var watchCfg *bakiConfig
	if *configFlag != "" {
		if cfg, err := loadConfig(*configFlag); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli watch: config: %v\n", err)
			os.Exit(2)
		} else if cfg != nil {
			watchCfg = cfg
			if !setFlags["fail-on"] && cfg.FailOn != "" {
				*failOn = cfg.FailOn
			}
			if !setFlags["rules"] && cfg.Rules != "" {
				*rulesFlag = cfg.Rules
			}
			if !setFlags["custom-rules"] && cfg.CustomRules != "" {
				*customRulesFlag = cfg.CustomRules
			}
			if !setFlags["policy"] && cfg.Policy != "" {
				*policyFlag = cfg.Policy
			}
		}
	}
	// Validate AFTER the config merge: a typo'd failOn in .bakirc.json used to
	// silently replace the validated value.
	if !isValidFailOn(*failOn) {
		fmt.Fprintf(os.Stderr, "bakicli watch: unknown -fail-on value %q (%s)\n", *failOn, failOnUsage)
		os.Exit(2)
	}

	var policy models.Policy
	if *policyFlag != "" {
		var err error
		policy, err = loadPolicy(*policyFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli watch: read policy: %v\n", err)
			os.Exit(2)
		}
	}
	var baselineKeys []string
	hadBaseline := false
	if *baselineFlag != "" {
		var err error
		baselineKeys, hadBaseline, err = loadBaseline(*baselineFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli watch: %v\n", err)
			os.Exit(2)
		}
	}

	rules := selectRules(*rulesFlag, *customRulesFlag, inlineRulesOf(watchCfg))
	files, err := expandFixTargets(target)
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "bakicli watch: %s: no .txt/.pad files to watch\n", target)
		os.Exit(2)
	}

	// SIGINT/SIGTERM → clean exit (exit 0; watch is informational).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "bakicli watch: %s (%d file(s); poll %s; Ctrl-C to stop)\n", target, len(files), *interval)
	analyzeOnce := func() {
		doc, err := load(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli watch: parse error: %v\n", err)
			return
		}
		report := analyzer.RunAnalysis(doc, rules, buildSettingsFromConfig(watchCfg), nil)
		if !*quiet {
			fmt.Println(separator())
			printText(report, false)
		}
		switch {
		case *policyFlag != "":
			res := analyzer.EvaluatePolicy(report, policy)
			printPolicyResult(res, *quiet)
			if res.Passed {
				fmt.Fprintln(os.Stderr, "  → gate PASS (policy)")
			} else {
				fmt.Fprintln(os.Stderr, "  → gate FAIL (policy)")
			}
		case *baselineFlag != "":
			if !hadBaseline {
				if shouldFail(report.Findings, *failOn) {
					fmt.Fprintf(os.Stderr, "  → gate FAIL (no baseline yet; ≥%s)\n", *failOn)
				} else {
					fmt.Fprintf(os.Stderr, "  → gate PASS (no baseline yet; ≥%s)\n", *failOn)
				}
				return
			}
			drift := analyzer.ComputeDrift(report.FlowID, report, baselineKeys)
			fmt.Fprintf(os.Stderr, "  baseline drift: %d new / %d total\n", len(drift.New), len(report.Findings))
			if shouldFail(drift.New, *failOn) {
				fmt.Fprintf(os.Stderr, "  → gate FAIL (new findings ≥%s)\n", *failOn)
			} else {
				fmt.Fprintf(os.Stderr, "  → gate PASS (new findings < %s)\n", *failOn)
			}
		default:
			if shouldFail(report.Findings, *failOn) {
				fmt.Fprintf(os.Stderr, "  → gate FAIL (≥%s)\n", *failOn)
			} else {
				fmt.Fprintf(os.Stderr, "  → gate PASS\n")
			}
		}
	}

	analyzeOnce()

	mtimes := snapshotMt(files)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	var debounce *time.Timer
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "\nbakicli watch: stopped")
			return
		case <-ticker.C:
			cur := snapshotMt(files)
			if !mtimesChanged(mtimes, cur) {
				continue
			}
			mtimes = cur
			// Debounce: one save can bump mtime several times; wait one poll
			// interval for the write to settle, then run once.
			if debounce != nil {
				debounce.Stop()
			}
			r := analyzeOnce
			debounce = time.AfterFunc(*interval, r)
		}
	}
}

// snapshotMt records each file's modtime (zero on a missing file so a
// delete+recreate is detected as a change).
func snapshotMt(files []string) map[string]time.Time {
	out := make(map[string]time.Time, len(files))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			out[f] = info.ModTime()
		} else {
			out[f] = time.Time{}
		}
	}
	return out
}

func mtimesChanged(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return true
	}
	for k, v := range b {
		if av, ok := a[k]; !ok || !av.Equal(v) {
			return true
		}
	}
	return false
}

func separator() string {
	return strings.Repeat("-", 60)
}

// loadBaseline reads a []string of finding fingerprints; a missing file
// returns (nil, false, nil) — "no baseline yet".
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

// writeBaseline captures the run's finding fingerprints; the keys are
// content-stable, so re-imports don't trip the gate.
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

	// Rule-skip warnings (safeCheck recovered a panic) go to stderr so CI sees
	// them without polluting the stdout report stream.
	if report.Stats.RulesSkipped > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d rule evaluation(s) were skipped due to internal errors; findings may be incomplete.\n",
			report.Stats.RulesSkipped)
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

// runFix applies auto-fixers iteratively (parse → analyze → patch → re-parse); dry-run by default.
func runFix(args []string) {
	fs := flag.NewFlagSet("fix", flag.ExitOnError)
	apply := fs.Bool("apply", false, "write the fixed source back to the file (default: dry-run, print to stdout)")
	rule := fs.String("rule", "", "comma-separated rule IDs to fix (empty = all auto-fixable)")
	limit := fs.Int("limit", 50, "maximum fix iterations (safety cap against a fixer that doesn't resolve)")
	format := fs.String("format", "text", "summary format: text, json")
	quiet := fs.Bool("quiet", false, "suppress per-fix log")
	// `fix` reads the SAME .bakirc.json as `analyze` so a rule disabled in
	// the config is never auto-fixed here.
	configFlag := fs.String("config", ".bakirc.json", "config file path (set to empty to skip auto-discovery)")
	customRulesFlag := fs.String("custom-rules", "", "path to custom rules JSON file")
	// Off by default: these fixes change what the flow DOES at runtime, not
	// how it handles failure.
	unsafeFlag := fs.Bool("unsafe", false,
		"also apply fixes that change runtime behaviour (command sanitisation, credential masking)")
	// Keep the original before the atomic replace; the flow source is the
	// user's only copy.
	backup := fs.Bool("backup", false, "with -apply, keep the original as <file>.bak before replacing a changed file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli fix [flags] <file.txt> [file2.txt ...] [folder]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError exits on -h/bad flags; return is always nil
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}

	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	var cfg *bakiConfig
	if *configFlag != "" {
		loaded, err := loadConfig(*configFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: config: %v\n", err)
			os.Exit(2)
		}
		cfg = loaded
	}
	if cfg != nil && !setFlags["custom-rules"] && cfg.CustomRules != "" {
		*customRulesFlag = cfg.CustomRules
	}

	opts := buildFixOptions(cfg, *rule, *customRulesFlag, *limit)
	opts.IncludeBehaviourChanging = *unsafeFlag
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
			n, err := fixOneFile(file, *apply, *backup, opts, *quiet)
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

// expandFixTargets resolves a path arg to files: dirs walked recursively for .txt/.pad honoring .bakiignore; files as-is.
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

func fixOneFile(file string, apply, backup bool, opts analyzer.FixOptions, quiet bool) (int, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- path is a CLI argument supplied by the operator
	if err != nil {
		return 0, err
	}
	source := string(data)
	opts.OnFix = func(ruleID, fixType string, line int) {
		if !quiet {
			fmt.Fprintf(os.Stderr, "fix: [%s] %s %s (line %d)\n", ruleID, fixType, filepath.Base(file), line)
		}
	}
	fixed, err := analyzer.ApplyFixesToSource(&source, filepath.Base(file), opts)
	if err != nil {
		return 0, err
	}
	if apply {
		if backup && source != string(data) {
			if err := writeBackup(file, data); err != nil {
				return 0, err
			}
		}
		if err := atomicWriteFile(file, []byte(source)); err != nil {
			return 0, err
		}
	} else {
		fmt.Print(source)
	}
	return fixed, nil
}

func writeBackup(file string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(file); err == nil {
		mode = info.Mode().Perm()
	}
	// #nosec G306,G304,G703 -- `file` is the path this command was asked to fix
	// (a CLI argument, not request input); the mode mirrors the source file.
	if err := os.WriteFile(file+".bak", data, mode); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	return nil
}

// atomicWriteFile writes via a temp file in the same directory renamed over
// path, so an interrupted write leaves the original intact; the original's
// permission mode is restored (a fresh temp is 0600).
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o600) // default for a not-yet-existing target
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".baki-fix-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp file over %s: %w", path, err)
	}
	return nil
}

// printFixSummary goes to stderr so it never mixes with the patched source on stdout in dry-run mode.
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

// parseRuleSet parses a comma-separated rule-ID list into a set; nil (empty) means all rules.
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
				continue
			}
			label := blockDiffLabel(b)
			fmt.Printf("  %s %s\n", changeSymbol(b.Change), label)
		}
	}
}

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

func blockDiffLabel(b models.BlockDiff) string {
	if b.New != nil {
		return b.New.Name + " (" + b.New.RawType + ")"
	}
	if b.Old != nil {
		return b.Old.Name + " (" + b.Old.RawType + ")"
	}
	return "(empty)"
}

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
	if fs.Arg(0) == "test" {
		runRulesTest(fs.Args()[1:])
		return
	}
	describeRule(fs.Arg(0))
}

// runRulesTest tries custom rules against a flow; construction errors are
// fatal. Exit 2 invalid rule file, 1 no match unless --fail-on-none.
func runRulesTest(args []string) {
	fs := flag.NewFlagSet("rules test", flag.ExitOnError)
	failOnNone := fs.Bool("fail-on-none", false, "exit 1 when no rule matched")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: bakicli rules test <rules.json> <flow.txt|folder|->")
		fmt.Fprintln(os.Stderr, "  <rules.json>: custom-rule JSON — an array, or a single rule object")
		os.Exit(2)
	}
	rulesPath, flowTarget := fs.Arg(0), fs.Arg(1)

	configs, err := readCustomRuleConfigs(rulesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli: rules test: %v\n", err)
		os.Exit(2)
	}
	var rules []analyzer.Rule
	invalid := 0
	for i, cfg := range configs {
		r, cerr := analyzer.NewCustomRule(cfg)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "bakicli: rules test: entry %d (id %q) invalid: %v\n", i, cfg.ID, cerr)
			invalid++
			continue
		}
		rules = append(rules, r)
	}
	if invalid > 0 {
		fmt.Fprintf(os.Stderr, "bakicli: rules test: %d of %d rule(s) invalid\n", invalid, len(configs))
		os.Exit(2)
	}
	if len(rules) == 0 {
		fmt.Fprintln(os.Stderr, "bakicli: rules test: no rules in file")
		os.Exit(2)
	}

	doc, err := load(flowTarget)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli: rules test: load flow: %v\n", err)
		os.Exit(2)
	}
	report := analyzer.RunAnalysis(doc, rules, models.DefaultSettings(), nil)

	matched := 0
	fmt.Printf("%d custom rule(s) against %q (%d block(s)):\n", len(rules), flowTarget, doc.Metadata.BlockCount)
	for _, f := range report.Findings {
		matched++
		fmt.Printf("  [%s] %s — %s (block %s, subflow %s)\n",
			strings.ToUpper(string(f.Severity)), f.RuleID, f.Title, f.BlockID, f.SubflowID)
	}
	perRule := map[string]int{}
	for _, f := range report.Findings {
		perRule[f.RuleID]++
	}
	for _, r := range rules {
		fmt.Printf("rule %-20s %d match(es)\n", r.ID(), perRule[r.ID()])
	}
	if matched == 0 {
		fmt.Println("no matches")
		if *failOnNone {
			os.Exit(1)
		}
		return
	}
	fmt.Printf("%d finding(s)\n", matched)
}

// readCustomRuleConfigs accepts the custom-rules file as an array or a single rule object.
func readCustomRuleConfigs(path string) ([]analyzer.CustomRuleConfig, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, err
	}
	var arr []analyzer.CustomRuleConfig
	if err := json.Unmarshal(data, &arr); err == nil {
		return arr, nil
	}
	var one analyzer.CustomRuleConfig
	if err := json.Unmarshal(data, &one); err == nil && one.ID != "" {
		return []analyzer.CustomRuleConfig{one}, nil
	}
	return nil, fmt.Errorf("%s: not a custom-rule JSON array or object", path)
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
		// Options: tunable thresholds — the keys .bakirc.json ruleConfig.<id>.options accepts.
		Options map[string]any `json:"options,omitempty"`
	}
	defaults := models.DefaultSettings()
	entries := make([]ruleEntry, 0, len(rules))
	for _, r := range rules {
		e := ruleEntry{
			ID:          r.ID(),
			Name:        r.Name(),
			Description: r.Description(),
			Category:    r.Category(),
			Severity:    string(r.DefaultSeverity()),
			Confidence:  string(analyzer.RuleConfidence(r.ID())),
			AutoFix:     analyzer.RuleAutoFix(r.ID()),
		}
		if rc, ok := defaults.Analysis.Rules[r.ID()]; ok && len(rc.Options) > 0 {
			e.Options = maps.Clone(rc.Options)
		}
		entries = append(entries, e)
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

// ---- bakicli migrate ----

// maxDefaultDownSteps caps how far one `migrate down` may roll back without
// the operator raising --max-steps.
const maxDefaultDownSteps = 3

// runMigrate implements `bakicli migrate down <version>`: operator-only schema
// rollback, the reverse of the server's boot-time forward migrate(). Guards:
// --max-steps (default 3), typed confirm (--force skips), reversibility
// pre-check, and a boot-reapply warning (a newer binary re-applies the steps).
func runMigrate(args []string) {
	if len(args) == 0 || args[0] != "down" {
		printMigrateUsage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet("migrate down", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (default: $PAD_DATABASE_URL)")
	force := fs.Bool("force", false, "skip the interactive confirm prompt")
	maxSteps := fs.Int("max-steps", maxDefaultDownSteps, "refuse to roll back more than this many steps in one invocation")
	fs.Usage = printMigrateUsage
	_ = fs.Parse(args[1:])
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	target, err := strconv.Atoi(fs.Arg(0))
	if err != nil || target < 0 {
		fmt.Fprintf(os.Stderr, "bakicli migrate down: target version must be a non-negative integer, got %q\n", fs.Arg(0))
		os.Exit(2)
	}

	dsn := *dsnFlag
	if dsn == "" {
		dsn = os.Getenv("PAD_DATABASE_URL")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "bakicli migrate down: no database DSN (set --dsn or PAD_DATABASE_URL)")
		os.Exit(2)
	}

	// database.New runs the idempotent forward migrate, so schema_migrations
	// exists and the step set is loaded.
	b, err := database.New(context.Background(), database.DefaultConfig(dsn))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli migrate down: connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	current, err := b.CurrentSchemaVersion(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli migrate down: read version: %v\n", err)
		os.Exit(1)
	}
	if target >= current {
		fmt.Printf("schema already at v%d (target v%d); nothing to roll back.\n", current, target)
		return
	}

	// Build + validate the plan from the binary's step list (no DB writes yet).
	steps := database.MigrationSteps()
	stepCount := current - target
	if stepCount > *maxSteps {
		fmt.Fprintf(os.Stderr, "bakicli migrate down: refusing to roll back %d steps (max %d); pass --max-steps to raise the cap\n", stepCount, *maxSteps)
		os.Exit(2)
	}
	fmt.Printf("Rollback plan: v%d → v%d (%d step(s))\n", current, target, stepCount)
	for v := current; v > target; v-- {
		var name string
		reversible := false
		for _, s := range steps {
			if s.Version == v {
				name, reversible = s.Name, s.Reversible
				break
			}
		}
		mark := "ok"
		if !reversible {
			mark = "NOT REVERSIBLE"
		}
		fmt.Printf("  v%d %s [%s]\n", v, name, mark)
		if !reversible {
			fmt.Fprintf(os.Stderr, "bakicli migrate down: v%d %q has no down-migration; cannot roll back past it. Restore from backup instead.\n", v, name)
			os.Exit(2)
		}
	}

	fmt.Println("\nWARNING: a newer server binary will RE-APPLY these steps on its next")
	fmt.Println("boot (the forward migrate() runs at startup). For the rollback to stick,")
	fmt.Println("deploy a binary that no longer contains these steps, or keep the server stopped.")

	if !*force {
		fmt.Printf("\nThis is DESTRUCTIVE and drops data. To proceed, type the target version (%d): ", target)
		var resp string
		if _, err := fmt.Fscanln(os.Stdin, &resp); err != nil {
			fmt.Fprintln(os.Stderr, "bakicli migrate down: no confirmation received; aborting")
			os.Exit(1)
		}
		if resp != strconv.Itoa(target) {
			fmt.Fprintln(os.Stderr, "bakicli migrate down: confirmation did not match target version; aborting")
			os.Exit(1)
		}
	}

	rolled, err := b.MigrateDown(ctx, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli migrate down: FAILED after %d step(s): %v\n", len(rolled), err)
		if len(rolled) > 0 {
			fmt.Fprintf(os.Stderr, "  rolled back: %v\n", rolled)
		}
		os.Exit(1)
	}
	fmt.Printf("Rolled back %d step(s): %v\n", len(rolled), rolled)
}

func printMigrateUsage() {
	fmt.Fprintln(os.Stderr, "usage: bakicli migrate down [flags] <target-version>")
	fmt.Fprintln(os.Stderr, "  roll the DB schema back to <target-version> (operator-only; never runs on boot)")
	fmt.Fprintln(os.Stderr, "flags:")
	fmt.Fprintln(os.Stderr, "  -dsn string       Postgres DSN (default: $PAD_DATABASE_URL)")
	fmt.Fprintln(os.Stderr, "  -force            skip the interactive confirm prompt")
	fmt.Fprintln(os.Stderr, "  -max-steps int    refuse to roll back more than this many steps (default 3)")
}

// bakiConfig is the .bakirc.json schema; all fields optional (missing = defaults).
type bakiConfig struct {
	FailOn      string                  `json:"failOn,omitempty"`
	Format      string                  `json:"format,omitempty"`
	Rules       string                  `json:"rules,omitempty"`
	CustomRules string                  `json:"customRules,omitempty"`
	Policy      string                  `json:"policy,omitempty"`
	Verbose     *bool                   `json:"verbose,omitempty"`
	RuleCfg     map[string]bakiRuleConf `json:"ruleConfig,omitempty"`
	// CustomRulesInline: rule definitions in the config, additive to customRules
	// (a path); /api/orgs/{id}/rules/export emits exactly this shape.
	CustomRulesInline []analyzer.CustomRuleConfig `json:"customRulesInline,omitempty"`
}

type bakiRuleConf struct {
	Enabled  *bool  `json:"enabled,omitempty"`
	Severity string `json:"severity,omitempty"`
	// Options: a rule's thresholds; mirrors models.RuleConfig.Options and the org export's options.
	Options map[string]any `json:"options,omitempty"`
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
		rc := bakiRuleConf{Severity: string(sev), Enabled: &enabled}
		// Emit the thresholds the rule honours so `init` documents their defaults.
		if d, ok := defaults.Analysis.Rules[r.ID()]; ok && len(d.Options) > 0 {
			rc.Options = maps.Clone(d.Options)
		}
		cfg.RuleCfg[r.ID()] = rc
	}
	return cfg
}

// loadConfig reads a .bakirc.json; a missing file returns (nil, nil).
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

// loadIgnorePatterns reads .bakiignore lines as glob patterns; missing file returns nil.
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

// shouldIgnoreFile matches relPath against patterns (filepath.Match wildcards, base-name, and "dir/" prefix).
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

// buildSettingsFromConfig translates RuleCfg into per-rule AppSettings overrides; nil when no config is loaded.
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
		mrc := models.RuleConfig{Enabled: enabled, Severity: rc.Severity}
		// Copy, don't alias: rules read Options while a watch loop may reload.
		if len(rc.Options) > 0 {
			mrc.Options = maps.Clone(rc.Options)
		}
		rules[id] = mrc
	}
	return &models.AppSettings{Analysis: models.AnalysisSettings{Rules: rules}}
}

// buildFixOptions assembles the fix config with the rule set assembled exactly
// as `analyze` does, so a fix run never sees different rules.
func buildFixOptions(cfg *bakiConfig, ruleFlag, customRulesPath string, limit int) analyzer.FixOptions {
	// -rule scopes which findings get fixed; without it the config's rules selection stands in.
	ruleFilter := parseRuleSet(ruleFlag)
	if ruleFilter == nil && cfg != nil && cfg.Rules != "" {
		ruleFilter = parseRuleSet(cfg.Rules)
	}
	return analyzer.FixOptions{
		RuleFilter: ruleFilter,
		Settings:   buildSettingsFromConfig(cfg),
		Rules:      selectRules("", customRulesPath, inlineRulesOf(cfg)),
		Limit:      limit,
	}
}

func runDiffReports(args []string) {
	fs := flag.NewFlagSet("diff-reports", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json")
	failOnNew := fs.Bool("fail-on-new", false, "exit 1 when any finding was ADDED")
	failOn := fs.String("fail-on", "error", "with -fail-on-new: only count added findings at or above this severity (error, warning, info)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli diff-reports [-format text|json] [-fail-on-new [-fail-on sev]] <old.json> <new.json>")
		fmt.Fprintln(os.Stderr, "  findings-level diff between two saved analysis runs")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		fs.Usage()
		os.Exit(2)
	}
	if !isValidFailOn(*failOn) {
		fmt.Fprintf(os.Stderr, "bakicli diff-reports: unknown -fail-on value %q (%s)\n", *failOn, failOnUsage)
		os.Exit(2)
	}

	oldReport, err := readReport(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli diff-reports: %v\n", err)
		os.Exit(2)
	}
	newReport, err := readReport(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli diff-reports: %v\n", err)
		os.Exit(2)
	}

	diff := analyzer.DiffReports(oldReport, newReport)

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(diff); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Printf("diff: +%d added  -%d removed  =%d persisted (old %d → new %d)\n",
			diff.AddedCount, diff.RemovedCount, diff.PersistedCount,
			len(oldReport.Findings), len(newReport.Findings))
		for _, f := range diff.Added {
			fmt.Printf("  + [%s] %s — %s (block %s)\n", strings.ToUpper(string(f.Severity)), f.RuleID, f.Title, f.BlockID)
		}
		for _, f := range diff.Removed {
			fmt.Printf("  - [%s] %s — %s (block %s)\n", strings.ToUpper(string(f.Severity)), f.RuleID, f.Title, f.BlockID)
		}
	}

	if *failOnNew && diff.AddedCount > 0 {
		if shouldFail(diff.Added, *failOn) {
			fmt.Fprintf(os.Stderr, "gate FAIL: %d added finding(s) at or above %s\n", countAtOrAbove(diff.Added, *failOn), *failOn)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "gate PASS: added findings below %s\n", *failOn)
	}
}

// runSuppressions inventories inline `# pad-ignore` directives and flags stale
// ones — a stale directive silently masks future findings. Exit 1 with --fail-on-stale.
func runSuppressions(args []string) {
	fs := flag.NewFlagSet("suppressions", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json")
	failOnStale := fs.Bool("fail-on-stale", false, "exit 1 when any stale directive exists")
	customRulesFlag := fs.String("custom-rules", "", "path to custom rules JSON file")
	configFlag := fs.String("config", ".bakirc.json", "config file path (set to empty to skip auto-discovery)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli suppressions [-format text|json] [-fail-on-stale] [-config .bakirc.json] <file.txt|folder|->")
		fmt.Fprintln(os.Stderr, "  list inline pad-ignore directives and flag stale ones")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}

	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	var cfg *bakiConfig
	if *configFlag != "" {
		loaded, err := loadConfig(*configFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: config: %v\n", err)
			os.Exit(2)
		}
		cfg = loaded
	}
	if cfg != nil && !setFlags["custom-rules"] && cfg.CustomRules != "" {
		*customRulesFlag = cfg.CustomRules
	}

	doc, err := load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli suppressions: %v\n", err)
		os.Exit(2)
	}
	entries := analyzer.SuppressionInventory(doc, selectRules("", *customRulesFlag, inlineRulesOf(cfg)), buildSettingsFromConfig(cfg))

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
	} else {
		if len(entries) == 0 {
			fmt.Println("no pad-ignore directives found")
		}
		stale := 0
		for _, e := range entries {
			marker := "     "
			if e.Stale {
				marker = "STALE"
				stale++
			}
			fmt.Printf("%s line %-4d %s → %s (%s)%s\n",
				marker, e.Line, displayRule(e.Rule), e.BlockLabel, firstNonEmptyStr(e.BlockType, "block"), e.Subflow)
			if e.Reason != "" {
				fmt.Printf("       ↳ %s\n", e.Reason)
			}
		}
		fmt.Printf("%d directive(s), %d stale\n", len(entries), stale)
	}

	if *failOnStale {
		for _, e := range entries {
			if e.Stale {
				os.Exit(1)
			}
		}
	}
}

func displayRule(rule string) string {
	if rule == "*" {
		return "[all rules]"
	}
	return "[" + rule + "]"
}

func firstNonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func readReport(path string) (*models.AnalysisReport, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path) // #nosec G304 -- operator-supplied path
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	var report models.AnalysisReport
	if err := json.NewDecoder(r).Decode(&report); err != nil {
		return nil, fmt.Errorf("%s: invalid analysis report: %w", path, err)
	}
	return &report, nil
}

func countAtOrAbove(findings []models.Finding, min string) int {
	rank := map[string]int{"error": 3, "warning": 2, "info": 1}
	n := 0
	for _, f := range findings {
		if rank[string(f.Severity)] >= rank[min] {
			n++
		}
	}
	return n
}

func inlineRulesOf(cfg *bakiConfig) []analyzer.CustomRuleConfig {
	if cfg == nil {
		return nil
	}
	return cfg.CustomRulesInline
}
