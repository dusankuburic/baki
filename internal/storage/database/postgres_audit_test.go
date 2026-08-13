package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

func makeAuditEvents(n int, userID string) []*interfaces.AuditEvent {
	events := make([]*interfaces.AuditEvent, n)
	now := time.Now().UTC()
	for i := range events {
		events[i] = &interfaces.AuditEvent{
			// Namespace the primary key by userID so a unique per-run userID
			// yields unique event IDs — leftover rows from an interrupted run
			// can never collide on audit_events_pkey.
			ID:           fmt.Sprintf("%s-evt-%d", userID, i),
			UserID:       userID,
			Email:        "audit@example.com",
			Action:       "test.action",
			ResourceType: "flow",
			ResourceID:   fmt.Sprintf("res-%d", i),
			IP:           "127.0.0.1",
			Meta:         map[string]string{"seq": fmt.Sprintf("%d", i)},
			CreatedAt:    now,
		}
	}
	return events
}

// The Postgres wire protocol caps a statement at 65535 bind parameters. At
// auditInsertCols params/row a single INSERT must stay under 65535/9 ≈ 7281
// rows — SaveAuditEvents chunks larger batches to respect that ceiling.
func TestChunkAuditEvents_RespectsParamCeiling(t *testing.T) {
	cases := []struct {
		n          int
		wantChunks int
	}{
		{0, 1},    // empty → single (empty) batch
		{1, 1},    // under one batch
		{5000, 1}, // exactly one batch
		{5001, 2},
		{8000, 2},  // the regression case: > 7281 rows
		{12345, 3}, // ceil(12345/5000)
	}
	for _, tc := range cases {
		batches := chunkAuditEvents(makeAuditEvents(tc.n, "u"), auditInsertBatchRows)
		if len(batches) != tc.wantChunks {
			t.Errorf("chunkAuditEvents(n=%d): got %d chunks, want %d", tc.n, len(batches), tc.wantChunks)
		}
		var total int
		for _, b := range batches {
			if len(b) > auditInsertBatchRows {
				t.Errorf("chunkAuditEvents(n=%d): chunk of %d rows exceeds batch cap %d", tc.n, len(b), auditInsertBatchRows)
			}
			// Every chunk must stay under the 65535 bind-param ceiling.
			if got := len(b) * auditInsertCols; got > 65535 {
				t.Errorf("chunkAuditEvents(n=%d): chunk needs %d params, over 65535", tc.n, got)
			}
			total += len(b)
		}
		if tc.n > 0 && total != tc.n {
			t.Errorf("chunkAuditEvents(n=%d): chunks cover %d rows, want %d", tc.n, total, tc.n)
		}
	}
}

func TestBuildAuditInsert_ParamCount(t *testing.T) {
	const n = 5000
	query, args := buildAuditInsert(makeAuditEvents(n, "u"))
	if len(args) != n*auditInsertCols {
		t.Errorf("args = %d, want %d", len(args), n*auditInsertCols)
	}
	// n placeholder groups → n-1 comma separators between them.
	if got := strings.Count(query, "),("); got != n-1 {
		t.Errorf("placeholder groups: got %d separators, want %d", got, n-1)
	}
	// Highest placeholder index must be exactly n*cols and under the ceiling.
	last := fmt.Sprintf("$%d)", n*auditInsertCols)
	if !strings.HasSuffix(query, last) {
		t.Errorf("query should end with %q", last)
	}
	if n*auditInsertCols > 65535 {
		t.Fatalf("test batch itself exceeds param ceiling")
	}
}

// TestSaveAuditEvents_LargeBatch inserts more rows than a single statement's
// 65535-param ceiling allows, proving the chunking path round-trips without the
// "got N parameters but PostgreSQL only supports 65535" failure. Skipped unless
// DATABASE_URL is set.
func TestSaveAuditEvents_LargeBatch(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping audit large-batch integration test")
	}
	ctx := context.Background()
	b, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer b.Close()

	// Unique per run so leftover rows from an interrupted run can't collide.
	userID := "audit-large-batch-" + time.Now().Format("150405.000000000")
	// > 7281 rows: a single INSERT would overflow the bind-param ceiling.
	const n = 8000
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM audit_events WHERE user_id = $1`, userID)
	})

	if err := b.SaveAuditEvents(ctx, makeAuditEvents(n, userID)); err != nil {
		t.Fatalf("SaveAuditEvents(%d): %v", n, err)
	}

	var count int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n {
		t.Errorf("round-trip count = %d, want %d", count, n)
	}
}
