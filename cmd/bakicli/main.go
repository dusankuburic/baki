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
//	bakicli watch [flags] <file.txt|folder>      Re-analyze on every save (Ctrl-C to stop)
//	bakicli migrate down [flags] <version>       Roll schema back (operator-only)
//	bakicli --version                            Print version
//
// Flags (analyze):
//
//	-fail-on string   Minimum severity that causes a non-zero exit: error, warning, info (default "error")
//	-format  string   Output format: text, json, sarif, junit, csv, html (default "text")
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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

// version is set via -ldflags "-X main.version=..." at build time. "dev" when
// built without ldflags (go run / go build without flags).
var version = "dev"

func main() {
	// The parser mints one UUID per block; batched randomness (still
	// crypto-seeded) avoids a crypto/rand syscall per block on large flows
	// and in the per-fix re-parse loop.
	uuid.EnableRandPool()

	// --version / -v short-circuits before any subcommand dispatch.
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("bakicli %s\n", version)
		return
	}

	// Subcommand dispatch (backward-compatible): "fix" / "diff" / "rules" /
	// "init" / "watch" route to their own flagsets; anything else (-flags, a
	// bare file/folder) runs the legacy analyze flow unchanged so existing CI
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
		}
	}
	runAnalyze()
}

func runAnalyze() {
	failOn := flag.String("fail-on", "error", "minimum severity that causes exit 1: error, warning, info")
	format := flag.String("format", "text", "output format: text, json, sarif, junit, csv, html")
	rulesFlag := flag.String("rules", "", "comma-separated rule IDs to run (empty = all)")
	customRulesFlag := flag.String("custom-rules", "", "path to custom rules JSON file")
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

	// -policy short-circuits before the baseline ratchet logic, so combining it
	// with -baseline/-update-baseline silently drops the baseline behavior (no
	// file written, no drift gate). Reject the combination up front so an
	// operator's intent isn't quietly ignored — run them as separate steps.
	if *policyFlag != "" && (*baselineFlag != "" || *updateBaseline != "") {
		fmt.Fprintln(os.Stderr, "bakicli: -policy cannot be combined with -baseline or -update-baseline (the policy gate short-circuits the baseline ratchet); run them as separate invocations")
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
			if !setFlags["custom-rules"] && cfg.CustomRules != "" {
				*customRulesFlag = cfg.CustomRules
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

	rules := selectRules(*rulesFlag, *customRulesFlag)
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

// loadFolder loads a multi-file flow folder the same way `fix`/`watch` see it:
// top-level .txt AND .pad member files, with .bakiignore patterns honored.
// analyze/diff previously hard-wired ParseFolder (.txt only, ignore-blind), so
// a file excluded from `fix` still failed the `analyze` gate and .pad members
// were silently invisible — one folder, two disagreeing file sets.
func loadFolder(dir string) (*models.FlowDocument, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read folder: %w", err)
	}
	ignorePatterns := loadIgnorePatterns(dir)
	files := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".txt" && ext != ".pad" {
			continue
		}
		if shouldIgnoreFile(name, ignorePatterns) {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- member of the folder the operator pointed at
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", name, rerr)
		}
		files[name] = string(data)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .txt or .pad files found in %s", dir)
	}
	doc, perr := parser.ParseFiles(files, filepath.Base(dir))
	if perr != nil {
		return nil, perr
	}
	// ParseFiles is path-less (upload-shaped); analyze/diff output paths read
	// naturally with the folder recorded.
	doc.FilePath = dir
	return doc, nil
}

func selectRules(rulesFlag, customRulesPath string) []analyzer.Rule {
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

// ---- bakicli watch ----

// runWatch re-analyzes a flow (file or folder) every time it changes on disk —
// a tight feedback loop for an author editing in the PAD designer who wants the
// gate to re-run on every save. Uses an mtime-polling watcher (no fsnotify
// dependency; cross-platform; a single watcher on a CLI is cheap) with a short
// debounce so the multiple mtime bumps of one save fire one analysis.
//
// Exits 0 on Ctrl-C (SIGINT/SIGTERM). Per-run exit code is NOT used (watch mode
// is informational); each run prints a findings summary and, unless -quiet, the
// finding list.
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

	validSeverities := map[string]bool{"error": true, "warning": true, "info": true}
	if !validSeverities[*failOn] {
		fmt.Fprintf(os.Stderr, "bakicli watch: unknown -fail-on value %q (must be error, warning, or info)\n", *failOn)
		os.Exit(2)
	}

	// Config discovery + defaults, identical to analyze: CLI flags win, the
	// .bakirc.json fills unset ones. Watch previously ignored the config and
	// the baseline/policy gates entirely, so the local feedback loop was
	// WEAKER than the CI gate it was supposed to preview.
	if *policyFlag != "" && *baselineFlag != "" {
		fmt.Fprintln(os.Stderr, "bakicli watch: -policy cannot be combined with -baseline (same restriction as analyze)")
		os.Exit(2)
	}
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if *configFlag != "" {
		if cfg, err := loadConfig(*configFlag); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli watch: config: %v\n", err)
			os.Exit(2)
		} else if cfg != nil {
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

	rules := selectRules(*rulesFlag, *customRulesFlag)
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
		report := analyzer.RunAnalysis(doc, rules, models.DefaultSettings(), nil)
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

	// Initial run immediately.
	analyzeOnce()

	// Snapshot mtimes; loop until a change or signal.
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
			// Debounce: a single save can bump mtime several times; wait one
			// poll interval for the write to settle, then run once.
			if debounce != nil {
				debounce.Stop()
			}
			r := analyzeOnce
			debounce = time.AfterFunc(*interval, r)
		}
	}
}

// snapshotMt records the current modtime of each file (zero on a missing file
// so a delete+recreate is detected as a change).
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

	// Surface rule-skip events (safeCheck recovered a panic) on stderr so a CI
	// operator can see findings may be incomplete. Stderr keeps it out of the
	// report stream (which a pipe / file redirect captures from stdout).
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
		if err := atomicWriteFile(file, []byte(source)); err != nil {
			return 0, err
		}
	} else {
		fmt.Print(source)
	}
	return fixed, nil
}

// atomicWriteFile writes data to path atomically: it writes to a temp file in
// the same directory, then renames it over path. An interrupted write (crash,
// Ctrl-C, disk full) therefore leaves the ORIGINAL file intact instead of a
// half-written, truncated flow — `fix --apply` must never corrupt the user's
// automation source, which (unlike a regenerable baseline/config) can't be
// recovered. The original file's permission mode is preserved: a plain
// os.WriteFile over an existing file keeps its mode, so the temp+rename path
// must restore it explicitly (a fresh temp defaults to 0600).
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
	if fs.Arg(0) == "test" {
		runRulesTest(fs.Args()[1:])
		return
	}
	describeRule(fs.Arg(0))
}

// runRulesTest tries custom rules against a fixture flow: load the rule file
// (array or single object), validate every entry (construction errors are
// FATAL here — the whole point is authoring feedback), run ONLY the custom
// rules against the flow, and print per-rule match counts plus each finding.
// Exit codes: 2 invalid rule file, 1 no rule matched (--fail-on-none turns
// the usual 0 into 1 when you expect a match).
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

// readCustomRuleConfigs accepts the custom-rules file in either shape: the
// canonical array used by -custom-rules, or a single rule object (handier
// while iterating on one rule).
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

// ---- bakicli migrate ----

// maxDefaultDownSteps caps how many steps a single `migrate down` may roll back
// without the operator raising --max-steps. A safety net against a fat-fingered
// target version silently destroying many migrations' worth of schema/data.
const maxDefaultDownSteps = 3

// runMigrate implements `bakicli migrate down <version>`: an operator-only,
// never-on-boot, never-over-HTTP schema rollback. It is the reverse of the
// server's boot-time forward migrate().
//
// Guards (per the security review of this feature):
//   - --max-steps (default 3): refuse to roll back more steps than this in one go.
//   - interactive confirm requiring the operator to type the target version,
//     unless --force skips it (non-interactive / scripted).
//   - reversibility pre-check: every step in the path must have a downSQL; an
//     irreversible step (the baseline) aborts before any destructive work.
//   - a loud boot-reapply warning: a newer server binary re-applies the rolled-
//     back steps on its next boot, so the rollback only sticks if the binary is
//     rolled back too (or the step is removed from the binary).
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

	// Connect. database.New runs the forward migrate (idempotent — a no-op when
	// already current) so schema_migrations exists and the step set is loaded.
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

	// Boot-reapply warning: the rollback is undone by a newer binary on next boot.
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

// bakiConfig is the .bakirc.json schema. All fields optional — missing fields
// fall back to defaults (same as running without a config).
type bakiConfig struct {
	FailOn      string                  `json:"failOn,omitempty"`
	Format      string                  `json:"format,omitempty"`
	Rules       string                  `json:"rules,omitempty"`
	CustomRules string                  `json:"customRules,omitempty"`
	Policy      string                  `json:"policy,omitempty"`
	Verbose     *bool                   `json:"verbose,omitempty"`
	RuleCfg     map[string]bakiRuleConf `json:"ruleConfig,omitempty"`
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

// runDiffReports compares two saved analysis runs at the FINDINGS level
// (`bakicli -format json run.json > old.json`, twice): added / removed /
// persisted, with an optional gate. CI pipelines comparing branches had to
// eyeball two JSON blobs — the server had this endpoint, the CLI didn't.
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
	validSeverities := map[string]bool{"error": true, "warning": true, "info": true}
	if !validSeverities[*failOn] {
		fmt.Fprintf(os.Stderr, "bakicli diff-reports: unknown -fail-on value %q\n", *failOn)
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

// runSuppressions inventories the flow's inline `# pad-ignore` directives and
// audits them: STALE directives (rule no longer fires on the target block)
// silently mask future findings forever — a governance audit needs to see
// them. Exit 1 with --fail-on-stale for CI enforcement.
func runSuppressions(args []string) {
	fs := flag.NewFlagSet("suppressions", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json")
	failOnStale := fs.Bool("fail-on-stale", false, "exit 1 when any stale directive exists")
	customRulesFlag := fs.String("custom-rules", "", "path to custom rules JSON file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli suppressions [-format text|json] [-fail-on-stale] <file.txt|folder|->")
		fmt.Fprintln(os.Stderr, "  list inline pad-ignore directives and flag stale ones")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}

	doc, err := load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli suppressions: %v\n", err)
		os.Exit(2)
	}
	entries := analyzer.SuppressionInventory(doc, selectRules("", *customRulesFlag), models.DefaultSettings())

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

// readReport loads a saved AnalysisReport JSON (the `bakicli -format json`
// output of a prior run).
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

// countAtOrAbove counts findings whose severity meets or exceeds min (for
// gate summaries; severity rank error > warning > info).
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
