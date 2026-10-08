package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// findFlow returns the flow with the given id from a list, or nil.
func findFlow(flows []*interfaces.FlowDocument, id string) *interfaces.FlowDocument {
	for _, f := range flows {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// runFlowScenarios holds the flow listing's filters and the optimistic save.
func runFlowScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("ListFlows_MetadataOnly_omits_content_keeps_metadata", func(t *testing.T) {
		owner := "contract-owner-" + runID
		flow := &interfaces.FlowDocument{
			ID:       "contract-flow-meta-" + runID,
			Name:     "Meta Flow",
			Content:  json.RawMessage(`{"big":"` + strings.Repeat("x", 1000) + `"}`),
			Metadata: interfaces.FlowMetadata{BlockCount: 7, SubflowCount: 2},
			OwnerID:  owner,
		}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		metaOnly, err := b.ListFlows(ctx, interfaces.FlowFilter{UserID: owner, MetadataOnly: true})
		if err != nil {
			t.Fatalf("ListFlows meta-only: %v", err)
		}
		found := findFlow(metaOnly, flow.ID)
		if found == nil {
			t.Fatalf("flow %s not present in meta-only list", flow.ID)
		}
		if len(found.Content) != 0 {
			t.Errorf("MetadataOnly: expected empty content, got %d bytes", len(found.Content))
		}
		if found.Metadata.BlockCount != 7 {
			t.Errorf("MetadataOnly: metadata lost, BlockCount=%d want 7", found.Metadata.BlockCount)
		}

		full, err := b.ListFlows(ctx, interfaces.FlowFilter{UserID: owner})
		if err != nil {
			t.Fatalf("ListFlows full: %v", err)
		}
		foundFull := findFlow(full, flow.ID)
		if foundFull == nil {
			t.Fatalf("flow %s not present in full list", flow.ID)
		}
		if len(foundFull.Content) == 0 {
			t.Errorf("full list: expected content present, got empty")
		}
	})

	run("LoadFlowHeader_returns_metadata_without_content", func(t *testing.T) {
		owner := "contract-hdr-owner-" + runID
		flow := &interfaces.FlowDocument{
			ID:       "contract-flow-hdr-" + runID,
			Name:     "Header Flow",
			Content:  json.RawMessage(`{"big":"` + strings.Repeat("y", 1000) + `"}`),
			Source:   strings.Repeat("Variables.SetVariable Variable: x Value: 1\n", 20),
			Metadata: interfaces.FlowMetadata{BlockCount: 5, SubflowCount: 1},
			OwnerID:  owner,
		}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		hdr, err := b.LoadFlowHeader(ctx, flow.ID)
		if err != nil {
			t.Fatalf("LoadFlowHeader: %v", err)
		}
		if len(hdr.Content) != 0 {
			t.Errorf("LoadFlowHeader: expected nil content, got %d bytes", len(hdr.Content))
		}
		if hdr.Source != "" {
			t.Errorf("LoadFlowHeader: expected empty source, got %d bytes", len(hdr.Source))
		}
		if hdr.OwnerID != owner {
			t.Errorf("LoadFlowHeader: OwnerID=%q want %q", hdr.OwnerID, owner)
		}
		if hdr.Metadata.BlockCount != 5 {
			t.Errorf("LoadFlowHeader: metadata lost, BlockCount=%d want 5", hdr.Metadata.BlockCount)
		}

		full, err := b.LoadFlow(ctx, flow.ID)
		if err != nil {
			t.Fatalf("LoadFlow: %v", err)
		}
		if len(full.Content) == 0 {
			t.Errorf("LoadFlow: expected content present, got empty")
		}
		if full.Source != flow.Source {
			t.Errorf("LoadFlow: source not round-tripped (%d bytes, want %d)", len(full.Source), len(flow.Source))
		}

		if _, err := b.LoadFlowHeader(ctx, "no-such-flow-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("LoadFlowHeader(missing): expected ErrNotFound, got %v", err)
		}
	})

	run("ListFlows_AfterID_keyset_pages_by_id", func(t *testing.T) {
		// ScopeFlowIDs walks a scope with ORDER BY id + AfterID instead of
		// OFFSET; both backends must return exactly the ids after the cursor.
		owner := "contract-keyset-owner-" + runID
		ids := []string{"contract-ks-a-" + runID, "contract-ks-b-" + runID, "contract-ks-c-" + runID}
		for _, id := range ids {
			if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: id, OwnerID: owner}); err != nil {
				t.Fatalf("SaveFlow %s: %v", id, err)
			}
		}
		page, err := b.ListFlows(ctx, interfaces.FlowFilter{
			UserID: owner, SortBy: interfaces.FlowSortIDAsc, AfterID: ids[0], Limit: 10, MetadataOnly: true,
		})
		if err != nil {
			t.Fatalf("ListFlows(AfterID): %v", err)
		}
		var got []string
		for _, f := range page {
			if f.OwnerID == owner {
				got = append(got, f.ID)
			}
		}
		if len(got) != 2 || got[0] != ids[1] || got[1] != ids[2] {
			t.Errorf("ListFlows(AfterID=%s) = %v, want %v", ids[0], got, ids[1:])
		}
		n, err := b.CountFlows(ctx, interfaces.FlowFilter{UserID: owner, AfterID: ids[1]})
		if err != nil {
			t.Fatalf("CountFlows(AfterID): %v", err)
		}
		if n != 1 {
			t.Errorf("CountFlows(AfterID=%s) = %d, want 1", ids[1], n)
		}
	})

	run("ListFlows_unscoped_matches_nothing_AllFlows_enumerates", func(t *testing.T) {
		// Both real backends must agree: an unscoped filter is a defense-in-depth no-match, while
		// AllFlows enumerates every flow (the migrator depends on that). Assertions are robust against
		// a shared/long-lived Postgres DB: exact counts only under the unscoped guard, presence-only under AllFlows.
		owner := "contract-allflows-owner-" + runID
		flow := &interfaces.FlowDocument{
			ID:      "contract-flow-allflows-" + runID,
			Name:    "AllFlows Flow",
			Content: json.RawMessage(`{"k":"v"}`),
			OwnerID: owner,
		}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		// Unscoped: 1=0 / matchesFilter→false. Returns nothing on both backends
		// regardless of pre-existing rows, so an exact length check is safe.
		unscoped, err := b.ListFlows(ctx, interfaces.FlowFilter{})
		if err != nil {
			t.Fatalf("ListFlows unscoped: %v", err)
		}
		if len(unscoped) != 0 {
			t.Errorf("unscoped filter: expected 0 flows (guard), got %d", len(unscoped))
		}

		// AllFlows: the seeded flow must be present. Other rows may exist on a
		// shared DB, so assert presence rather than an exact count.
		all, err := b.ListFlows(ctx, interfaces.FlowFilter{AllFlows: true})
		if err != nil {
			t.Fatalf("ListFlows AllFlows: %v", err)
		}
		if findFlow(all, flow.ID) == nil {
			t.Errorf("AllFlows: seeded flow %s missing from enumeration", flow.ID)
		}
	})

	run("SaveFlow_stale_version_returns_ErrVersionConflict", func(t *testing.T) {
		// OCC is strict on both backends: an update whose Version does not match the current stored
		// version fails — there is no "0 skips the check" escape hatch; updaters must load first.
		owner := "contract-occ-owner-" + runID
		flow := &interfaces.FlowDocument{
			ID:      "contract-flow-occ-" + runID,
			Name:    "OCC Flow",
			Content: json.RawMessage(`{"k":1}`),
			OwnerID: owner,
		}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow create: %v", err)
		}
		flow.Content = json.RawMessage(`{"k":2}`)
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow update (current version): %v", err)
		}

		stale := &interfaces.FlowDocument{
			ID:      flow.ID,
			Name:    "OCC Flow stale",
			Content: json.RawMessage(`{"k":3}`),
			OwnerID: owner,
			Version: flow.Version - 1,
		}
		if err := b.SaveFlow(ctx, stale); !errors.Is(err, interfaces.ErrVersionConflict) {
			t.Errorf("stale version: expected ErrVersionConflict, got %v", err)
		}
	})
}

// runFlowWalkScenarios holds the whole-table walks of the migrator and the governance scanner.
func runFlowWalkScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	// A LIMIT/OFFSET walk of the whole table must visit every flow exactly once — the storage migrator
	// and the governance scanner both enumerate that way, and in the migrator a skipped row is silent data
	// loss. Flows are seeded with a SHARED UpdatedAt on purpose: ties are ordinary (the FS backend never
	// stamps UpdatedAt itself) and its slice came from a map range into a non-stable sort, so tied rows
	// once shifted across page boundaries. Assertions count only this run's flows (persistent DB safe).
	run("ListFlows_paginated_walk_visits_every_flow_once", func(t *testing.T) {
		const seeded, pageSize = 25, 10
		owner := "contract-page-owner-" + runID
		shared := time.Now().UTC().Truncate(time.Hour)

		want := make(map[string]bool, seeded)
		for i := 0; i < seeded; i++ {
			id := fmt.Sprintf("contract-page-%s-%02d", runID, i)
			if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
				ID: id, Name: "Paged Flow", Content: json.RawMessage(`{}`),
				OwnerID: owner, UpdatedAt: shared,
			}); err != nil {
				t.Fatalf("SaveFlow %s: %v", id, err)
			}
			want[id] = true
		}
		t.Cleanup(func() {
			for id := range want {
				_ = b.DeleteFlow(ctx, id)
			}
		})

		seen := map[string]int{}
		// maxPages bounds the walk so a pagination bug that never advances
		// fails with this message instead of hanging the suite.
		const maxPages = 500
		for offset, pages := 0, 0; ; pages++ {
			if pages > maxPages {
				t.Fatalf("pagination did not terminate after %d pages", maxPages)
			}
			page, err := b.ListFlows(ctx, interfaces.FlowFilter{
				AllFlows: true, Limit: pageSize, Offset: offset, MetadataOnly: true,
			})
			if err != nil {
				t.Fatalf("ListFlows(offset=%d): %v", offset, err)
			}
			if len(page) == 0 {
				break
			}
			for _, f := range page {
				if want[f.ID] {
					seen[f.ID]++
				}
			}
			offset += len(page)
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
			t.Errorf("paginated walk skipped %d of %d flows: %v", len(missed), seeded, missed)
		}
		if len(duped) > 0 {
			t.Errorf("paginated walk returned %d flow(s) more than once: %v", len(duped), duped)
		}
	})

	// A full-table walk must survive a concurrent save — the governance scanner sweeps while users edit,
	// and the migrator can run against a live source. FlowSortIDAsc exists for exactly this: the default
	// updated_at DESC is a MUTABLE sort key, so a flow saved mid-walk jumps to the front and pushes the row
	// on the next page boundary out of the walk (a unique tiebreaker does not help — the row genuinely moved).
	run("ListFlows_paginated_walk_survives_a_concurrent_save", func(t *testing.T) {
		const seeded, pageSize = 24, 8
		owner := "contract-cwalk-owner-" + runID
		shared := time.Now().UTC().Truncate(time.Hour)

		ids := make([]string, 0, seeded)
		want := make(map[string]bool, seeded)
		for i := 0; i < seeded; i++ {
			id := fmt.Sprintf("contract-cwalk-%s-%02d", runID, i)
			if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
				ID: id, Name: "Concurrent Walk", Content: json.RawMessage(`{}`),
				OwnerID: owner, UpdatedAt: shared,
			}); err != nil {
				t.Fatalf("SaveFlow %s: %v", id, err)
			}
			ids = append(ids, id)
			want[id] = true
		}
		t.Cleanup(func() {
			for id := range want {
				_ = b.DeleteFlow(ctx, id)
			}
		})

		seen := map[string]int{}
		const maxPages = 500
		bumped := false
		for offset, pages := 0, 0; ; pages++ {
			if pages > maxPages {
				t.Fatalf("pagination did not terminate after %d pages", maxPages)
			}
			page, err := b.ListFlows(ctx, interfaces.FlowFilter{
				AllFlows: true, SortBy: interfaces.FlowSortIDAsc,
				Limit: pageSize, Offset: offset, MetadataOnly: true,
			})
			if err != nil {
				t.Fatalf("ListFlows(offset=%d): %v", offset, err)
			}
			if len(page) == 0 {
				break
			}
			for _, f := range page {
				if want[f.ID] {
					seen[f.ID]++
				}
			}
			offset += len(page)

			// After the first page, a user saves one of the flows the walk has ALREADY passed —
			// under a mutable ordering that re-sorts it to the front and shifts everything still to come.
			if !bumped {
				bumped = true
				existing, err := b.LoadFlow(ctx, ids[0])
				if err != nil {
					t.Fatalf("LoadFlow for concurrent save: %v", err)
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
			t.Errorf("a save during the walk made it skip %d of %d flows: %v", len(missed), seeded, missed)
		}
		if len(duped) > 0 {
			t.Errorf("a save during the walk made it return %d flow(s) twice: %v", len(duped), duped)
		}
	})
}
