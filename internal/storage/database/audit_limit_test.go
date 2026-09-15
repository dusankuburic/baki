package database_test

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestListAuditEvents_OverMaxLimitClampsToMaxNotDefault pins the semantics of a
// cap.
//
// The clamp read `if limit <= 0 || limit > 500 { limit = 100 }` — so asking for
// MORE than the maximum silently gave you the DEFAULT, which is five times
// FEWER. That is the wrong failure mode for a cap, and it had a live victim:
// the GDPR data-subject export (postgres_retention.go) requests Limit: 1000 and
// was therefore shipping 100 audit events with no error and no truncation
// marker. A compliance export that silently omits records is worse than one
// that fails.
func TestListAuditEvents_OverMaxLimitClampsToMaxNotDefault(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	userID := "audit-limit-user-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() { cleanupUser(t, b, userID) })

	// Seed more than the default (100) but within the max (500), so the two
	// behaviours are distinguishable.
	const seeded = 150
	events := make([]*interfaces.AuditEvent, 0, seeded)
	for i := 0; i < seeded; i++ {
		events = append(events, &interfaces.AuditEvent{
			ID:           userID + "-" + time.Now().Format("150405.000000000") + "-" + itoa(int64(i)),
			UserID:       userID,
			Action:       "flow.analyze",
			ResourceType: "flow",
			ResourceID:   "f1",
			CreatedAt:    time.Now().UTC(),
		})
	}
	if err := b.SaveAuditEvents(ctx, events); err != nil {
		t.Fatalf("seed audit events: %v", err)
	}

	got, err := b.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: userID, Limit: 1000})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(got) == 100 {
		t.Fatalf("asking for 1000 returned exactly the DEFAULT of 100 — an over-max limit is "+
			"falling back to the default instead of clamping to the maximum; the DSAR export "+
			"silently truncates here (seeded %d)", seeded)
	}
	if len(got) != seeded {
		t.Errorf("got %d events, want all %d seeded (clamped to the 500 max)", len(got), seeded)
	}
}
