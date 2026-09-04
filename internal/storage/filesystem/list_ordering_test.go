package filesystem

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestListUsers_OrderIsDeterministic and its pagination sibling cover the same
// defect fixed for flows: the slice is built by ranging a MAP and sorted with a
// NON-STABLE sort on a non-unique key, so any group of users sharing CreatedAt
// came out in a different arbitrary order on every call. The admin user list is
// offset-paginated, so that dropped and repeated users across page boundaries.
//
// Ties are easy to hit — a seeded install or a bulk import creates accounts
// inside one clock tick — and this backend's ListUsers preserved whatever
// CreatedAt the caller supplied.
func TestListUsers_OrderIsDeterministic(t *testing.T) {
	b := seedUsers(t, 40, time.Time{})
	ctx := context.Background()

	order := func() []string {
		got, err := b.ListUsers(ctx, 0, 0)
		if err != nil {
			t.Fatalf("ListUsers: %v", err)
		}
		ids := make([]string, len(got))
		for i, u := range got {
			ids[i] = u.ID
		}
		return ids
	}
	first := order()
	for i := 0; i < 20; i++ {
		got := order()
		for j := range got {
			if j < len(first) && got[j] != first[j] {
				t.Fatalf("call %d differs from the first at position %d (%s vs %s) — ListUsers ordering is not deterministic, so its offset pagination cannot be correct", i, j, got[j], first[j])
			}
		}
	}
}

func TestListUsers_PaginationVisitsEveryUserOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stamp time.Time
	}{
		{"all tied on the zero time", time.Time{}},
		{"all tied on one timestamp (bulk import)", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const total, pageSize = 40, 10
			b := seedUsers(t, total, tc.stamp)
			ctx := context.Background()

			seen := map[string]int{}
			for offset := 0; ; {
				page, err := b.ListUsers(ctx, pageSize, offset)
				if err != nil {
					t.Fatalf("ListUsers(offset=%d): %v", offset, err)
				}
				if len(page) == 0 {
					break
				}
				for _, u := range page {
					seen[u.ID]++
				}
				offset += len(page)
				if len(page) < pageSize {
					break
				}
			}

			var missed, duped []string
			for i := 0; i < total; i++ {
				id := fmt.Sprintf("user-%02d", i)
				switch n := seen[id]; {
				case n == 0:
					missed = append(missed, id)
				case n > 1:
					duped = append(duped, fmt.Sprintf("%s x%d", id, n))
				}
			}
			if len(missed) > 0 {
				t.Errorf("paginated walk skipped %d of %d users: %v", len(missed), total, missed)
			}
			if len(duped) > 0 {
				t.Errorf("paginated walk returned %d user(s) more than once: %v", len(duped), duped)
			}
		})
	}
}

func seedUsers(t *testing.T, n int, stamp time.Time) *LocalStorageBackend {
	t.Helper()
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < n; i++ {
		u := &interfaces.User{
			ID:        fmt.Sprintf("user-%02d", i),
			Email:     fmt.Sprintf("user-%02d@example.com", i),
			Password:  "hash",
			CreatedAt: stamp,
		}
		if err := b.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser %s: %v", u.ID, err)
		}
	}
	return b
}
