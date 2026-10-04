package di

import (
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
)

// TestDecorateAnalysisService_WiresAuthMode: with auth on, the service must
// know it, or an analysis without a user (the scanner's) broadcasts its
// progress to every connected client of every tenant.
func TestDecorateAnalysisService_WiresAuthMode(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		svc, err := service.NewAnalysisService(nil, nil, nil)
		if err != nil {
			t.Fatalf("NewAnalysisService: %v", err)
		}
		cfg := &config.Config{}
		cfg.Auth.Enabled = enabled
		got := decorateAnalysisService(cfg, nil, nil, svc)
		if got != svc {
			t.Fatal("the decorator must return the service it was given")
		}
		if got.AuthEnabled() != enabled {
			t.Errorf("auth enabled %v: AnalysisService.AuthEnabled() = %v", enabled, got.AuthEnabled())
		}
	}
}
