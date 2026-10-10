package app

import (
	"testing"

	"go.uber.org/fx"
)

// TestAppGraph: every provider's and invoke's dependencies are satisfiable. fx.ValidateApp checks the graph's
// types without calling any constructor, so it needs no database or config; a missing or duplicated provider
// used to surface only when the binary booted. (Deployment modes change values, not the graph's types.) The root
// package checks the same graph with the build stamps main passes.
func TestAppGraph(t *testing.T) {
	if err := fx.ValidateApp(Options(Build{})...); err != nil {
		t.Fatal(err)
	}
}
