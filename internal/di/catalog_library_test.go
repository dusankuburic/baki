package di

import (
	"testing"

	"pad-analyzer/internal/config"
	"pad-core/analyzer"
)

// TestProvideAnalysisService_ListsLibraryRulesWhereTheyRun: the library rules run
// only in folder batch analysis, which is refused with accounts, so only a
// deployment without accounts lists them in the rule catalog.
func TestProvideAnalysisService_ListsLibraryRulesWhereTheyRun(t *testing.T) {
	lib := analyzer.LibraryRules()
	if len(lib) == 0 {
		t.Fatal("no library rule registered")
	}
	for _, accounts := range []bool{true, false} {
		cfg := &config.Config{}
		cfg.Auth.Enabled = accounts
		svc, err := ProvideAnalysisService(nil, nil, nil, cfg, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("ProvideAnalysisService: %v", err)
		}
		listed := false
		for _, r := range svc.GetRules() {
			listed = listed || r.ID == lib[0].ID()
		}
		if listed == accounts {
			t.Errorf("accounts %v: library rule %s listed = %v", accounts, lib[0].ID(), listed)
		}
	}
}
