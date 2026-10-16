package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// The durable ring is bounded per user too: a snapshot records who made the mutation, and the user's snapshots on
// every flow share one bound, so editing many flows cannot multiply the per-flow one. A push without a user (a
// background job) is bounded per flow only.
func TestSnapshotStore_BoundedPerUserAcrossFlows(t *testing.T) {
	backend := testutil.NewFakeBackend()
	svc := replica(backend)
	if svc.snapshotLimits.UserKeep != maxUserSnapshots || svc.snapshotLimits.UserMaxBytes != maxUserSnapshotBytes {
		t.Fatalf("WithSnapshotStore limits = %+v, want the per-user bound %d / %d", svc.snapshotLimits, maxUserSnapshots, maxUserSnapshotBytes)
	}
	if maxUserSnapshotBytes < maxFlowSnapshotBytes || maxUserSnapshots < maxFlowSnapshots {
		t.Fatal("the per-user bound must hold at least one full flow ring, or a snapshot the flow bound keeps would be refused")
	}

	ctx := auth.WithActor(context.Background(), &auth.Actor{Kind: auth.ActorUser, UserID: "u-editor"})
	a, b := newCloudSnapshotDoc(t, snapshotFlowSrc), newCloudSnapshotDoc(t, snapshotFlowSrc)
	b.ID = "cloud-flow-b"
	for _, doc := range []string{a.ID, b.ID} {
		if err := backend.SaveFlow(ctx, &storageif.FlowDocument{ID: doc, Name: doc, OwnerID: "u1", Content: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	svc.snapshotLimits.UserKeep = 3
	push := func(ctx context.Context, doc string, label string) {
		d := a
		if doc == b.ID {
			d = b
		}
		svc.pushSnapshot(ctx, d, svc.snapshotCloudSourceText(d, snapshotFlowSrc, label))
	}
	push(ctx, a.ID, "a1")
	push(ctx, a.ID, "a2")
	push(context.Background(), a.ID, "job") // no user: not counted
	push(ctx, b.ID, "b1")
	push(ctx, b.ID, "b2")

	labels := func(doc string) (out []string) {
		d := a
		if doc == b.ID {
			d = b
		}
		for _, s := range snapshotsOf(t, svc, d) {
			out = append(out, s.Label)
		}
		return out
	}
	if got := labels(a.ID); len(got) != 2 || got[0] != "a2" || got[1] != "job" {
		t.Errorf("flow A = %v, want [a2 job]: the editor's oldest went, the job's stays", got)
	}
	if got := labels(b.ID); len(got) != 2 {
		t.Errorf("flow B = %v, want both of the editor's newest", got)
	}
}
