package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"pad-core/analyzer"
	"pad-core/models"
)

// selectRules assembles the run's rules: the built-ins, the -custom-rules file
// and the config's inline rules, narrowed by -rules; and the library rules
// -rules leaves in. Exit 2 on a custom rule this bakicli cannot read, a named
// custom-rules file that does not exist, or a -rules list matching nothing.
func selectRules(rulesFlag, customRulesPath string, inline []analyzer.CustomRuleConfig) ([]analyzer.Rule, []analyzer.LibraryRule) {
	all := append(analyzer.AllRules(), loadCustomRules(customRulesPath, inline)...)
	library := analyzer.LibraryRules()
	if rulesFlag == "" {
		return all, library
	}
	wanted := make(map[string]bool)
	for _, id := range strings.Split(rulesFlag, ",") {
		wanted[strings.TrimSpace(id)] = true
	}
	out := []analyzer.Rule{}
	for _, r := range all {
		if wanted[r.ID()] {
			out = append(out, r)
		}
	}
	var lib []analyzer.LibraryRule
	for _, r := range library {
		if wanted[r.ID()] {
			lib = append(lib, r)
		}
	}
	if len(out) == 0 && len(lib) == 0 {
		fmt.Fprintf(os.Stderr, "bakicli: -rules %q matched no known rules\n", rulesFlag)
		os.Exit(2)
	}
	return out, lib
}

// loadCustomRules compiles the custom rules of a run tolerantly (ADR 0015): an
// unknown field, a missing id or a built-in id is a warning and the rule
// runs; an invalid value skips the rule with a warning. A rule of a kind or
// schemaVersion this bakicli does not run, a named file that does not exist,
// or a file that is not valid JSON exits 2: running without the rule would
// weaken the gate silently.
func loadCustomRules(path string, inline []analyzer.CustomRuleConfig) []analyzer.Rule {
	var out []analyzer.Rule
	tooNew := 0
	compile := func(source string, index int, cfg analyzer.CustomRuleConfig) {
		r, warnings, err := analyzer.CompileCustomRule(cfg, analyzer.CustomRuleTolerant)
		id := cfg.ID
		if id == "" {
			id = fmt.Sprintf("#%d (no id)", index)
		}
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "bakicli: %s: warning: entry %d (rule %q): %v\n", source, index, id, w)
		}
		var ce *analyzer.CustomRuleError
		switch {
		case errors.As(err, &ce) && needsNewerBakicli(ce, cfg):
			tooNew++
			fmt.Fprintf(os.Stderr, "bakicli: %s: entry %d (rule %q): %v; %s\n", source, index, id, err, newerBakicliHint)
		case err != nil:
			fmt.Fprintf(os.Stderr, "bakicli: %s: entry %d (rule %q) skipped: %v\n", source, index, id, err)
		default:
			out = append(out, r)
		}
	}
	if path != "" {
		entries, err := readRulesDocument(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(os.Stderr, "bakicli: -custom-rules: %s does not exist (a named custom-rules file must exist; the run would gate without its rules)\n", path)
			os.Exit(2)
		case err != nil:
			fmt.Fprintf(os.Stderr, "bakicli: -custom-rules: %v\n", err)
			os.Exit(2)
		}
		source := "-custom-rules: " + filepath.Base(path)
		// A JSON type error anywhere fails the file, as when it was one json.Unmarshal; a newer release's rule
		// whose fields do not decode is one this bakicli cannot run.
		for _, e := range entries {
			switch {
			case e.Err.IsNewerRelease():
				tooNew++
				fmt.Fprintf(os.Stderr, "bakicli: %s: entry %d (rule %q): %v; %s\n", source, e.Index, e.Err.RuleID, e.Err, newerBakicliHint)
			case e.Err != nil:
				fmt.Fprintf(os.Stderr, "bakicli: %s: entry %d (rule %q): %v\n", source, e.Index, e.Err.RuleID, e.Err)
				os.Exit(2)
			}
		}
		for _, e := range entries {
			if e.Err == nil {
				compile(source, e.Index, e.Config)
			}
		}
	}
	for i, cfg := range inline {
		compile("config customRulesInline", i, cfg)
	}
	if tooNew > 0 {
		fmt.Fprintf(os.Stderr, "bakicli: %d custom rule(s) need a newer bakicli\n", tooNew)
		os.Exit(2)
	}
	return out
}

const newerBakicliHint = "the rule needs a newer bakicli"

// needsNewerBakicli: a kind, or a schemaVersion above the newest, that this
// build cannot run (every mode refuses it; running it as a block rule would
// match the wrong blocks). A negative schemaVersion is just invalid.
func needsNewerBakicli(p *analyzer.CustomRuleError, cfg analyzer.CustomRuleConfig) bool {
	switch p.Reason {
	case analyzer.ReasonUnknownKind:
		return true
	case analyzer.ReasonUnsupportedVersion:
		return cfg.SchemaVersion > analyzer.CustomRuleSchemaVersion
	}
	return false
}

// ---- bakicli rules test ----

// runRulesTest tries custom rules against a flow, checked strictly (as the
// API's validate and test endpoints). Exit 2 on an invalid rule file, 1 on no
// match with -fail-on-none.
func runRulesTest(args []string) {
	fs := flag.NewFlagSet("rules test", flag.ExitOnError)
	failOnNone := fs.Bool("fail-on-none", false, "exit 1 when no rule matched")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: bakicli rules test <rules.json> <flow.txt|folder|->")
		fmt.Fprintln(os.Stderr, "  <rules.json>: custom-rule JSON — an array, a single rule object, or {\"rules\": …}")
		os.Exit(2)
	}
	rulesPath, flowTarget := fs.Arg(0), fs.Arg(1)

	entries, err := readRulesDocument(rulesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli: rules test: %v\n", err)
		os.Exit(2)
	}
	var rules []analyzer.Rule
	invalid := 0
	for _, e := range entries {
		if e.Err != nil {
			fmt.Fprintf(os.Stderr, "bakicli: rules test: entry %d (id %q) invalid: %v\n", e.Index, e.Err.RuleID, e.Err)
			invalid++
			continue
		}
		r, warnings, cerr := analyzer.CompileCustomRule(e.Config, analyzer.CustomRuleStrict)
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "bakicli: rules test: entry %d (id %q): warning: %v\n", e.Index, e.Config.ID, w)
		}
		if cerr != nil {
			hint := ""
			var ce *analyzer.CustomRuleError
			if errors.As(cerr, &ce) && needsNewerBakicli(ce, e.Config) {
				hint = "; " + newerBakicliHint
			}
			fmt.Fprintf(os.Stderr, "bakicli: rules test: entry %d (id %q) invalid: %v%s\n", e.Index, e.Config.ID, cerr, hint)
			invalid++
			continue
		}
		rules = append(rules, r)
	}
	if invalid > 0 {
		fmt.Fprintf(os.Stderr, "bakicli: rules test: %d of %d rule(s) invalid\n", invalid, len(entries))
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

// readRulesDocument reads a custom-rules document (an array, one rule object
// with an id, or {"rules": …}); each entry decodes on its own.
func readRulesDocument(path string) ([]analyzer.CustomRuleEntry, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, err
	}
	entries, err := analyzer.DecodeCustomRuleDocument(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, nil
}

// ---- bakicli rules lint ----

// lintProblem is one problem `rules lint` reports.
type lintProblem struct {
	File     string `json:"file"`
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Field    string `json:"field,omitempty"`
	Reason   string `json:"reason"`
	Severity string `json:"severity"` // error or warning
	Message  string `json:"message"`
}

type lintResult struct {
	Rules    int           `json:"rules"`
	Errors   int           `json:"errors"`
	Warnings int           `json:"warnings"`
	Problems []lintProblem `json:"problems"`
}

func (l *lintResult) add(file string, p *analyzer.CustomRuleError, index int, newer bool) {
	sev := "error"
	if p.Warning {
		sev = "warning"
		l.Warnings++
	} else {
		l.Errors++
	}
	msg := p.Error()
	if newer {
		msg += "; " + newerBakicliHint
	}
	l.Problems = append(l.Problems, lintProblem{File: file, Index: index, ID: p.RuleID, Field: p.Field,
		Reason: p.Reason, Severity: sev, Message: msg})
}

// runRulesLint checks custom rules strictly, without a flow, and prints every
// problem, warnings (a rule without a matcher) included. The file is a rules
// document or a .bakirc.json, whose customRulesInline and customRules file are
// checked. Exit 0 clean (warnings allowed), 1 on errors, 2 when the file
// cannot be read.
func runRulesLint(args []string) {
	fs := flag.NewFlagSet("rules lint", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli rules lint [-format text|json] <rules.json|.bakirc.json>")
		fmt.Fprintln(os.Stderr, "  check custom rules strictly: exit 0 clean, 1 on errors, 2 when the file cannot be read")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 || (*format != "text" && *format != "json") {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli rules lint: %v\n", err)
		os.Exit(2)
	}
	res, err := lintRules(path, data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli rules lint: %v\n", err)
		os.Exit(2)
	}

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		for _, p := range res.Problems {
			where := p.File
			if p.Index >= 0 {
				id := p.ID
				if id == "" {
					id = "(no id)"
				}
				where = fmt.Sprintf("%s: entry %d (rule %s)", p.File, p.Index, id)
			}
			field := ""
			if p.Field != "" {
				field = " " + p.Field
			}
			fmt.Printf("%s: %s %s%s: %s\n", where, p.Severity, p.Reason, field, p.Message)
		}
		fmt.Printf("%s: %d rule(s), %d error(s), %d warning(s)\n", path, res.Rules, res.Errors, res.Warnings)
	}
	if res.Errors > 0 {
		os.Exit(1)
	}
}

// lintRules lints data, a rules document or a .bakirc.json read from path. The
// error is a customRules file the config names that cannot be read.
func lintRules(path string, data []byte) (lintResult, error) {
	res := lintResult{Problems: []lintProblem{}}
	cfg, ok := asConfigDocument(path, data)
	if !ok {
		res.lintDocument(path, data)
		return res, nil
	}
	for i, raw := range cfg.CustomRulesInline {
		res.lintEntry(path, i, raw)
	}
	if cfg.CustomRules != "" {
		// Resolved like analyze does: against the working directory.
		rulesData, err := os.ReadFile(cfg.CustomRules) // #nosec G304 -- path named by the operator's config
		if err != nil {
			return res, fmt.Errorf("%s: customRules: %w", path, err)
		}
		res.lintDocument(cfg.CustomRules, rulesData)
	}
	return res, nil
}

func (l *lintResult) lintDocument(file string, data []byte) {
	entries, err := analyzer.DecodeCustomRuleDocument(data)
	if err != nil {
		var ce *analyzer.CustomRuleError
		if !errors.As(err, &ce) {
			ce = &analyzer.CustomRuleError{Index: -1, Reason: analyzer.ReasonInvalidJSON, Msg: err.Error(), Err: err}
		}
		l.add(file, ce, -1, false)
		return
	}
	for _, e := range entries {
		l.Rules++
		if e.Err != nil {
			l.add(file, e.Err, e.Index, e.Err.IsNewerRelease())
			continue
		}
		for _, p := range analyzer.CheckCustomRule(e.Config, analyzer.CustomRuleStrict) {
			l.add(file, p, e.Index, needsNewerBakicli(p, e.Config))
		}
	}
}

func (l *lintResult) lintEntry(file string, index int, raw json.RawMessage) {
	l.Rules++
	cfg, err := analyzer.DecodeCustomRule(raw)
	if err != nil {
		var ce *analyzer.CustomRuleError
		if !errors.As(err, &ce) {
			ce = &analyzer.CustomRuleError{Index: index, Reason: analyzer.ReasonInvalidJSON, Msg: err.Error(), Err: err}
		}
		l.add(file, ce, index, ce.IsNewerRelease())
		return
	}
	for _, p := range analyzer.CheckCustomRule(cfg, analyzer.CustomRuleStrict) {
		l.add(file, p, index, needsNewerBakicli(p, cfg))
	}
}

// lintConfig is the part of a .bakirc.json that carries custom rules.
type lintConfig struct {
	CustomRules       string            `json:"customRules"`
	CustomRulesInline []json.RawMessage `json:"customRulesInline"`
}

// asConfigDocument recognises a .bakirc.json: an object that is not itself a
// rule (no id) and is named .bakirc.json or carries a config key (a "rules"
// string is the config's rule list; a "rules" array is a rules envelope). A
// config without custom rules lints clean.
func asConfigDocument(path string, data []byte) (lintConfig, bool) {
	var keys map[string]json.RawMessage
	if json.Unmarshal(data, &keys) != nil {
		return lintConfig{}, false
	}
	if _, id := keys["id"]; id {
		return lintConfig{}, false
	}
	isConfig := filepath.Base(path) == ".bakirc.json"
	for _, k := range []string{"failOn", "format", "customRules", "customRulesInline", "policy", "verbose", "ruleConfig"} {
		if _, ok := keys[k]; ok {
			isConfig = true
		}
	}
	if r, ok := keys["rules"]; ok {
		isConfig = len(r) > 0 && r[0] == '"'
	}
	if !isConfig {
		return lintConfig{}, false
	}
	var cfg lintConfig
	if json.Unmarshal(data, &cfg) != nil {
		return lintConfig{}, false
	}
	return cfg, true
}
