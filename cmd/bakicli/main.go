// bakicli is the headless CLI for PAD Analyzer: parse Power Automate Desktop
// flows, run static analysis, exit non-zero when findings meet the severity
// threshold (CI/CD gate). See printUsage for subcommands, flags, exit codes.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
  bakicli [flags] <file.txt | folder | -> ...    Analyze (default) and gate
  bakicli fix [flags] <file|folder> ...          Apply auto-fixers (dry-run unless -apply)
  bakicli diff [flags] <old.txt> <new.txt>       Compare two flows structurally
  bakicli diff-reports [flags] <old> <new>       Compare two saved JSON reports
  bakicli rules [-format json] [rule-id]         List or describe rules
  bakicli rules test [flags] <rules.json> <flow> Try custom rules against a flow
  bakicli rules lint [flags] <rules.json>        Check a custom-rules file (or a .bakirc.json)
  bakicli suppressions [flags] <file|folder|->   List inline pad-ignore directives, flag stale ones
  bakicli report [flags] [report.json]           Record a run on a PAD Analyzer server (CI webhook)
  bakicli init [-o .bakirc.json]                 Generate a starter config
  bakicli watch [flags] <file|folder>            Re-analyze on every save (Ctrl-C to stop)
  bakicli migrate down [flags] <version>         Roll the server schema back (operator-only)
  bakicli migrate adopt-checksum [flags] <ver>   Adopt this binary's checksum for a drifted step
  bakicli migrate preflight [flags]              Check pre-deploy migration readiness & drift
  bakicli --version                              Print version
  bakicli <subcommand> -h                        Flags of a subcommand

Output formats (-format): %s
  -report-json <path> additionally writes the JSON report from the same run.
  Several flows (one per path; a path given twice counts once) are analyzed
  together with -format text or json; their cross-flow library findings
  (preview) never gate. Flags go before the paths: a flag after a path is an
  error (exit 2).

Exit codes:
  0  gate passed
  1  gate failed: findings at/above -fail-on, policy violated, stale suppressions
     with -fail-on-stale, or the output could not be written
  2  usage error, invalid config/policy/baseline, parse error, or file not found
     (no report is produced — do not upload the output file)

Analyze flags:
`, version, strings.Join(outputFormats(), ", "))
	flag.PrintDefaults()
}

func runAnalyze() {
	failOn := flag.String("fail-on", "error", "minimum severity that causes exit 1: error, warning, info")
	format := flag.String("format", textFormat, "output format: "+strings.Join(outputFormats(), ", "))
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
		fmt.Fprintln(os.Stderr, "usage: bakicli [flags] <file.txt|folder|-> [more flows ...]")
		os.Exit(2)
	}
	// Flag parsing stops at the first path, so a flag after it would be read
	// as a flow (and was silently ignored before several paths existed).
	for _, arg := range flag.Args() {
		if len(arg) > 1 && strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "bakicli: flags must come before the paths: %s (a path that starts with '-' can be written ./%s)\n", arg, arg)
			os.Exit(2)
		}
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
	if !isValidFormat(*format) {
		fmt.Fprintf(os.Stderr, "bakicli: unknown -format value %q (must be %s)\n", *format, formatChoices())
		os.Exit(2)
	}
	targets := distinctTargets(flag.Args())
	if err := checkOutput(*format, targets, isTerminal(os.Stdout)); err != nil {
		fmt.Fprintf(os.Stderr, "bakicli: %v\n", err)
		os.Exit(2)
	}

	// Every input that can fail with exit 2 is read before anything is written,
	// so exit 2 never leaves a report behind.
	var policy *models.Policy
	if *policyFlag != "" {
		p, err := loadPolicy(*policyFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: read policy: %v\n", err)
			os.Exit(2)
		}
		policy = &p
	}
	var baseline baselineFile
	if *baselineFlag != "" && *updateBaseline == "" { // -update-baseline wins and may be rewriting a broken file
		var err error
		if baseline, err = loadBaseline(*baselineFlag); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: %v\n", err)
			os.Exit(2)
		}
	}

	rules, libraryRules := selectRules(*rulesFlag, *customRulesFlag, inlineRulesOf(loadedCfg))
	if len(rules) == 0 && len(targets) < 2 {
		fmt.Fprintf(os.Stderr, "bakicli: warning: -rules %q selects only library rules, which run across two or more flows\n", *rulesFlag)
	}
	settings := buildSettingsFromConfig(loadedCfg)

	runs := make([]flowRun, 0, len(targets))
	for _, target := range targets {
		if !*quiet {
			fmt.Fprintf(os.Stderr, "bakicli: analyzing %s\n", target)
		}
		doc, err := load(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: parse error: %v\n", err)
			os.Exit(2)
		}
		run := flowRun{target: target, doc: doc, report: analyzer.RunAnalysis(doc, rules, settings, nil)}
		if policy != nil {
			// A policy is the authoritative gate (its rules + GateSeverity), replacing -fail-on.
			run.policy = analyzer.EvaluatePolicy(run.report, *policy)
		}
		if *newFromRev != "" {
			if run.revKeys, err = keysAtRev(target, *newFromRev, rules, settings); err != nil {
				fmt.Fprintf(os.Stderr, "bakicli: -new-from-rev: %v\n", err)
				os.Exit(2)
			}
		}
		runs = append(runs, run)
	}
	var library *models.LibraryReport
	if len(runs) > 1 {
		docs := make([]*models.FlowDocument, len(runs))
		for i, r := range runs {
			docs[i] = r.doc
		}
		library = analyzer.AnalyzeLibrary(context.Background(), docs, libraryRules, settings)
	}

	// Structured formats carry the policy verdict and analysed root so SARIF/JUnit/JSON can say why the gate failed.
	exportOpts := export.Options{ToolVersion: version}
	if len(runs) == 1 {
		exportOpts.Policy = runs[0].policy
		exportOpts.SourceRoot, exportOpts.SourceRootURI = sarifSourceRoot(targets[0])
	}
	if *reportJSON != "" {
		if err := writeReportJSON(*reportJSON, runs, library, exportOpts); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: write -report-json: %v\n", err)
			os.Exit(1)
		}
	}
	if err := renderReport(*format, runs, library, exportOpts, *quiet); err != nil {
		fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
		os.Exit(1)
	}

	if gateFailed(runs, *format, *failOn, analyzeGate{
		policy: policy != nil, updateBaseline: *updateBaseline, newFromRev: *newFromRev,
		baselinePath: *baselineFlag, baseline: baseline, quiet: *quiet,
	}) {
		os.Exit(1)
	}
}

// distinctTargets drops a path argument that names a path given before it,
// with a warning: one flow given twice was analyzed, reported and compared by
// the library rules as two. Two existing paths are the same when os.SameFile
// says so (another case where the filesystem ignores case, a link), else when
// their cleaned absolute paths are equal; lowercasing on Windows merged two
// flows of a case-sensitive directory. "-" (stdin) is one input however often
// it is given.
func distinctTargets(targets []string) []string {
	type given struct {
		target, abs string
		info        os.FileInfo // nil when the path cannot be stat'ed
	}
	seen := make([]given, 0, len(targets))
	out := make([]string, 0, len(targets))
next:
	for _, target := range targets {
		cur := given{target: target, abs: target}
		if target != "-" {
			if abs, err := filepath.Abs(target); err == nil {
				cur.abs = abs
			}
			if info, err := os.Stat(target); err == nil {
				cur.info = info
			}
		}
		for _, prev := range seen {
			if cur.abs == prev.abs || (cur.info != nil && prev.info != nil && os.SameFile(cur.info, prev.info)) {
				fmt.Fprintf(os.Stderr, "bakicli: warning: %s names the same flow as %s; analyzing it once\n", target, prev.target)
				continue next
			}
		}
		seen = append(seen, cur)
		out = append(out, target)
	}
	return out
}

// analyzeGate is how an analyze run decides its exit code.
type analyzeGate struct {
	policy         bool
	updateBaseline string
	newFromRev     string
	baselinePath   string
	baseline       baselineFile
	quiet          bool
}

// gateFailed applies the run's gate after the report was written and prints
// its informational lines (stdout beside text, stderr otherwise). A write
// failure of -update-baseline exits 1 itself.
func gateFailed(runs []flowRun, format, failOn string, g analyzeGate) bool {
	info := infoOut(format)
	var findings, newFindings []models.Finding
	for _, r := range runs {
		findings = append(findings, r.report.Findings...)
	}
	switch {
	case g.policy:
		for _, r := range runs {
			if !r.policy.Passed {
				return true
			}
		}
		return false

	case g.updateBaseline != "":
		if err := writeBaseline(g.updateBaseline, baselineKeys(runs)); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: failed to write baseline: %v\n", err)
			os.Exit(1)
		}
		if !g.quiet {
			fmt.Fprintf(info, "baseline written: %d findings → %s\n", len(findings), g.updateBaseline)
		}
		return false

	case g.newFromRev != "":
		// The ratchet without a committed artifact; reuses ComputeDrift so
		// "new" means what it means for -baseline.
		var errs, warns, infos int
		for _, r := range runs {
			drift := analyzer.ComputeDrift(r.report.FlowID, r.report, r.revKeys)
			newFindings = append(newFindings, drift.New...)
			errs, warns, infos = errs+drift.NewErrors, warns+drift.NewWarnings, infos+drift.NewInfo
		}
		if !g.quiet {
			fmt.Fprintf(info, "new since %s: %d (%d errors, %d warnings, %d info) out of %d total%s\n",
				g.newFromRev, len(newFindings), errs, warns, infos, len(findings), previewNote(newFindings))
		}
		return shouldFail(newFindings, failOn)

	case g.baselinePath != "":
		// Gate on NEW findings only (the ratchet); a missing baseline gates on everything.
		if !g.baseline.Found {
			if !g.quiet {
				fmt.Fprintf(info, "no baseline at %s — gating on all findings; run with -update-baseline %s to accept the current state\n", g.baselinePath, g.baselinePath)
			}
			return shouldFail(findings, failOn)
		}
		var errs, warns, infos, newer int
		for _, r := range runs {
			keys := g.baseline.Keys
			if len(runs) > 1 {
				keys = keysInScope(keys, r.target) // see baselineKeys
			}
			drift := analyzer.ComputeDrift(r.report.FlowID, r.report, keys)
			newFindings = append(newFindings, drift.New...)
			errs, warns, infos = errs+drift.NewErrors, warns+drift.NewWarnings, infos+drift.NewInfo
			newer += newerRuleFindings(g.baseline, r.report, drift)
		}
		if !g.quiet {
			fmt.Fprintf(info, "baseline drift: %d new (%d errors, %d warnings, %d info) out of %d total%s\n",
				len(newFindings), errs, warns, infos, len(findings), previewNote(newFindings))
			if g.baseline.RuleSet != nil {
				fmt.Fprintf(info, "%d findings from rules newer than this baseline\n", newer)
			}
		}
		return shouldFail(newFindings, failOn)
	}
	return shouldFail(findings, failOn)
}

// keysAtRev analyzes target as it was at rev with the same rules and returns
// its finding keys (fingerprints, legacy keys as fallback); a target that did
// not exist at rev has none.
func keysAtRev(target, rev string, rules []analyzer.Rule, settings *models.AppSettings) ([]string, error) {
	oldFiles, err := flowFilesAtRev(target, rev)
	if err != nil {
		return nil, err
	}
	if len(oldFiles) == 0 {
		return nil, nil
	}
	oldDoc, err := parser.ParseFiles(oldFiles, filepath.Base(target))
	if err != nil {
		return nil, fmt.Errorf("parse at %s: %w", rev, err)
	}
	return findingKeys(analyzer.RunAnalysis(oldDoc, rules, settings, nil)), nil
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

func printPolicyResult(w io.Writer, res *models.PolicyResult, quiet bool) {
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
	if res.Preview > 0 {
		extra += fmt.Sprintf("  preview (not gated): %d", res.Preview)
	}
	fmt.Fprintf(w, "policy %q: %s — %d violation(s) (errors: %d  warnings: %d  info: %d)%s\n",
		name, verdict, len(res.Violations), res.Errors, res.Warnings, res.Info, extra)
	if quiet {
		return
	}
	for _, v := range res.Violations {
		fmt.Fprintf(w, "  [%s] %s (rule: %s)\n", strings.ToUpper(string(v.Severity)), v.Title, v.RuleID)
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

// loadFolder loads the folder's file set (parser.TreeFolder: every .txt/.pad
// under dir, outside dot directories and .bakiignore matches, symlinks
// skipped, reads confined to dir) as one flow; keys are slash paths relative
// to dir so same-named flows stay distinct.
func loadFolder(dir string) (*models.FlowDocument, error) {
	warnIgnoreFile(dir)
	return parser.ParseFolderTree(dir, parser.TreeFolder())
}

// warnedIgnoreFiles holds the folders whose .bakiignore was checked, so watch
// (which reloads on every save) warns once.
var warnedIgnoreFiles sync.Map

// warnIgnoreFile warns on stderr about .bakiignore patterns that match nothing
// the way they are written: path.Match rejects them, or a backslash (an
// escape, not a separator) is in them.
func warnIgnoreFile(dir string) {
	key := dir
	if abs, err := filepath.Abs(dir); err == nil {
		key = abs
	}
	if _, done := warnedIgnoreFiles.LoadOrStore(key, true); done {
		return
	}
	for _, p := range parser.ReadIgnoreFile(dir, ".bakiignore") {
		if _, err := path.Match(p, ""); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: warning: %s: .bakiignore pattern %q is malformed and matches nothing (use '/' between directories)\n", dir, p)
		} else if strings.Contains(p, `\`) {
			fmt.Fprintf(os.Stderr, "bakicli: warning: %s: .bakiignore pattern %q has a backslash, which escapes the next character; use '/' between directories\n", dir, p)
		}
	}
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
	var baseline baselineFile
	if *baselineFlag != "" {
		var err error
		if baseline, err = loadBaseline(*baselineFlag); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli watch: %v\n", err)
			os.Exit(2)
		}
	}

	rules, _ := selectRules(*rulesFlag, *customRulesFlag, inlineRulesOf(watchCfg))
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
			printPolicyResult(os.Stdout, res, *quiet)
			if res.Passed {
				fmt.Fprintln(os.Stderr, "  → gate PASS (policy)")
			} else {
				fmt.Fprintln(os.Stderr, "  → gate FAIL (policy)")
			}
		case *baselineFlag != "":
			if !baseline.Found {
				if shouldFail(report.Findings, *failOn) {
					fmt.Fprintf(os.Stderr, "  → gate FAIL (no baseline yet; ≥%s)\n", *failOn)
				} else {
					fmt.Fprintf(os.Stderr, "  → gate PASS (no baseline yet; ≥%s)\n", *failOn)
				}
				return
			}
			drift := analyzer.ComputeDrift(report.FlowID, report, baseline.Keys)
			fmt.Fprintf(os.Stderr, "  baseline drift: %d new / %d total%s\n", len(drift.New), len(report.Findings), previewNote(drift.New))
			if baseline.RuleSet != nil {
				fmt.Fprintf(os.Stderr, "  %d findings from rules newer than this baseline\n", newerRuleFindings(baseline, report, drift))
			}
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
		fmt.Printf("%s\n\n", previewNote(report.Findings))
	}

	// Rule-skip warnings (safeCheck recovered a panic) go to stderr so CI sees
	// them without polluting the stdout report stream.
	warnSkippedRules(report)

	for i := range report.Findings {
		f := &report.Findings[i]
		tag := "[" + strings.ToUpper(string(f.Severity)) + "]"
		if !analyzer.Gates(f) {
			tag += "[PREVIEW]"
		}
		fmt.Printf("%s %s\n", tag, f.Title)
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

// expandFixTargets resolves a path arg to files: a folder gives analyze's
// file set (parser.TreeFolder, sorted), a file itself.
func expandFixTargets(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	warnIgnoreFile(path)
	keys, err := parser.ListFolder(path, parser.TreeFolder())
	if err != nil {
		return nil, err
	}
	files := make([]string, len(keys))
	for i, k := range keys {
		files[i] = filepath.Join(path, filepath.FromSlash(k))
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
	fmt.Printf("wrote %s (%d rules)\n", *out, len(config.RuleCfg))
}

// ---- bakicli migrate ----

// maxDefaultDownSteps caps how far one `migrate down` may roll back without
// the operator raising --max-steps.
const maxDefaultDownSteps = 3

// runMigrate dispatches migrate subcommands (down, adopt-checksum, preflight).
func runMigrate(args []string) {
	if len(args) == 0 {
		printMigrateUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "down":
		runMigrateDown(args[1:])
	case "adopt-checksum":
		runMigrateAdoptChecksum(args[1:])
	case "preflight":
		runMigratePreflight(args[1:])
	default:
		printMigrateUsage()
		os.Exit(2)
	}
}

// runMigrateDown implements `bakicli migrate down <version>`: operator-only schema
// rollback, the reverse of the server's boot-time forward migrate(). Guards:
// --max-steps (default 3), typed confirm (--force skips), reversibility
// pre-check, and a boot-reapply warning (a newer binary re-applies the steps).
func runMigrateDown(args []string) {
	fs := flag.NewFlagSet("migrate down", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (default: $PAD_DATABASE_URL)")
	force := fs.Bool("force", false, "skip the interactive confirm prompt")
	maxSteps := fs.Int("max-steps", maxDefaultDownSteps, "refuse to roll back more than this many steps in one invocation")
	fs.Usage = printMigrateDownUsage
	_ = fs.Parse(args)
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

	// OpenForMaintenance connects and configures the pool without running
	// forward migrations or verifyChecksums, allowing down migrations on drifted
	// schemas or pre-48 schemas with duplicate data.
	b, err := database.OpenForMaintenance(context.Background(), database.DefaultConfig(dsn))
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
	maxKnown := latestKnownMigrationVersion(steps)
	stepCount := current - target
	if current > maxKnown {
		fmt.Printf("Rollback plan: v%d → v%d (%d step(s))\n", current, target, stepCount)
		fmt.Printf("  v%d [NEWER THAN THIS BINARY]\n", current)
		fmt.Fprintf(os.Stderr, "bakicli migrate down: v%d is newer than this bakicli (knows v%d); run migrate down with the release that added it\n", current, maxKnown)
		os.Exit(2)
	}
	if stepCount > *maxSteps {
		fmt.Fprintf(os.Stderr, "bakicli migrate down: refusing to roll back %d steps (max %d); pass --max-steps to raise the cap\n", stepCount, *maxSteps)
		os.Exit(2)
	}
	fmt.Printf("Rollback plan: v%d → v%d (%d step(s))\n", current, target, stepCount)
	for v := current; v > target; v-- {
		if v > maxKnown {
			fmt.Printf("  v%d [NEWER THAN THIS BINARY]\n", v)
			fmt.Fprintf(os.Stderr, "bakicli migrate down: v%d is newer than this bakicli (knows v%d); run migrate down with the release that added it\n", v, maxKnown)
			os.Exit(2)
		}
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
	fmt.Println("boot (the forward migrate() runs at startup). Stop every replica first (its")
	fmt.Println("requests would also wait on the down's locks); for the rollback to stick,")
	fmt.Println("deploy a binary that no longer contains these steps, or keep the server stopped.")
	fmt.Println("An image rollback past expand-only steps needs no down at all.")

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

func latestKnownMigrationVersion(steps []database.MigrationStep) int {
	var maxV int
	for _, s := range steps {
		if s.Version > maxV {
			maxV = s.Version
		}
	}
	return maxV
}

// runMigrateAdoptChecksum implements `bakicli migrate adopt-checksum <version>`:
// adopts this binary's computed checksum for a recorded migration row.
func runMigrateAdoptChecksum(args []string) {
	fs := flag.NewFlagSet("migrate adopt-checksum", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (default: $PAD_DATABASE_URL)")
	force := fs.Bool("force", false, "skip the interactive confirm prompt")
	fs.Usage = printMigrateAdoptChecksumUsage
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	version, err := strconv.Atoi(fs.Arg(0))
	if err != nil || version <= 0 {
		fmt.Fprintf(os.Stderr, "bakicli migrate adopt-checksum: version must be a positive integer, got %q\n", fs.Arg(0))
		os.Exit(2)
	}

	dsn := *dsnFlag
	if dsn == "" {
		dsn = os.Getenv("PAD_DATABASE_URL")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "bakicli migrate adopt-checksum: no database DSN (set --dsn or PAD_DATABASE_URL)")
		os.Exit(2)
	}

	steps := database.MigrationSteps()
	maxKnown := latestKnownMigrationVersion(steps)
	computed, ok := database.MigrationChecksum(version)
	if !ok || version > maxKnown {
		fmt.Fprintf(os.Stderr, "bakicli migrate adopt-checksum: unknown migration version %d (latest known is %d)\n", version, maxKnown)
		os.Exit(2)
	}

	b, err := database.OpenForMaintenance(context.Background(), database.DefaultConfig(dsn))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli migrate adopt-checksum: connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()

	var recorded sql.NullString
	err = b.DB().QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = $1`, version).Scan(&recorded)
	if errors.Is(err, sql.ErrNoRows) {
		fmt.Fprintf(os.Stderr, "bakicli migrate adopt-checksum: migration v%d is not recorded in schema_migrations\n", version)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli migrate adopt-checksum: read version v%d: %v\n", version, err)
		os.Exit(1)
	}
	if !recorded.Valid || recorded.String == "" {
		fmt.Fprintf(os.Stderr, "bakicli migrate adopt-checksum: migration v%d has no recorded checksum to adopt (empty)\n", version)
		os.Exit(1)
	}

	fmt.Printf("Migration v%d checksum:\n", version)
	fmt.Printf("  recorded: %s\n", recorded.String)
	fmt.Printf("  computed: %s\n", computed)

	if recorded.String == computed {
		fmt.Printf("Checksum for v%d is already up to date.\n", version)
		return
	}

	if !*force {
		fmt.Printf("\nTo adopt the computed checksum for v%d, type the version (%d): ", version, version)
		var resp string
		if _, err := fmt.Fscanln(os.Stdin, &resp); err != nil {
			fmt.Fprintln(os.Stderr, "bakicli migrate adopt-checksum: no confirmation received; aborting")
			os.Exit(1)
		}
		if resp != strconv.Itoa(version) {
			fmt.Fprintln(os.Stderr, "bakicli migrate adopt-checksum: confirmation did not match version; aborting")
			os.Exit(1)
		}
	}

	rec, comp, err := b.AdoptChecksum(ctx, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli migrate adopt-checksum: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Adopted checksum for v%d: %s (was %s)\n", version, comp, rec)
}

// runMigratePreflight implements `bakicli migrate preflight`: verifies readiness
// for migration 0048 (checks for case-duplicate emails, duplicate invite tokens)
// and inspects schema_migrations >= 47 for checksum drift.
func runMigratePreflight(args []string) {
	fs := flag.NewFlagSet("migrate preflight", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (default: $PAD_DATABASE_URL)")
	fs.Usage = printMigratePreflightUsage
	_ = fs.Parse(args)

	dsn := *dsnFlag
	if dsn == "" {
		dsn = os.Getenv("PAD_DATABASE_URL")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "bakicli migrate preflight: no database DSN (set --dsn or PAD_DATABASE_URL)")
		os.Exit(2)
	}

	b, err := database.OpenForMaintenance(context.Background(), database.DefaultConfig(dsn))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli migrate preflight: connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	db := b.DB()

	actionNeeded := false

	// 1. Check for case-duplicate emails in users
	type dupEmailGroup struct {
		address string
		ids     string
	}
	var emailDups []dupEmailGroup
	rows, err := db.QueryContext(ctx, database.DuplicateEmailGroupsSQL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight: query duplicate emails: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	for rows.Next() {
		var g dupEmailGroup
		if err := rows.Scan(&g.address, &g.ids); err != nil {
			fmt.Fprintf(os.Stderr, "preflight: scan duplicate email row: %v\n", err)
			os.Exit(1)
		}
		emailDups = append(emailDups, g)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "preflight: iterate duplicate emails: %v\n", err)
		os.Exit(1)
	}

	if len(emailDups) > 0 {
		actionNeeded = true
		fmt.Printf("ACTION NEEDED: found %d case-duplicate email group(s) in users:\n", len(emailDups))
		for _, g := range emailDups {
			fmt.Printf("  address %q: %s\n", g.address, g.ids)
		}
		fmt.Println("\nRecipe to resolve duplicates before deploying v48 (docs/deployment.md, Before deploying v48):")
		fmt.Println(database.DuplicateEmailResolveSQL)
		fmt.Println()
	}

	// 2. Check for duplicate token_hash in org_invites
	type dupInviteGroup struct {
		ids string
	}
	var inviteDups []dupInviteGroup
	invRows, err := db.QueryContext(ctx, database.DuplicateInviteTokenGroupsSQL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight: query duplicate invites: %v\n", err)
		os.Exit(1)
	}
	defer invRows.Close()
	for invRows.Next() {
		var d dupInviteGroup
		if err := invRows.Scan(&d.ids); err != nil {
			fmt.Fprintf(os.Stderr, "preflight: scan duplicate invite row: %v\n", err)
			os.Exit(1)
		}
		inviteDups = append(inviteDups, d)
	}
	if err := invRows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "preflight: iterate duplicate invites: %v\n", err)
		os.Exit(1)
	}

	if len(inviteDups) > 0 {
		actionNeeded = true
		fmt.Printf("ACTION NEEDED: found %d duplicate token_hash group(s) in org_invites:\n", len(inviteDups))
		for _, d := range inviteDups {
			fmt.Printf("  invite ids: %s\n", d.ids)
		}
		fmt.Println("Recipe: DELETE all but one invite of each group (the invitee can be re-invited).")
		fmt.Println()
	}

	// 3. Check schema_migrations rows >= 47
	type migRow struct {
		version  int
		name     string
		checksum string
		compat   string
	}
	var migRows []migRow
	mRows, err := db.QueryContext(ctx, `SELECT version, name, coalesce(checksum, ''), coalesce(compat, '') FROM schema_migrations WHERE version >= 47 ORDER BY version`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight: query schema_migrations: %v\n", err)
		os.Exit(1)
	}
	defer mRows.Close()
	for mRows.Next() {
		var r migRow
		if err := mRows.Scan(&r.version, &r.name, &r.checksum, &r.compat); err != nil {
			fmt.Fprintf(os.Stderr, "preflight: scan migration row: %v\n", err)
			os.Exit(1)
		}
		migRows = append(migRows, r)
	}
	if err := mRows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "preflight: iterate schema_migrations: %v\n", err)
		os.Exit(1)
	}

	if len(migRows) > 0 {
		fmt.Println("Schema migrations (version >= 47):")
		for _, r := range migRows {
			computed, known := database.MigrationChecksum(r.version)
			if !known {
				fmt.Printf("  v%d %s: newer than this binary (recorded=%s)\n", r.version, r.name, r.checksum)
				continue
			}
			if r.checksum != "" && r.checksum != computed {
				actionNeeded = true
				fmt.Printf("  v%d %s: DRIFT DETECTED (recorded=%s, computed=%s) -> adopt with `bakicli migrate adopt-checksum %d` if shape matches\n",
					r.version, r.name, r.checksum, computed, r.version)
			} else {
				fmt.Printf("  v%d %s: ok (checksum=%s)\n", r.version, r.name, r.checksum)
			}
		}
		fmt.Println()
	}

	if actionNeeded {
		fmt.Fprintln(os.Stderr, "preflight: action needed before deploy")
		os.Exit(1)
	}

	fmt.Println("preflight: clean — ready for deployment")
}

func printMigrateUsage() {
	fmt.Fprintln(os.Stderr, "usage: bakicli migrate <subcommand> [flags] [args]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  down [flags] <target-version>       roll the DB schema back (operator-only; never runs on boot)")
	fmt.Fprintln(os.Stderr, "  adopt-checksum [flags] <version>    adopt this binary's checksum for a drifted step")
	fmt.Fprintln(os.Stderr, "  preflight [flags]                   check pre-deploy migration readiness & drift")
}

func printMigrateDownUsage() {
	fmt.Fprintln(os.Stderr, "usage: bakicli migrate down [flags] <target-version>")
	fmt.Fprintln(os.Stderr, "  roll the DB schema back to <target-version> (operator-only; never runs on boot)")
	fmt.Fprintln(os.Stderr, "flags:")
	fmt.Fprintln(os.Stderr, "  -dsn string       Postgres DSN (default: $PAD_DATABASE_URL)")
	fmt.Fprintln(os.Stderr, "  -force            skip the interactive confirm prompt")
	fmt.Fprintln(os.Stderr, "  -max-steps int    refuse to roll back more than this many steps (default 3)")
}

func printMigrateAdoptChecksumUsage() {
	fmt.Fprintln(os.Stderr, "usage: bakicli migrate adopt-checksum [flags] <version>")
	fmt.Fprintln(os.Stderr, "  adopt this binary's computed checksum for a recorded migration step")
	fmt.Fprintln(os.Stderr, "flags:")
	fmt.Fprintln(os.Stderr, "  -dsn string       Postgres DSN (default: $PAD_DATABASE_URL)")
	fmt.Fprintln(os.Stderr, "  -force            skip the interactive confirm prompt")
}

func printMigratePreflightUsage() {
	fmt.Fprintln(os.Stderr, "usage: bakicli migrate preflight [flags]")
	fmt.Fprintln(os.Stderr, "  check for conditions migration 0048 refuses (case-duplicate emails, duplicate invite tokens) and migration drift")
	fmt.Fprintln(os.Stderr, "flags:")
	fmt.Fprintln(os.Stderr, "  -dsn string       Postgres DSN (default: $PAD_DATABASE_URL)")
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
	CustomRulesInline inlineRules `json:"customRulesInline,omitempty"`
}

type bakiRuleConf struct {
	Enabled  *bool  `json:"enabled,omitempty"`
	Severity string `json:"severity,omitempty"`
	// Options: a rule's thresholds; mirrors models.RuleConfig.Options and the org export's options.
	Options map[string]any `json:"options,omitempty"`
}

// buildDefaultConfig is what `bakicli init` writes: every built-in rule's
// default (enabled, severity, thresholds) from the rule registry.
func buildDefaultConfig() bakiConfig {
	cfg := bakiConfig{
		FailOn: "error",
		Format: "text",
	}
	defaults := analyzer.DefaultRuleConfigs()
	cfg.RuleCfg = make(map[string]bakiRuleConf, len(defaults))
	for id, d := range defaults {
		enabled := d.Enabled
		rc := bakiRuleConf{Severity: d.Severity, Enabled: &enabled}
		// Emit the thresholds the rule honours so `init` documents their defaults.
		if len(d.Options) > 0 {
			rc.Options = maps.Clone(d.Options)
		}
		cfg.RuleCfg[id] = rc
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
	fixRules, _ := selectRules("", customRulesPath, inlineRulesOf(cfg))
	return analyzer.FixOptions{
		RuleFilter: ruleFilter,
		Settings:   buildSettingsFromConfig(cfg),
		Rules:      fixRules,
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
	rules, _ := selectRules("", *customRulesFlag, inlineRulesOf(cfg))
	entries := analyzer.SuppressionInventory(doc, rules, buildSettingsFromConfig(cfg))

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
	var doc struct {
		models.AnalysisReport
		// Flows marks the several-flows document (cliMultiReport), which
		// would otherwise decode as an empty report without error.
		Flows json.RawMessage `json:"flows"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: invalid analysis report: %w", path, err)
	}
	if doc.Flows != nil {
		return nil, fmt.Errorf("%s is a several-flows report; this takes one flow's report (run bakicli with one path)", path)
	}
	return &doc.AnalysisReport, nil
}

func inlineRulesOf(cfg *bakiConfig) []analyzer.CustomRuleEntry {
	if cfg == nil {
		return nil
	}
	return cfg.CustomRulesInline
}
