package di

import (
	"testing"

	"pad-analyzer/internal/config"
)

// TestProvideAnalysisService_WiresAuthMode: with auth on, the service must
// know it, or an analysis without a user (the scanner's) broadcasts its
// progress to every connected client of every tenant.
func TestProvideAnalysisService_WiresAuthMode(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		cfg := &config.Config{}
		cfg.Auth.Enabled = enabled
		svc, err := ProvideAnalysisService(nil, nil, nil, cfg, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("ProvideAnalysisService: %v", err)
		}
		if svc.AuthEnabled() != enabled {
			t.Errorf("auth enabled %v: AnalysisService.AuthEnabled() = %v", enabled, svc.AuthEnabled())
		}
	}
}
