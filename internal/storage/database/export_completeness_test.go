package database_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_UserScopedWalk_CompleteUnderConcurrentSave protects the
// enumeration ExportUserData and ExportUserDataStream depend on.
//
// Those two page through one user's flows with LIMIT/OFFSET, and the contract
// they state about themselves is absolute: "A data-subject export must be
// complete or fail." Ordering that walk on updated_at — a MUTABLE column —
// broke it: a flow saved while the export ran jumped to the front of the
// ordering and pushed the row on the next page boundary out of the walk. Every
// completeness guard downstream (the missing-blob check, the refuse-to-ship
// logic) then ran only over the flows that survived, so the export passed all
// of its own checks and was still short a flow. The user in another tab, or a
// collaborator on a shared flow, is enough to cause it.
//
// SCOPE: this reproduces the export's walk — same user-scoped filter, same
// ordering, a save interleaved BETWEEN pages — rather than calling
// ExportUserData itself. Interleaving is the whole mechanism, and there is no
// hook to inject a write midway through that function; a goroutine racing it
// would be timing-dependent and would pass or fail for the wrong reasons. What
// this pins is that the ordering those functions now request cannot be
// perturbed by a concurrent write.
func TestPostgres_UserScopedWalk_CompleteUnderConcurrentSave(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	run := time.Now().UnixNano()

	const seeded, pageSize = 30, 10
	user := &interfaces.User{
		ID:       "export-user-" + itoa(run),
		Email:    "export-" + itoa(run) + "@example.com",
		Password: "hash",
	}
	if err := b.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteUser(ctx, user.ID) })

	// A shared timestamp so a page boundary is guaranteed to sit inside a tied
	// group — the state a bulk import or a restored backup leaves behind.
	shared := time.Now().UTC().Truncate(time.Hour)
	want := make(map[string]bool, seeded)
	ids := make([]string, 0, seeded)
	for i := 0; i < seeded; i++ {
		id := fmt.Sprintf("export-flow-%s-%02d", itoa(run), i)
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: id, Name: "Export Flow", Content: json.RawMessage(`{}`),
			OwnerID: user.ID, UpdatedAt: shared,
		}); err != nil {
			t.Fatalf("SaveFlow %s: %v", id, err)
		}
		want[id] = true
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		for id := range want {
			_ = b.DeleteFlow(ctx, id)
		}
	})

	seen := map[string]int{}
	const maxPages = 200
	bumped := false
	for offset, pages := 0, 0; ; pages++ {
		if pages > maxPages {
			t.Fatalf("pagination did not terminate after %d pages", maxPages)
		}
		// The filter ExportUserData uses, including its ordering.
		page, err := b.ListFlows(ctx, interfaces.FlowFilter{
			UserID: user.ID, SortBy: interfaces.FlowSortIDAsc,
			Limit: pageSize, Offset: offset,
		})
		if err != nil {
			t.Fatalf("ListFlows(offset=%d): %v", offset, err)
		}
		if len(page) == 0 {
			break
		}
		for _, f := range page {
			seen[f.ID]++
		}
		offset += len(page)

		// After the first page: the user saves a flow the walk has already
		// passed. Under an updated_at ordering that relocates it to the front
		// and shifts every row still to come.
		if !bumped {
			bumped = true
			existing, err := b.LoadFlow(ctx, ids[0])
			if err != nil {
				t.Fatalf("LoadFlow for the concurrent save: %v", err)
			}
			existing.UpdatedAt = time.Now().UTC()
			if err := b.SaveFlow(ctx, existing); err != nil {
				t.Fatalf("concurrent SaveFlow: %v", err)
			}
		}

		if len(page) < pageSize {
			break
		}
	}

	var missed, duped []string
	for id := range want {
		switch n := seen[id]; {
		case n == 0:
			missed = append(missed, id)
		case n > 1:
			duped = append(duped, fmt.Sprintf("%s x%d", id, n))
		}
	}
	if len(missed) > 0 {
		t.Errorf("a save during the walk dropped %d of %d flows from a data-subject export: %v", len(missed), seeded, missed)
	}
	if len(duped) > 0 {
		t.Errorf("a save during the walk duplicated %d flow(s) in a data-subject export: %v", len(duped), duped)
	}
}
