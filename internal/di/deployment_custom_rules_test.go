package di

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/config"
)

// captureLogs runs fn with slog writing text lines into the returned buffer.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func logLine(logs, attr string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, attr) {
			return line
		}
	}
	return ""
}

// A PAD_CUSTOM_RULES entry that loads with a warning (an unknown field) must not be logged as skipped; one that
// is skipped says so. Each line names the entry, the rule, the field and the reason as attributes.
func TestProvideDeploymentCustomRules_WarningsSayWhetherTheRuleLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	doc := `[{"id":"ok","nameMatch":"x","nameMatches":"typo"},{"id":"bad","nameMatch":"*["},{"kind":"flow","id":"future"}]`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Server.CustomRulesPath = path
	var rules []string
	logs := captureLogs(t, func() {
		for _, r := range ProvideDeploymentCustomRules(cfg) {
			rules = append(rules, r.ID())
		}
	})

	if len(rules) != 1 || rules[0] != "ok" {
		t.Fatalf("loaded %v, want only %q", rules, "ok")
	}
	for _, want := range []struct{ rule, msg, index, field, reason string }{
		{"ok", `msg="custom rule warning"`, "index=0", "field=nameMatches", "reason=UNKNOWN_FIELD"},
		{"bad", `msg="custom rule skipped"`, "index=1", "field=nameMatch", "reason=INVALID_PATTERN"},
		{"future", `msg="custom rule skipped"`, "index=2", "field=kind", "reason=UNKNOWN_KIND"},
	} {
		line := logLine(logs, "rule="+want.rule+" ")
		for _, part := range []string{"level=WARN", want.msg, want.index, want.field, want.reason} {
			if !strings.Contains(line, part) {
				t.Errorf("rule %s: log line %q lacks %s", want.rule, line, part)
			}
		}
	}
}

// A named file that does not exist is a WARN naming it (it used to be silent: a stale path ran without the
// operator's rules); an unset path logs nothing.
func TestProvideDeploymentCustomRules_MissingFileWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	cfg := &config.Config{}
	cfg.Server.CustomRulesPath = path
	logs := captureLogs(t, func() {
		if rules := ProvideDeploymentCustomRules(cfg); rules != nil {
			t.Errorf("a missing file gave %d rules", len(rules))
		}
	})
	if line := logLine(logs, "custom rules file not found"); !strings.Contains(line, "level=WARN") || !strings.Contains(line, "missing.json") {
		t.Errorf("missing file: log = %q, want a WARN naming the path", logs)
	}

	cfg.Server.CustomRulesPath = ""
	if logs := captureLogs(t, func() { ProvideDeploymentCustomRules(cfg) }); logs != "" {
		t.Errorf("an unset path logged %q", logs)
	}
}
