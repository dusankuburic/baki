package contract

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runReturnShapeScenarios holds the return shapes every backend shares: ErrNotFound for a missing record, an empty slice (never nil) for an empty list.
func runReturnShapeScenarios(ctx context.Context, b interfaces.StorageBackend, run func(string, func(*testing.T))) {
	run("LoadConversation_missing_returns_empty_slice_not_nil", func(t *testing.T) {
		msgs, err := b.LoadConversation(ctx, "no-such-flow", "scope")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msgs == nil {
			t.Errorf("missing conversation: expected non-nil empty slice (matches filesystem semantics), got nil")
		}
		if len(msgs) != 0 {
			t.Errorf("missing conversation: expected empty, got %d messages", len(msgs))
		}
	})

	run("LoadFlow_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadFlow(ctx, "no-such-flow-id")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing flow: expected ErrNotFound, got %v", err)
		}
	})

	run("LoadUserByEmail_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadUserByEmail(ctx, "no-such@example.com")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing user (by email): expected ErrNotFound, got %v", err)
		}
	})

	run("LoadUserByID_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadUserByID(ctx, "no-such-user-id")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing user (by id): expected ErrNotFound, got %v", err)
		}
	})

	run("LoadOrg_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadOrg(ctx, "no-such-org")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing org: expected ErrNotFound, got %v", err)
		}
	})

	run("ListCollaborators_empty_returns_non_nil_slice", func(t *testing.T) {
		// Same nil-vs-empty contract as LoadConversation: callers should be
		// able to do `len(coll) == 0` without backend-specific nil checks.
		coll, err := b.ListCollaborators(ctx, "no-such-flow", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if coll == nil {
			t.Errorf("empty collaborators: expected non-nil empty slice, got nil")
		}
		if len(coll) != 0 {
			t.Errorf("empty collaborators: expected length 0, got %d", len(coll))
		}
	})
}
