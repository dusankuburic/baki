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

// listFlowsWithTotalSQL must be the list statement with exactly one extra
// select column: the same WHERE (same args, same order) so the total always
// describes exactly the rows the page filtered.
func TestListFlowsWithTotal_SQLShape(t *testing.T) {
	filter := interfaces.FlowFilter{
		UserID: "u1", Query: "etl", Limit: 25, Offset: 50,
		SortBy: interfaces.FlowSortBlocksDesc, MetadataOnly: true,
	}
	listQ, listArgs := listFlowsSQL(filter)
	totalQ, totalArgs := listFlowsWithTotalSQL(filter)

	if !strings.Contains(totalQ, "COUNT(*) OVER() AS page_total") {
		t.Errorf("total query missing COUNT(*) OVER(): %s", totalQ)
	}
	if listQ != strings.Replace(totalQ, ", COUNT(*) OVER() AS page_total", "", 1) {
		t.Errorf("total query is not the list query plus one column:\nlist:  %s\ntotal: %s", listQ, totalQ)
	}
	if len(totalArgs) != len(listArgs) {
		t.Fatalf("arg count diverged: list %d, total %d", len(listArgs), len(totalArgs))
	}
	for i := range listArgs {
		if listArgs[i] != totalArgs[i] {
			t.Errorf("arg %d diverged: list %v, total %v", i, listArgs[i], totalArgs[i])
		}
	}
}

// The v30 index set: both tiebroken definitions present, the superseded
// gov_alerts_created_idx gone. Gated on DATABASE_URL (the podman harness).
func TestMigration_V30_TiebreakIndexes_Postgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping v30 index test")
	}
	ctx := context.Background()
	b, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	var version int
	if err := b.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if version < 30 {
		t.Fatalf("schema version = %d, want >= 30", version)
	}

	def := func(index string) string {
		t.Helper()
		var d string
		if err := b.db.QueryRowContext(ctx,
			`SELECT indexdef FROM pg_indexes WHERE indexname = $1`, index).Scan(&d); err != nil {
			t.Fatalf("index %s: %v", index, err)
		}
		return d
	}
	if d := def("gov_alerts_created_id_idx"); !strings.Contains(d, "created_at DESC, id") {
		t.Errorf("gov_alerts_created_id_idx = %s, want (created_at DESC, id)", d)
	}
	if d := def("flows_blockcount_updated_idx"); !strings.Contains(d, "id)") {
		t.Errorf("flows_blockcount_updated_idx = %s, want the definition tiebroken with id", d)
	}
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'gov_alerts_created_idx'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("gov_alerts_created_idx still exists — the redundant index was not dropped")
	}
}

// ListFlowsWithTotal must agree with the two-statement path it replaces:
// same docs, and a total that ignores Limit/Offset.
func TestListFlowsWithTotal_Postgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping ListFlowsWithTotal test")
	}
	ctx := context.Background()
	b, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	stamp := time.Now().UnixNano()
	owner := fmt.Sprintf("lft-owner-%d", stamp)
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(ctx, `DELETE FROM flows WHERE owner_id = $1`, owner)
	})
	for i := 1; i <= 3; i++ {
		_, err := b.db.ExecContext(ctx, `INSERT INTO flows (id, name, content, owner_id, org_id)
			VALUES ($1, $2, '{}'::jsonb, $3, '') ON CONFLICT (id) DO NOTHING`,
			fmt.Sprintf("lft-%d-%d", stamp, i), fmt.Sprintf("Flow %d", i), owner)
		if err != nil {
			t.Fatalf("seed flow %d: %v", i, err)
		}
	}

	filter := interfaces.FlowFilter{
		UserID: owner, Limit: 2, SortBy: interfaces.FlowSortUpdatedDesc, MetadataOnly: true,
	}
	docs, total, err := b.ListFlowsWithTotal(ctx, filter)
	if err != nil {
		t.Fatalf("ListFlowsWithTotal: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("docs = %d, want the page size 2", len(docs))
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 (unpaginated)", total)
	}
	for _, d := range docs {
		if d.OwnerID != owner {
			t.Errorf("doc %s owner = %q, want %q", d.ID, d.OwnerID, owner)
		}
		if d.Content != nil {
			t.Errorf("doc %s has Content despite MetadataOnly", d.ID)
		}
	}

	// Same rows the two-statement path returns.
	listDocs, err := b.ListFlows(ctx, filter)
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	if len(listDocs) != len(docs) {
		t.Fatalf("ListFlows returned %d docs, ListFlowsWithTotal %d", len(listDocs), len(docs))
	}
	for i := range docs {
		if docs[i].ID != listDocs[i].ID {
			t.Errorf("row %d: total-path id %s, list-path id %s", i, docs[i].ID, listDocs[i].ID)
		}
	}
}

// A redundant re-persist of an unchanged history must not rewrite the row
// (the IS DISTINCT FROM guard skips the TOAST write): updated_at stays put
// when the messages are byte-identical and moves only on a real change.
func TestSaveConversation_SkipsIdenticalRewrite_Postgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping conversation rewrite test")
	}
	ctx := context.Background()
	b, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	flowID := fmt.Sprintf("conv-rt-%d", time.Now().UnixNano())
	const scope = "provider-1"
	msgs := []interfaces.ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	t.Cleanup(func() { _ = b.DeleteConversation(ctx, flowID, scope) })

	if err := b.SaveConversation(ctx, flowID, scope, msgs); err != nil {
		t.Fatalf("first save: %v", err)
	}
	updated := func() time.Time {
		t.Helper()
		var ts time.Time
		if err := b.db.QueryRowContext(ctx,
			`SELECT updated_at FROM conversations WHERE flow_id = $1 AND scope = $2`, flowID, scope).Scan(&ts); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		return ts
	}
	first := updated()

	// Identical re-save: the guard must skip the write, so updated_at (part
	// of the same row write) cannot move. Sleep so a rewrite would be visible.
	time.Sleep(20 * time.Millisecond)
	if err := b.SaveConversation(ctx, flowID, scope, msgs); err != nil {
		t.Fatalf("identical re-save: %v", err)
	}
	if again := updated(); !again.Equal(first) {
		t.Errorf("identical re-save moved updated_at %v -> %v: the IS DISTINCT FROM guard is not applied", first, again)
	}

	// A real change writes.
	msgs = append(msgs, interfaces.ChatMessage{Role: "user", Content: "next question"})
	if err := b.SaveConversation(ctx, flowID, scope, msgs); err != nil {
		t.Fatalf("changed save: %v", err)
	}
	if third := updated(); !third.After(first) {
		t.Errorf("changed save did not move updated_at (%v -> %v)", first, third)
	}
	loaded, err := b.LoadConversation(ctx, flowID, scope)
	if err != nil {
		t.Fatalf("LoadConversation: %v", err)
	}
	if len(loaded) != 3 || loaded[2].Content != "next question" {
		t.Errorf("loaded %d messages, want the appended turn stored", len(loaded))
	}
}
