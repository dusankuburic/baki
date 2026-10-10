package metrics

import "github.com/prometheus/client_golang/prometheus"

// NewTestRegistry returns an empty registry that records registrations like the package registry, for the tests
// outside the package.
func NewTestRegistry() *moduleRegistry { return newModuleRegistry() }

// Packages returns the package that registered each collector on r, in order.
func (r *moduleRegistry) Packages() []string {
	var out []string
	for _, reg := range r.registrations() {
		out = append(out, reg.pkg)
	}
	return out
}

// ACentralCollector returns a collector this package registered at init, for the duplicate-registration tests
// outside the package.
func ACentralCollector() prometheus.Collector { return onBehalf[0] }
