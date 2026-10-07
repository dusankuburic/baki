package di

import (
	"context"
	"testing"

	"go.uber.org/fx"

	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// syncDeliveries reads how many sync deliveries of flow.changed subscriber has had (the bus records each).
func syncDeliveries(t *testing.T, subscriber string) uint64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "pad_event_subscriber_duration_seconds" {
			continue
		}
	metric:
		for _, m := range f.GetMetric() {
			want := map[string]string{"event": "flow.changed", "subscriber": subscriber, "tier": "sync"}
			for _, l := range m.GetLabel() {
				if v, ok := want[l.GetName()]; ok && v != l.GetValue() {
					continue metric
				}
			}
			return m.GetHistogram().GetSampleCount()
		}
	}
	return 0
}

// The service graph subscribes every flow-derived cache to FlowChanged on the graph's bus, and the publishers
// announce on that same bus.
func TestServiceModule_SubscribesTheFlowCaches(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeCloud
	cfg.Auth.Enabled = true
	backend := testutil.NewFakeBackend()
	var (
		bus     *eventbus.Bus
		library *service.LibraryService
	)
	app := fx.New(
		fx.NopLogger,
		fx.Provide(
			func() context.Context { return context.Background() },
			func() *config.Config { return cfg },
			func(cfg *config.Config) config.DeploymentMode { return cfg.Mode },
			func() storageif.StorageBackend { return backend },
			func() service.EventNotifier { return service.NilNotifier{} },
			func() *collaboration.OrgService { return nil },
			func() service.KeyStore { return testutil.NewFakeSecretStore() },
		),
		ServiceModule,
		redisx.Module,
		fx.Populate(&bus, &library),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	rec := eventbus.NewRecorder(bus)
	names := []string{"dashboard-cache", "chat-context-cache", "flow-search-index", "flow-document-cache", "analyzer-report-cache"}
	before := map[string]uint64{}
	for _, n := range names {
		before[n] = syncDeliveries(t, n)
	}

	if _, err := library.CreateLibraryFlow(context.Background(), "u1", "", storageif.FlowDocument{Name: "f"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := eventbus.Recorded[events.FlowChanged](rec); len(got) != 1 || got[0].Kind != events.FlowCreated {
		t.Fatalf("the library announced %+v on the graph's bus, want one creation", got)
	}
	for _, n := range names {
		if got := syncDeliveries(t, n); got != before[n]+1 {
			t.Errorf("subscriber %q had %d deliveries, want %d", n, got-before[n], 1)
		}
	}
}
