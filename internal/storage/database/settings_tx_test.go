package database_test

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_Settings_JoinTheRequestTransaction proves the settings writes
// participate in the caller's RLS transaction instead of committing on their own
// pooled connection.
//
// They used to use b.db directly. rlsMiddleware wraps a request in a
// transaction and rolls it back when the handler answers >=400 or panics — but a
// write on b.db is not in that transaction, so the rollback left the settings
// change behind. Every other tenant-scoped storage method goes through
// b.query(ctx); these four were the outliers.
//
// The test rolls back explicitly rather than driving a failing HTTP request, so
// it pins the storage-layer property directly.
func TestPostgres_Settings_JoinTheRequestTransaction(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	run := time.Now().UnixNano()
	user := &interfaces.User{
		ID:       "settings-tx-user-" + itoa(run),
		Email:    "settings-tx-" + itoa(run) + "@example.com",
		Password: "hash",
	}
	if err := b.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteUser(ctx, user.ID) })

	// Baseline: a committed write is durable.
	if err := b.SaveUserSettings(ctx, user.ID, &interfaces.AppSettings{Version: 7}); err != nil {
		t.Fatalf("SaveUserSettings (autocommit): %v", err)
	}
	got, err := b.LoadUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("LoadUserSettings: %v", err)
	}
	if got.Version != 7 {
		t.Fatalf("baseline: got Version=%d, want 7", got.Version)
	}

	// Now write inside a transaction and roll it back. If the write escapes the
	// transaction it survives, which is the bug.
	tx, err := b.BeginRLS(ctx, user.ID)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	txCtx := database.WithRLSTx(ctx, tx)
	if err := b.SaveUserSettings(txCtx, user.ID, &interfaces.AppSettings{Version: 99}); err != nil {
		tx.Rollback()
		t.Fatalf("SaveUserSettings (in tx): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	after, err := b.LoadUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("LoadUserSettings after rollback: %v", err)
	}
	if after.Version != 7 {
		t.Errorf("settings write survived a rolled-back transaction: got Version=%d, want 7 — the write is not joining the request's RLS transaction", after.Version)
	}
}
