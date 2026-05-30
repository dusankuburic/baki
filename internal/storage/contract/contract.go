// Package contract provides a shared test suite that both StorageBackend
// implementations (filesystem and database/postgres) run against, so the two
// can't quietly drift apart on return-shape semantics like
//
//   * "not found" → nil vs interfaces.ErrNotFound
//   * "empty list" → nil slice vs non-nil empty slice
//
// The historical motivation: `LoadConversation` returned a non-nil empty
// slice on the filesystem backend and a `nil` slice on Postgres, so frontend
// nil-checks branched differently per deployment.
//
// This package is not a `_test` package: each storage-backend test file
// imports it and calls RunSuite from a Test* function. That way the same
// scenarios run in CI under both backends (Postgres is opt-in via
// DATABASE_URL).
package contract

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/storage/interfaces"
)

// RunSuite executes the cross-backend contract scenarios against b. The
// suite seeds uuid-prefixed fixtures, so it is safe to re-run against a
// backend with existing state (e.g. a long-lived Postgres DB used across
// test sessions). The "first user becomes admin" scenario is skipped if
// the backend already has users.
func RunSuite(t *testing.T, b interfaces.StorageBackend) {
	t.Helper()
	ctx := context.Background()
	runID := uuid.New().String()[:8]

	t.Run("LoadConversation_missing_returns_empty_slice_not_nil", func(t *testing.T) {
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

	t.Run("LoadFlow_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadFlow(ctx, "no-such-flow-id")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing flow: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("LoadUserByEmail_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadUserByEmail(ctx, "no-such@example.com")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing user (by email): expected ErrNotFound, got %v", err)
		}
	})

	t.Run("LoadUserByID_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadUserByID(ctx, "no-such-user-id")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing user (by id): expected ErrNotFound, got %v", err)
		}
	})

	t.Run("LoadOrg_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadOrg(ctx, "no-such-org")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing org: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("ListCollaborators_empty_returns_non_nil_slice", func(t *testing.T) {
		// Same nil-vs-empty contract as LoadConversation: callers should be
		// able to do `len(coll) == 0` without backend-specific nil checks.
		coll, err := b.ListCollaborators(ctx, "no-such-flow")
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

	t.Run("CreateUser_promotes_first_to_admin", func(t *testing.T) {
		// Only meaningful on a truly empty backend. Skip when prior state
		// exists (e.g. a Postgres DB shared across test runs).
		count, err := b.CountUsers(ctx)
		if err != nil {
			t.Fatalf("CountUsers: %v", err)
		}
		if count > 0 {
			t.Skip("backend has existing users; cannot exercise first-admin promotion")
		}

		first := &interfaces.User{
			ID:       "contract-user-1-" + runID,
			Email:    "contract-1-" + runID + "@example.com",
			Password: "hash",
		}
		if err := b.CreateUser(ctx, first); err != nil {
			t.Fatalf("CreateUser first: %v", err)
		}
		if string(first.Role) != "admin" {
			t.Errorf("first user: expected role=admin, got %q", first.Role)
		}

		second := &interfaces.User{
			ID:       "contract-user-2-" + runID,
			Email:    "contract-2-" + runID + "@example.com",
			Password: "hash",
		}
		if err := b.CreateUser(ctx, second); err != nil {
			t.Fatalf("CreateUser second: %v", err)
		}
		if string(second.Role) == "admin" {
			t.Errorf("second user: expected non-admin role, got admin")
		}
	})

	t.Run("CreateUser_duplicate_email_returns_ErrEmailExists", func(t *testing.T) {
		email := "contract-dup-" + runID + "@example.com"
		u := &interfaces.User{
			ID:       "contract-user-dup-a-" + runID,
			Email:    email,
			Password: "hash",
		}
		if err := b.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser first: %v", err)
		}
		dup := &interfaces.User{
			ID:       "contract-user-dup-b-" + runID,
			Email:    email,
			Password: "hash",
		}
		err := b.CreateUser(ctx, dup)
		if !errors.Is(err, interfaces.ErrEmailExists) {
			t.Errorf("duplicate email: expected ErrEmailExists, got %v", err)
		}
	})

	t.Run("SaveConversation_then_LoadConversation_round_trips", func(t *testing.T) {
		flowID := "contract-flow-" + runID
		msgs := []interfaces.ChatMessage{
			{ID: "m1", Role: "user", Content: "hi", Timestamp: time.Now().UTC().Format(time.RFC3339)},
			{ID: "m2", Role: "assistant", Content: "hello", Timestamp: time.Now().UTC().Format(time.RFC3339)},
		}
		if err := b.SaveConversation(ctx, flowID, "scope", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		got, err := b.LoadConversation(ctx, flowID, "scope")
		if err != nil {
			t.Fatalf("LoadConversation: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("round-trip: want 2 messages, got %d", len(got))
		}
	})
}
