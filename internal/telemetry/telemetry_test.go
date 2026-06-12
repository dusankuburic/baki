package telemetry

import (
	"context"
	"testing"
)

func TestInit_NoEndpoint(t *testing.T) {
	t.Parallel()

	shutdown, err := Init(context.Background(), "test-service", "0.0.1", "")
	if err != nil {
		t.Fatalf("expected no error with empty endpoint, got %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}
	shutdown()
}
