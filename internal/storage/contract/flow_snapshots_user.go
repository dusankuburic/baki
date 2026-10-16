package contract

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runFlowSnapshotUserBoundScenarios holds the per-user bound of the undo ring (migration 0047): a push also trims the
// pushing user's snapshots across every flow, newest first in push order, by count and by stored bytes, so editing
// many flows cannot multiply the per-flow bound. Another user's snapshots and those without a user are not counted.
func runFlowSnapshotUserBoundScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	newFlow := func(t *testing.T, name string) string {
		t.Helper()
		id := "contract-usnap-" + name + "-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: name, Content: json.RawMessage(`{}`), OwnerID: "contract-usnap-owner-" + runID}); err != nil {
			t.Fatalf("SaveFlow %s: %v", id, err)
		}
		t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), id) })
		return id
	}
	push := func(t *testing.T, flowID, user, id string, size int, lim interfaces.FlowSnapshotLimits) error {
		t.Helper()
		return b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: id + "-" + runID, FlowID: flowID, UserID: user, Source: strings.Repeat("x", size)}, lim)
	}
	mustPush := func(t *testing.T, flowID, user, id string, size int, lim interfaces.FlowSnapshotLimits) {
		t.Helper()
		if err := push(t, flowID, user, id, size, lim); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	ids := func(t *testing.T, flowID string) string {
		t.Helper()
		list, err := b.ListFlowSnapshots(ctx, flowID)
		if err != nil {
			t.Fatalf("ListFlowSnapshots: %v", err)
		}
		out := make([]string, len(list))
		for i, s := range list {
			out[i] = strings.TrimSuffix(s.ID, "-"+runID)
		}
		return strings.Join(out, ",")
	}
	want := func(t *testing.T, flowID, w string) {
		t.Helper()
		if got := ids(t, flowID); got != w {
			t.Errorf("snapshots of %s = %q, want %q", flowID, got, w)
		}
	}

	run("FlowSnapshots_user_bound_by_count", func(t *testing.T) {
		a, bf, c := newFlow(t, "count-a"), newFlow(t, "count-b"), newFlow(t, "count-c")
		me, other := "contract-usnap-me-"+runID, "contract-usnap-other-"+runID
		lim := interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20, UserKeep: 4, UserMaxBytes: 1 << 20}
		mustPush(t, a, me, "a1", 1, lim)
		mustPush(t, a, other, "a-other", 1, lim)
		mustPush(t, a, "", "a-nobody", 1, lim)
		mustPush(t, a, me, "a2", 1, lim)
		mustPush(t, bf, me, "b1", 1, lim)
		mustPush(t, bf, me, "b2", 1, lim)
		want(t, a, "a1,a-other,a-nobody,a2") // four of mine: within the bound
		mustPush(t, c, me, "c1", 1, lim)
		// The fifth of mine drops my oldest, on another flow; the other user's and the unattributed one stay.
		want(t, a, "a-other,a-nobody,a2")
		want(t, bf, "b1,b2")
		want(t, c, "c1")
		mustPush(t, c, me, "c2", 1, lim)
		want(t, a, "a-other,a-nobody")
		want(t, c, "c1,c2")
		if _, err := b.LoadFlowSnapshot(ctx, a, "a2-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("a snapshot trimmed by the user bound loads: %v, want ErrNotFound", err)
		}
		// Without a user bound only the per-flow bound applies.
		mustPush(t, c, me, "c3", 1, interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20})
		want(t, bf, "b1,b2")
		want(t, c, "c1,c2,c3")
	})

	run("FlowSnapshots_user_bound_by_bytes", func(t *testing.T) {
		a, bf := newFlow(t, "bytes-a"), newFlow(t, "bytes-b")
		me := "contract-usnap-bytes-" + runID
		lim := interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20, UserKeep: 100, UserMaxBytes: 100}
		mustPush(t, a, me, "a1", 40, lim)
		mustPush(t, bf, me, "b1", 40, lim)
		mustPush(t, a, me, "a2", 15, lim)
		want(t, a, "a1,a2") // 95 bytes: within the bound
		mustPush(t, bf, me, "b2", 30, lim)
		// 125 > 100: my oldest goes, then 85 holds.
		want(t, a, "a2")
		want(t, bf, "b1,b2")

		// One that alone exceeds the user's bytes is refused and evicts nothing.
		if err := push(t, a, me, "big", 101, lim); !errors.Is(err, interfaces.ErrFlowSnapshotTooLarge) {
			t.Fatalf("push of 101 bytes over a 100 byte user bound = %v, want ErrFlowSnapshotTooLarge", err)
		}
		want(t, a, "a2")
		want(t, bf, "b1,b2")
	})
}
