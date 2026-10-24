package di

import (
	"context"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil/redisreplica"
	"pad-core/models"
)

// TestProvideAnalysisService_WiresAuthMode: with auth on, the service must
// know it, or an analysis without a user (the scanner's) broadcasts its
// progress to every connected client of every tenant.
func TestProvideAnalysisService_WiresAuthMode(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		cfg := &config.Config{}
		cfg.Auth.Enabled = enabled
		svc, err := ProvideAnalysisService(nil, nil, nil, cfg, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("ProvideAnalysisService: %v", err)
		}
		if svc.AuthEnabled() != enabled {
			t.Errorf("auth enabled %v: AnalysisService.AuthEnabled() = %v", enabled, svc.AuthEnabled())
		}
	}
}

// TestProvideAnalysisService_SharesItsRunsOnlyWithSeveralReplicas: the report pair and the record claims go to Redis
// only where a second replica can analyze the flow (PAD_MAX_REPLICAS > 1); a single replica keeps them in memory even
// with PAD_REDIS_URL set, so a Redis outage costs it nothing.
func TestProvideAnalysisService_SharesItsRunsOnlyWithSeveralReplicas(t *testing.T) {
	for _, tc := range []struct {
		replicas int
		want     bool
	}{{0, false}, {1, false}, {2, true}} {
		c := redisreplica.New(t, 1)
		cfg := config.Default()
		cfg.Storage.MaxReplicas = tc.replicas
		svc, err := ProvideAnalysisService(service.NilNotifier{}, staticSettings{}, nil, cfg, nil, nil, nil, nil, c.Replicas[0].Client)
		if err != nil {
			t.Fatal(err)
		}
		doc := &models.FlowDocument{ID: "flow-1", Name: "flow-1", Subflows: []models.Subflow{{ID: "main", Name: "Main"}}}
		if _, err := svc.AnalyzeFlow(context.Background(), doc); err != nil {
			t.Fatal(err)
		}
		if got := len(c.Redis.Keys()) > 0; got != tc.want {
			t.Errorf("replicas %d: the analysis wrote to Redis = %v, want %v (keys %v)", tc.replicas, got, tc.want, c.Redis.Keys())
		}
	}
}

// staticSettings is a SettingsProvider answering the defaults and ignoring writes.
type staticSettings struct{}

func (staticSettings) Get() *models.AppSettings                         { return models.DefaultSettings() }
func (staticSettings) Update(context.Context, models.AppSettings) error { return nil }
func (staticSettings) AddRecentFile(string, int64) error                { return nil }
func (staticSettings) RemoveRecentFile(string) error                    { return nil }
func (staticSettings) ClearRecentFiles() error                          { return nil }
