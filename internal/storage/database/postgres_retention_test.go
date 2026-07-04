package database_test

import (
	"context"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_DeleteUser_ErasesOwnedData verifies that DeleteUser removes the
// user and their owned flows (and is idempotent / safe on a missing user).
func TestPostgres_DeleteUser_ErasesOwnedData(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	uid := "del-user-1"
	if err := b.SaveUser(ctx, &interfaces.User{ID: uid, Email: "del@example.com", Password: "x"}); err != nil {
		t.Fatalf("SaveUser: %v", err)
	}
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: "del-flow-1", Name: "owned", OwnerID: uid, Content: []byte(`{}`)}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	_ = b.SaveAuditEvent(ctx, &interfaces.AuditEvent{ID: "del-evt-1", UserID: uid, Email: "del@example.com", Action: "auth.login", IP: "1.2.3.4"})

	if err := b.DeleteUser(ctx, uid); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	if u, err := b.LoadUserByID(ctx, uid); err == nil || u != nil {
		t.Errorf("expected user erased, got u=%v err=%v", u, err)
	}
	flows, err := b.ListFlows(ctx, interfaces.FlowFilter{UserID: uid})
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	if len(flows) != 0 {
		t.Errorf("expected owned flows erased, got %d", len(flows))
	}

	// Idempotent: deleting again is not an error.
	if err := b.DeleteUser(ctx, uid); err != nil {
		t.Errorf("DeleteUser idempotent: %v", err)
	}
}

// TestPostgres_ExportUserData verifies the DSAR export returns the user and their
// owned flows, and returns ErrNotFound once erased.
func TestPostgres_ExportUserData(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	uid := "exp-user-1"
	if err := b.SaveUser(ctx, &interfaces.User{ID: uid, Email: "exp@example.com", Password: "x"}); err != nil {
		t.Fatalf("SaveUser: %v", err)
	}
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: "exp-flow-1", Name: "owned", OwnerID: uid, Content: []byte(`{}`)}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}

	export, err := b.ExportUserData(ctx, uid)
	if err != nil {
		t.Fatalf("ExportUserData: %v", err)
	}
	if export.User == nil || export.User.ID != uid {
		t.Errorf("export user = %+v", export.User)
	}
	if len(export.Flows) != 1 {
		t.Errorf("expected 1 owned flow in export, got %d", len(export.Flows))
	}
	if export.ExportedAt.IsZero() {
		t.Error("expected ExportedAt set")
	}

	_ = b.DeleteUser(ctx, uid)
	if _, err := b.ExportUserData(ctx, uid); err == nil {
		t.Error("expected ErrNotFound after deletion, got nil")
	}
}

// TestPostgres_PurgeExpiredData_Smoke verifies the purge query set is valid and
// returns a result without error (counts depend on DB state).
func TestPostgres_PurgeExpiredData_Smoke(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	res, err := b.PurgeExpiredData(ctx, 365)
	if err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil PurgeResult")
	}
}
