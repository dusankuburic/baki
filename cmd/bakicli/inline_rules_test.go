package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInlineCustomRules_GateThroughConfig drives the CI-parity path end to end
// through the BUILT BINARY: a `.bakirc.json` carrying inline custom rules (the
// exact shape GET /api/orgs/{id}/rules/export emits) must make the gate fail on
// a flow that violates the org's own rule.
//
// Through the BINARY (the package's shared TestMain builds it once), because
// the value being protected is "curl the export, run bakicli, get the org's
// gate" — an in-process call would not exercise config discovery, flag merging,
// or the exit code CI actually reads.
func TestInlineCustomRules_GateThroughConfig(t *testing.T) {
	dir := t.TempDir()

	flow := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(flow, []byte("#Region \"Main\"\n    SET Token TO 'abc'\n#EndRegion\n"), 0o600); err != nil {
		t.Fatalf("write flow: %v", err)
	}

	// Exactly what the export endpoint returns.
	cfg := map[string]any{
		"customRulesInline": []map[string]any{{
			"id": "acme-no-set", "name": "Acme bans SET",
			"description": "House policy", "severity": "error",
			"category": "Style", "rawTypeMatch": "^SET$",
		}},
	}
	cfgPath := filepath.Join(dir, ".bakirc.json")
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Without the config the flow passes an error-level gate.
	stdout, stderr, code := runBakicli(t, "-fail-on", "error", "-quiet", flow)
	out := stdout + stderr
	if code != 0 {
		t.Fatalf("baseline: expected a clean gate without the org's rule, got exit %d\n%s", code, out)
	}

	// With it, the org's own rule fails the build.
	stdout, stderr, code = runBakicli(t, "-config", cfgPath, "-fail-on", "error", flow)
	out = stdout + stderr
	if code != 1 {
		t.Fatalf("expected exit 1 from the org's inline rule, got %d\n%s", code, out)
	}
	// The human renderer prints a rule's NAME, not its id, so assert the id
	// against the machine format CI actually consumes.
	stdout, _, _ = runBakicli(t, "-config", cfgPath, "-format", "json", flow)
	var report struct {
		Findings []struct {
			RuleID   string `json:"ruleId"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode json report: %v\n%s", err, stdout)
	}
	found := false
	for _, f := range report.Findings {
		if f.RuleID == "acme-no-set" {
			found = true
			if f.Severity != "error" {
				t.Errorf("the org's rule reported severity %q, want error — the exported severity did not survive", f.Severity)
			}
		}
	}
	if !found {
		t.Errorf("the org's inline rule produced no finding:\n%s", stdout)
	}

	// An unparseable inline entry is SKIPPED WITH A WARNING, never silently —
	// otherwise a typo'd regex removes a rule the org believes is enforcing.
	badCfg := map[string]any{
		"customRulesInline": []map[string]any{{
			"id": "bad-regex", "name": "bad", "severity": "error", "rawTypeMatch": "^SET(",
		}},
	}
	badPath := filepath.Join(dir, "bad.json")
	raw, _ = json.Marshal(badCfg)
	if err := os.WriteFile(badPath, raw, 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	stdout, stderr, code = runBakicli(t, "-config", badPath, "-fail-on", "error", "-quiet", flow)
	out = stdout + stderr
	if code != 0 {
		t.Fatalf("a skipped rule should not fail the gate, got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "bad-regex") || !strings.Contains(out, "skipped") {
		t.Errorf("the skipped rule was not reported by id:\n%s", out)
	}
}
