package metrics_test

// A package that registers its own metrics (metrics.MustRegister) is imported here, blank, so the metrics golden,
// built in this test binary, lists them; TestRegistration_OnlyThroughThePackageRegistry names a missing one.

import (
	"errors"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	_ "pad-analyzer/internal/ai"
	_ "pad-analyzer/internal/api"
	_ "pad-analyzer/internal/api/middleware"
	_ "pad-analyzer/internal/audit"
	_ "pad-analyzer/internal/errreport"
	_ "pad-analyzer/internal/invalidation"
	_ "pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/metrics"
	_ "pad-analyzer/internal/platform/eventbus"
	_ "pad-analyzer/internal/rag"
	_ "pad-analyzer/internal/redisx"
	_ "pad-analyzer/internal/service"
	_ "pad-analyzer/internal/storage/database"
	_ "pad-analyzer/internal/websocket"
)

// The registry records the package whose code called it, not this package's own registration functions.
func TestRegister_RecordsTheCallersPackage(t *testing.T) {
	r := metrics.NewTestRegistry()
	r.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "test_external_total", Help: "A test counter."}))
	if got, want := r.Packages(), []string{"pad-analyzer/internal/metrics_test"}; !slices.Equal(got, want) {
		t.Errorf("packages = %v, want %v", got, want)
	}
}

func TestRegister_RefusesADuplicate(t *testing.T) {
	var are prometheus.AlreadyRegisteredError
	if err := metrics.Register(metrics.ACentralCollector()); !errors.As(err, &are) {
		t.Errorf("Register(an already registered metric) = %v, want AlreadyRegisteredError", err)
	}
	clash := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "http_requests_total", Help: "Another help."}, []string{"x"})
	if err := metrics.Register(clash); err == nil {
		t.Error("Register(a second metric under an existing name) succeeded")
	}
	defer func() {
		if recover() == nil {
			t.Error("MustRegister of a duplicate did not panic")
		}
	}()
	metrics.MustRegister(metrics.ACentralCollector())
}
