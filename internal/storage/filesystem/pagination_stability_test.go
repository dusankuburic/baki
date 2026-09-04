package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestListFlows_PaginationVisitsEveryFlow pins the property the storage
// migrator and the governance scanner both depend on: walking ListFlows with
// LIMIT/OFFSET until a short page must visit every flow exactly once.
//
// It seeds flows that TIE on UpdatedAt, which is the ordinary case rather than a
// corner one: SaveFlow persists whatever UpdatedAt the caller supplied and never
// stamps one itself, so any flow written without an explicit timestamp carries
// the zero time — and all of them tie with each other.
//
// With a tie, ordering came from two non-deterministic sources at once: the map
// range in ListFlows, and slices.SortFunc, which is NOT a stable sort. Each page
// was therefore ordered independently of the last, so a LIMIT/OFFSET walk
// re-shuffled between pages and silently skipped rows.
func TestListFlows_PaginationVisitsEveryFlow(t *testing.T) {
	const total, pageSize = 40, 10

	for _, tc := range []struct {
		name  string
		stamp func(i int) time.Time
	}{
		{"all tied on the zero time (no UpdatedAt supplied)", func(int) time.Time { return time.Time{} }},
		{"all tied on one timestamp (bulk import)", func(int) time.Time {
			return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		}},
		{"distinct timestamps", func(i int) time.Time {
			return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewLocalStorageBackend(t.TempDir())
			if err != nil {
				t.Fatalf("NewLocalStorageBackend: %v", err)
			}
			ctx := context.Background()

			want := make(map[string]bool, total)
			for i := 0; i < total; i++ {
				id := fmt.Sprintf("flow-%02d", i)
				if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
					ID:        id,
					Name:      id,
					Content:   json.RawMessage(`{}`),
					UpdatedAt: tc.stamp(i),
				}); err != nil {
					t.Fatalf("SaveFlow %s: %v", id, err)
				}
				want[id] = true
			}

			// Exactly the loop in migration.Migrator.migrateFlows and
			// scanner.ScanOnce.
			seen := map[string]int{}
			for offset := 0; ; {
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
					seen[f.ID]++
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
				t.Errorf("paginated enumeration skipped %d of %d flows: %v", len(missed), total, missed)
			}
			if len(duped) > 0 {
				t.Errorf("paginated enumeration returned %d flow(s) more than once: %v", len(duped), duped)
			}
		})
	}
}

// TestListFlows_OrderIsDeterministic is the narrower property underneath:
// identical queries must return identical orderings. Without it, no
// LIMIT/OFFSET walk over this backend can be correct.
func TestListFlows_OrderIsDeterministic(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("flow-%02d", i)
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: id, Name: "same name", Content: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
	}

	order := func() []string {
		page, err := b.ListFlows(ctx, interfaces.FlowFilter{AllFlows: true, MetadataOnly: true})
		if err != nil {
			t.Fatalf("ListFlows: %v", err)
		}
		ids := make([]string, len(page))
		for i, f := range page {
			ids[i] = f.ID
		}
		return ids
	}

	first := order()
	for i := 0; i < 20; i++ {
		got := order()
		if len(got) != len(first) {
			t.Fatalf("call %d returned %d flows, first returned %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("call %d differs from the first at position %d: %s vs %s — ListFlows ordering is not deterministic, so LIMIT/OFFSET pagination cannot be correct", i, j, got[j], first[j])
			}
		}
	}
}
