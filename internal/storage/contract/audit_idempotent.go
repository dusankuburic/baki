package contract

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

func init() {
	auditScenarios = append(auditScenarios, struct {
		name string
		run  func(t *testing.T, store interfaces.AuditStore)
	}{"same_event_id_twice_stores_one_row", auditSameIDTwice})
}

// auditSameIDTwice: an audit event is written at least once (a spill file is replayed after a crash, or adopted by a
// second replica), so writing an id that is stored already is a no-op that returns nil, for the single save and the
// batch, and the first write's row is the one kept.
func auditSameIDTwice(t *testing.T, store interfaces.AuditStore) {
	ctx := context.Background()
	user := "contract-audit-idem-" + time.Now().Format("150405.000000")
	first := auditEvent(user, "contract.idempotent", time.Now())
	if err := store.SaveAuditEvent(ctx, first); err != nil {
		t.Fatalf("SaveAuditEvent: %v", err)
	}
	again := *first
	again.Action = "contract.idempotent.changed"
	if err := store.SaveAuditEvent(ctx, &again); err != nil {
		t.Fatalf("SaveAuditEvent of a stored id = %v, want nil", err)
	}
	if err := store.SaveAuditEvents(ctx, []*interfaces.AuditEvent{first, &again}); err != nil {
		t.Fatalf("SaveAuditEvents of stored ids = %v, want nil", err)
	}
	// Twice inside one batch is the same no-op.
	twin := auditEvent(user, "contract.idempotent.twin", time.Now())
	if err := store.SaveAuditEvents(ctx, []*interfaces.AuditEvent{twin, twin}); err != nil {
		t.Fatalf("SaveAuditEvents with an id twice = %v, want nil", err)
	}
	rows, err := store.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: user, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("listed %d rows, want 2 (one per id)", len(rows))
	}
	for _, r := range rows {
		if r.ID == first.ID && r.Action != "contract.idempotent" {
			t.Errorf("the stored row's action = %q, want the first write's", r.Action)
		}
	}
}
