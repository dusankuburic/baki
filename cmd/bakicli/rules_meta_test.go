package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

// TestCLI_RulesJSONCarriesStageAndKind: the catalog says when a rule arrived,
// whether it gates (stage) and what it checks (kind); the library rules are
// listed because a run over several flows runs them.
func TestCLI_RulesJSONCarriesStageAndKind(t *testing.T) {
	stdout, _, code := runBakicli(t, "rules", "-format", "json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Fatal(err)
	}
	if want := len(analyzer.AllRules()) + len(analyzer.LibraryRules()); len(entries) != want {
		t.Errorf("%d entries, want %d", len(entries), want)
	}
	byID := map[string]map[string]any{}
	for _, e := range entries {
		byID[e["id"].(string)] = e
		for _, key := range []string{"stage", "introducedIn", "kind"} {
			if _, ok := e[key]; !ok {
				t.Errorf("%s lacks %q", e["id"], key)
			}
		}
	}
	for id, want := range map[string]map[string]any{
		"unhandled-error":                {"stage": "stable", "kind": "block", "introducedIn": 1.0},
		"circular-subflow-dependency":    {"stage": "stable", "kind": "flow"},
		"duplicate-subflow-across-flows": {"stage": "preview", "kind": "library", "introducedIn": 1.0},
		"subflow-mismatch":               {"autoFix": "append-output"},
	} {
		for k, v := range want {
			if byID[id][k] != v {
				t.Errorf("%s.%s = %v, want %v", id, k, byID[id][k], v)
			}
		}
	}
}

// captureStdoutFile is captureStdout through a file: a pipe nobody drains blocks
// a large write on Windows (the rules table).
func captureStdoutFile(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	fn()
	os.Stdout = old
	_ = f.Close()
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestDescribeRule_StageAndIntroduced(t *testing.T) {
	out := captureStdoutFile(t, func() { describeRule("unhandled-error") })
	for _, want := range []string{"Kind:       block\n", "Stage:      stable\n", "Introduced: rule set 1\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("describe lacks %q:\n%s", want, out)
		}
	}
	out = captureStdoutFile(t, func() { describeRule("duplicate-subflow-across-flows") })
	if !strings.Contains(out, "Stage:      preview (reported, never gates)") || !strings.Contains(out, "Kind:       library") {
		t.Errorf("library rule describe:\n%s", out)
	}
	defer flipToPreview(t, "missing-timeout")()
	if out := captureStdoutFile(t, printRulesTable); !strings.Contains(out, " preview ") || !strings.Contains(out, "STAGE") {
		t.Errorf("table lacks the stage column:\n%s", out)
	}
}

// TestBuildDefaultConfig_MatchesDefaultSettings: `init` now reads the rule
// registry; what it writes must equal what the DefaultSettings copy said.
func TestBuildDefaultConfig_MatchesDefaultSettings(t *testing.T) {
	cfg := buildDefaultConfig()
	defaults := models.DefaultSettings().Analysis.Rules
	if len(cfg.RuleCfg) != len(analyzer.AllRules()) || len(cfg.RuleCfg) != len(defaults) {
		t.Fatalf("%d rules in init, %d registered, %d in DefaultSettings", len(cfg.RuleCfg), len(analyzer.AllRules()), len(defaults))
	}
	for id, rc := range cfg.RuleCfg {
		d := defaults[id]
		if *rc.Enabled != d.Enabled || rc.Severity != d.Severity {
			t.Errorf("%s: init {%v %s}, DefaultSettings {%v %s}", id, *rc.Enabled, rc.Severity, d.Enabled, d.Severity)
		}
		got, _ := json.Marshal(rc.Options)
		want, _ := json.Marshal(d.Options)
		if len(rc.Options)+len(d.Options) > 0 && string(got) != string(want) {
			t.Errorf("%s options: init %s, DefaultSettings %s", id, got, want)
		}
	}
}

// TestCLI_InitCountsRules: "wrote … (0 rules)" counted the empty -rules string.
func TestCLI_InitCountsRules(t *testing.T) {
	stdout, _, code := runBakicli(t, "init", "-o", filepath.Join(t.TempDir(), ".bakirc.json"))
	if want := fmt.Sprintf("(%d rules)", len(analyzer.AllRules())); code != 0 || !strings.Contains(stdout, want) {
		t.Errorf("exit %d stdout %q, want %q", code, stdout, want)
	}
}
