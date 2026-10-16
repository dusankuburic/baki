package testutil

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// The retention sweep and the account erasure treat the fake's undo rings as Postgres treats flow_snapshots: the
// sweep deletes what is past FlowSnapshotTTL and counts it, the erasure takes the owner's flows with their rings.
// A zero-value FakeBackend works, as the many tests that build one by literal need.
func TestFakeBackend_FlowSnapshotsPurgeAndErasure(t *testing.T) {
	ctx := context.Background()
	m := &FakeBackend{}
	lim := interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20}
	for _, flow := range []string{"f-kept", "f-owned"} {
		owner := "u-other"
		if flow == "f-owned" {
			owner = "u-erased"
		}
		if err := m.SaveFlow(ctx, &interfaces.FlowDocument{ID: flow, Name: flow, OwnerID: owner}); err != nil {
			t.Fatal(err)
		}
		for id, age := range map[string]time.Duration{flow + "-live": 0, flow + "-old": interfaces.FlowSnapshotTTL + time.Minute} {
			if err := m.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: id, FlowID: flow, Source: "x", CreatedAt: time.Now().Add(-age)}, lim); err != nil {
				t.Fatal(err)
			}
		}
	}

	res, err := m.PurgeExpiredData(ctx, 0)
	if err != nil || res.FlowSnapshots != 2 {
		t.Fatalf("PurgeExpiredData = %+v, %v; want the 2 expired snapshots", res, err)
	}
	if got, _ := m.ListFlowSnapshots(ctx, "f-kept"); len(got) != 1 || got[0].ID != "f-kept-live" {
		t.Errorf("the live snapshot did not survive the sweep: %+v", got)
	}

	// The erased user's snapshot on someone else's flow stays, without the user's id.
	if err := m.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: "f-kept-by-erased", FlowID: "f-kept", UserID: "u-erased", Source: "x"}, lim); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteUser(ctx, "u-erased"); err != nil {
		t.Fatal(err)
	}
	if ring := m.flowSnapshots.rings["f-kept"]; len(ring) != 2 || ring[1].ID != "f-kept-by-erased" || ring[1].UserID != "" {
		t.Errorf("after the erasure the other owner's ring is %+v; want the erased user's snapshot kept without its user", ring)
	}
	if got, _ := m.ListFlowSnapshots(ctx, "f-owned"); len(got) != 0 {
		t.Errorf("the erased owner's flow kept its ring: %+v", got)
	}
	if _, err := m.LoadFlowSnapshot(ctx, "f-owned", "f-owned-live"); err == nil {
		t.Error("the erased owner's snapshot still loads")
	}
}
