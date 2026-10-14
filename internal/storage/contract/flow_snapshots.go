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

// runFlowSnapshotScenarios holds the cloud undo ring (migration 0045): the newest snapshots of a flow in push order,
// listed without their text, loaded by (flow, id), bounded by count and bytes on every push, hidden after
// FlowSnapshotTTL, and gone with the flow. Row-level security is the Postgres suite's (tenancy needs a user scope).
func runFlowSnapshotScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	newFlow := func(t *testing.T, name string) string {
		t.Helper()
		id := "contract-snap-" + name + "-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: name, Content: json.RawMessage(`{}`), OwnerID: "contract-snap-owner-" + runID}); err != nil {
			t.Fatalf("SaveFlow %s: %v", id, err)
		}
		t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), id) })
		return id
	}
	push := func(t *testing.T, flowID, id, source string, lim interfaces.FlowSnapshotLimits) error {
		t.Helper()
		return b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: id, FlowID: flowID, Label: "before " + id, Bytes: int64(len(source)), Source: source}, lim)
	}
	roomy := interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20}
	ids := func(t *testing.T, flowID string) []string {
		t.Helper()
		list, err := b.ListFlowSnapshots(ctx, flowID)
		if err != nil {
			t.Fatalf("ListFlowSnapshots: %v", err)
		}
		out := make([]string, len(list))
		for i, s := range list {
			out[i] = s.ID
		}
		return out
	}
	want := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("snapshots = %v, want %v", got, want)
		}
	}

	run("FlowSnapshots_push_list_load", func(t *testing.T) {
		flowID := newFlow(t, "plain")
		if list, err := b.ListFlowSnapshots(ctx, flowID); err != nil || list == nil || len(list) != 0 {
			t.Fatalf("list of a flow without snapshots = %#v, %v; want an empty non-nil slice", list, err)
		}
		files := map[string]string{"Main.txt": "main text", "Sub.txt": "sub text"}
		if err := push(t, flowID, "a-"+runID, "first source", roomy); err != nil {
			t.Fatal(err)
		}
		if err := b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: "b-" + runID, FlowID: flowID, Label: "folder", Bytes: 16, Files: files}, roomy); err != nil {
			t.Fatal(err)
		}
		list, err := b.ListFlowSnapshots(ctx, flowID)
		if err != nil || len(list) != 2 {
			t.Fatalf("list = %v, %v; want two", list, err)
		}
		if list[0].ID != "a-"+runID || list[1].ID != "b-"+runID {
			t.Errorf("order = %s, %s; want push order, newest last", list[0].ID, list[1].ID)
		}
		if list[0].Label != "before a-"+runID || list[0].Bytes != int64(len("first source")) || list[0].FlowID != flowID || list[0].CreatedAt.IsZero() {
			t.Errorf("metadata = %+v", list[0])
		}
		if list[0].Source != "" || list[1].Files != nil {
			t.Errorf("a list carries text: source %q, files %v", list[0].Source, list[1].Files)
		}

		one, err := b.LoadFlowSnapshot(ctx, flowID, "a-"+runID)
		if err != nil || one.Source != "first source" || one.Files != nil || one.Label != "before a-"+runID {
			t.Fatalf("load single = %+v, %v", one, err)
		}
		folder, err := b.LoadFlowSnapshot(ctx, flowID, "b-"+runID)
		if err != nil || folder.Source != "" || len(folder.Files) != 2 || folder.Files["Sub.txt"] != "sub text" {
			t.Fatalf("load folder = %+v, %v", folder, err)
		}
	})

	run("FlowSnapshots_prune_to_keep", func(t *testing.T) {
		flowID := newFlow(t, "keep")
		lim := interfaces.FlowSnapshotLimits{Keep: 3, MaxBytes: 1 << 20}
		for i := 1; i <= 5; i++ {
			if err := push(t, flowID, fmt.Sprintf("s%d-%s", i, runID), "x", lim); err != nil {
				t.Fatal(err)
			}
		}
		want(t, ids(t, flowID), "s3-"+runID, "s4-"+runID, "s5-"+runID)
		if _, err := b.LoadFlowSnapshot(ctx, flowID, "s2-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("a pruned snapshot loads: %v, want ErrNotFound", err)
		}
		// A smaller Keep trims at the next push, never beyond it.
		if err := push(t, flowID, "s6-"+runID, "x", interfaces.FlowSnapshotLimits{Keep: 1, MaxBytes: 1 << 20}); err != nil {
			t.Fatal(err)
		}
		want(t, ids(t, flowID), "s6-"+runID)
	})

	run("FlowSnapshots_byte_bound_drops_oldest", func(t *testing.T) {
		flowID := newFlow(t, "bytes")
		lim := interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 100}
		for i, size := range []int{40, 40, 15} {
			if err := push(t, flowID, fmt.Sprintf("s%d-%s", i+1, runID), strings.Repeat("x", size), lim); err != nil {
				t.Fatal(err)
			}
		}
		want(t, ids(t, flowID), "s1-"+runID, "s2-"+runID, "s3-"+runID) // 95 bytes: within the bound
		if err := push(t, flowID, "s4-"+runID, strings.Repeat("x", 30), lim); err != nil {
			t.Fatal(err)
		}
		// 125 > 100: the oldest goes, then 85 holds.
		want(t, ids(t, flowID), "s2-"+runID, "s3-"+runID, "s4-"+runID)

		// A member's name counts toward the stored size of a folder snapshot.
		err := b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: "f-" + runID, FlowID: flowID, Files: map[string]string{strings.Repeat("n", 60): strings.Repeat("t", 41)}}, lim)
		if !errors.Is(err, interfaces.ErrFlowSnapshotTooLarge) {
			t.Fatalf("push of 101 stored bytes = %v, want ErrFlowSnapshotTooLarge", err)
		}
		// One that exceeds the whole bound is refused and evicts nothing.
		if err := push(t, flowID, "big-"+runID, strings.Repeat("x", 101), lim); !errors.Is(err, interfaces.ErrFlowSnapshotTooLarge) {
			t.Fatalf("push of 101 bytes = %v, want ErrFlowSnapshotTooLarge", err)
		}
		want(t, ids(t, flowID), "s2-"+runID, "s3-"+runID, "s4-"+runID)
	})

	run("FlowSnapshots_load_is_scoped_to_the_flow", func(t *testing.T) {
		a, other := newFlow(t, "scope-a"), newFlow(t, "scope-b")
		if err := push(t, a, "mine-"+runID, "a source", roomy); err != nil {
			t.Fatal(err)
		}
		if err := push(t, other, "theirs-"+runID, "other source", roomy); err != nil {
			t.Fatal(err)
		}
		if _, err := b.LoadFlowSnapshot(ctx, a, "theirs-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("another flow's snapshot id loads through this flow: %v, want ErrNotFound", err)
		}
		want(t, ids(t, a), "mine-"+runID)
		want(t, ids(t, other), "theirs-"+runID)
		if _, err := b.LoadFlowSnapshot(ctx, a, "never-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("an unknown id = %v, want ErrNotFound", err)
		}
	})

	run("FlowSnapshots_gone_with_the_flow", func(t *testing.T) {
		flowID := newFlow(t, "cascade")
		if err := push(t, flowID, "c1-"+runID, "x", roomy); err != nil {
			t.Fatal(err)
		}
		if err := b.DeleteFlow(ctx, flowID); err != nil {
			t.Fatalf("DeleteFlow: %v", err)
		}
		want(t, ids(t, flowID))
		if _, err := b.LoadFlowSnapshot(ctx, flowID, "c1-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("a deleted flow's snapshot loads: %v, want ErrNotFound", err)
		}
		// A flow created again under the id starts with an empty ring.
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: flowID, Name: "again", Content: json.RawMessage(`{}`), OwnerID: "contract-snap-owner-" + runID}); err != nil {
			t.Fatal(err)
		}
		want(t, ids(t, flowID))
	})

	run("FlowSnapshots_push_for_a_missing_flow", func(t *testing.T) {
		err := push(t, "contract-snap-missing-"+runID, "m-"+runID, "x", roomy)
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Fatalf("push on a flow that does not exist = %v, want ErrNotFound", err)
		}
	})

	run("FlowSnapshots_nul_byte_is_refused", func(t *testing.T) {
		flowID := newFlow(t, "nul")
		if err := push(t, flowID, "n1-"+runID, "a\x00b", roomy); !errors.Is(err, interfaces.ErrFlowSnapshotUnstorable) {
			t.Fatalf("push of a NUL = %v, want ErrFlowSnapshotUnstorable", err)
		}
		err := b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: "n2-" + runID, FlowID: flowID, Files: map[string]string{"A.txt": "x\x00"}}, roomy)
		if !errors.Is(err, interfaces.ErrFlowSnapshotUnstorable) {
			t.Fatalf("push of a NUL in a member = %v, want ErrFlowSnapshotUnstorable", err)
		}
		want(t, ids(t, flowID))
	})

	run("FlowSnapshots_expired_are_hidden", func(t *testing.T) {
		flowID := newFlow(t, "expiry")
		old := time.Now().Add(-interfaces.FlowSnapshotTTL - time.Hour)
		if err := b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: "old-" + runID, FlowID: flowID, CreatedAt: old, Source: "x"}, roomy); err != nil {
			t.Fatal(err)
		}
		if err := push(t, flowID, "new-"+runID, "y", roomy); err != nil {
			t.Fatal(err)
		}
		want(t, ids(t, flowID), "new-"+runID)
		if _, err := b.LoadFlowSnapshot(ctx, flowID, "old-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("an expired snapshot loads: %v, want ErrNotFound", err)
		}
	})
}
