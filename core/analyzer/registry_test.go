package analyzer

import (
	"testing"
)

func TestAllRules_ReturnsNonEmptyList(t *testing.T) {
	rules := AllRules()
	if len(rules) == 0 {
		t.Fatal("expected non-empty rules list")
	}
}

func TestAllRules_AllHaveIDs(t *testing.T) {
	rules := AllRules()
	for _, r := range rules {
		if r.ID() == "" {
			t.Errorf("rule %T has empty ID", r)
		}
		if r.Name() == "" {
			t.Errorf("rule %T has empty Name", r)
		}
	}
}

func TestAllRules_NoDuplicateIDs(t *testing.T) {
	rules := AllRules()
	seen := make(map[string]bool)
	for _, r := range rules {
		id := r.ID()
		if seen[id] {
			t.Errorf("duplicate rule ID: %s", id)
		}
		seen[id] = true
	}
}
