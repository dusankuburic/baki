package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"strings"

	"pad-core/analyzer"
)

// ---- bakicli rules ----

func runRules(args []string) {
	fs := flag.NewFlagSet("rules", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		switch fs.Arg(0) {
		case "test":
			runRulesTest(fs.Args()[1:])
			return
		case "lint":
			runRulesLint(fs.Args()[1:])
			return
		}
	}
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

// catalogRule is one rule `bakicli rules` lists: the built-ins, then the
// library rules (they run when several flows are analyzed together).
type catalogRule struct {
	rule analyzer.Rule
	meta analyzer.RuleMeta
	kind string
}

func ruleCatalog() []catalogRule {
	var out []catalogRule
	for _, r := range analyzer.AllRules() {
		out = append(out, catalogRule{rule: r, meta: analyzer.MetaOf(r), kind: ruleKind(r)})
	}
	for _, r := range analyzer.LibraryRules() {
		out = append(out, catalogRule{rule: r, meta: analyzer.MetaOf(r), kind: ruleKind(r)})
	}
	return out
}

// ruleKind is what a rule checks at a time: a block, a flow, or a set of flows.
func ruleKind(r analyzer.Rule) string {
	switch r.(type) {
	case analyzer.LibraryRule:
		return "library"
	case analyzer.FlowRule:
		return "flow"
	default:
		return "block"
	}
}

// autoFix is the fix the rule's findings may carry: its engine-stamped fix,
// else the fixes its check stamps itself (subflow-mismatch).
func (c catalogRule) autoFix() string {
	if c.meta.AutoFix != "" {
		return c.meta.AutoFix
	}
	return strings.Join(c.meta.DirectFixes, ",")
}

func (c catalogRule) stage() string {
	if c.meta.Stage == "" {
		return string(analyzer.StageStable)
	}
	return string(c.meta.Stage)
}

func printRulesTable() {
	rules := ruleCatalog()
	const row = "%-30s %-8s %-14s %-10s %-7s %s\n"
	fmt.Printf(row, "RULE", "SEVERITY", "CATEGORY", "AUTO-FIX", "STAGE", "NAME")
	fmt.Printf(row, strings.Repeat("-", 30), strings.Repeat("-", 8), strings.Repeat("-", 14), strings.Repeat("-", 10), strings.Repeat("-", 7), strings.Repeat("-", 30))
	fixable, preview := 0, 0
	for _, c := range rules {
		fix := c.autoFix()
		if fix == "" {
			fix = "-"
		} else {
			fixable++
		}
		if c.meta.Stage == analyzer.StagePreview {
			preview++
		}
		fmt.Printf(row, c.rule.ID(), c.rule.DefaultSeverity(), c.rule.Category(), fix, c.stage(), c.rule.Name())
	}
	fmt.Printf("\n%d rules (%d with auto-fix, %d preview)\n", len(rules), fixable, preview)
}

func printRulesJSON() {
	type ruleEntry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Category    string `json:"category"`
		Severity    string `json:"defaultSeverity"`
		Confidence  string `json:"confidence"`
		AutoFix     string `json:"autoFix,omitempty"`
		// Kind: block, flow or library (cross-flow; runs with several flows).
		Kind string `json:"kind"`
		// Stage: stable or preview (reported, never gates).
		Stage string `json:"stage"`
		// IntroducedIn: the rule-set version that added the rule.
		IntroducedIn int `json:"introducedIn"`
		// Options: tunable thresholds — the keys .bakirc.json ruleConfig.<id>.options accepts.
		Options map[string]any `json:"options,omitempty"`
	}
	defaults := analyzer.DefaultRuleConfigs()
	rules := ruleCatalog()
	entries := make([]ruleEntry, 0, len(rules))
	for _, c := range rules {
		e := ruleEntry{
			ID:           c.rule.ID(),
			Name:         c.rule.Name(),
			Description:  c.rule.Description(),
			Category:     c.rule.Category(),
			Severity:     string(c.rule.DefaultSeverity()),
			Confidence:   string(analyzer.RuleConfidence(c.rule.ID())),
			AutoFix:      c.autoFix(),
			Kind:         c.kind,
			Stage:        c.stage(),
			IntroducedIn: c.meta.IntroducedIn,
		}
		if rc, ok := defaults[c.rule.ID()]; ok && len(rc.Options) > 0 {
			e.Options = maps.Clone(rc.Options)
		}
		entries = append(entries, e)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(entries)
}

func describeRule(ruleID string) {
	for _, c := range ruleCatalog() {
		r := c.rule
		if r.ID() != ruleID {
			continue
		}
		fmt.Printf("Rule:       %s\n", r.ID())
		fmt.Printf("Name:       %s\n", r.Name())
		fmt.Printf("Category:   %s\n", r.Category())
		fmt.Printf("Severity:   %s\n", r.DefaultSeverity())
		fmt.Printf("Confidence: %s\n", analyzer.RuleConfidence(r.ID()))
		if fix := c.autoFix(); fix != "" {
			fmt.Printf("Auto-fix:   %s\n", fix)
		} else {
			fmt.Printf("Auto-fix:   (none)\n")
		}
		switch c.kind {
		case "library":
			fmt.Printf("Kind:       library (runs across the flows of one run: several path arguments)\n")
		default:
			fmt.Printf("Kind:       %s\n", c.kind)
		}
		if c.meta.Stage == analyzer.StagePreview {
			fmt.Printf("Stage:      preview (reported, never gates)\n")
		} else {
			fmt.Printf("Stage:      %s\n", c.stage())
		}
		fmt.Printf("Introduced: rule set %d\n", c.meta.IntroducedIn)
		fmt.Printf("\n%s\n", r.Description())
		return
	}
	fmt.Fprintf(os.Stderr, "bakicli rules: unknown rule %q (run 'bakicli rules' to list all rules)\n", ruleID)
	os.Exit(2)
}
