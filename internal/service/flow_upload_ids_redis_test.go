package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The flow id an upload minted is remembered per caller and content so that an identical re-upload updates that
// flow. In memory that holds for one replica; with Redis configured the id is claimed there, so a re-upload that
// lands on another replica finds it, and two replicas uploading the same bytes at once create one flow.

func uploadReplicas(t *testing.T, n int, withRedis bool) (*redisreplica.Cluster, []*FlowService, *testutil.FakeBackend) {
	t.Helper()
	backend := testutil.NewFakeBackend()
	cluster := redisreplica.New(t, n)
	svcs := make([]*FlowService, n)
	for i := range svcs {
		var opts []FlowOption
		if withRedis {
			opts = append(opts, WithUploadIDs(cluster.Replicas[i].Client))
		}
		svcs[i] = NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, nil, nil, opts...)
	}
	return cluster, svcs, backend
}

func upload(t *testing.T, svc *FlowService, caller string, save bool) string {
	t.Helper()
	ctx := context.Background()
	doc, err := svc.LoadFlowFiles(ctx, map[string]string{"Main.txt": uploadFlowText}, "Payroll", caller)
	if err != nil {
		t.Fatalf("upload by %s: %v", caller, err)
	}
	if save {
		doc.OwnerID = caller
		if err := svc.SaveUploadedFlow(ctx, doc, caller); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	return doc.ID
}

func TestUploadIDs_AnIdenticalReuploadOnAnotherReplicaUpdatesTheSameFlow(t *testing.T) {
	_, svcs, _ := uploadReplicas(t, 2, true)
	first := upload(t, svcs[0], "alice", true)
	if second := upload(t, svcs[1], "alice", true); second != first {
		t.Errorf("a re-upload through the other replica minted %s, want the first upload's flow %s", second, first)
	}
	if other := upload(t, svcs[1], "bob", true); other == first {
		t.Error("another user's identical upload was given alice's flow id")
	}
}

// Without Redis it is what it was: each replica remembers its own, and an identical upload through another one
// creates a second flow (the old reason of flow.upload-ids, kept for a deployment on one replica).
func TestUploadIDs_WithoutRedisEachReplicaRemembersItsOwn(t *testing.T) {
	_, svcs, _ := uploadReplicas(t, 2, false)
	first := upload(t, svcs[0], "alice", true)
	if again := upload(t, svcs[0], "alice", true); again != first {
		t.Errorf("a re-upload on the same replica minted %s, want %s", again, first)
	}
	if other := upload(t, svcs[1], "alice", true); other == first {
		t.Error("without Redis the other replica knew the id: the test premise is wrong")
	}
}

// Two replicas parse the same bytes at once, before either saved: the first to claim the id wins, the other adopts it.
func TestUploadIDs_ConcurrentFirstUploadsClaimOneID(t *testing.T) {
	_, svcs, _ := uploadReplicas(t, 2, true)
	a := upload(t, svcs[0], "alice", false)
	b := upload(t, svcs[1], "alice", false)
	if a != b {
		t.Errorf("two replicas minted %s and %s for one upload, want one flow id", a, b)
	}
}

// An id whose flow was deleted is replaced, for every replica.
func TestUploadIDs_ADeletedFlowsIDIsReplaced(t *testing.T) {
	prev := uploadClaimGrace
	uploadClaimGrace = 0 // the claim is old enough that a missing flow is a deleted one
	t.Cleanup(func() { uploadClaimGrace = prev })
	_, svcs, backend := uploadReplicas(t, 2, true)
	first := upload(t, svcs[0], "alice", true)
	if err := backend.DeleteFlow(context.Background(), first); err != nil {
		t.Fatalf("delete: %v", err)
	}
	second := upload(t, svcs[1], "alice", true)
	if second == first {
		t.Fatal("a deleted flow's id was reused")
	}
	if third := upload(t, svcs[0], "alice", true); third != second {
		t.Errorf("the replica that did not mint the replacement got %s, want %s", third, second)
	}
}

// The key lives a bounded time (the server evicts only keys with a TTL).
func TestUploadIDs_TheKeyHasATTL(t *testing.T) {
	c, svcs, _ := uploadReplicas(t, 1, true)
	upload(t, svcs[0], "alice", true)
	keys := c.Redis.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want exactly the upload's", keys)
	}
	if ttl := c.Redis.TTL(keys[0]); ttl <= 0 || ttl > astCacheTTL {
		t.Errorf("ttl = %v, want it set and no longer than %v", ttl, astCacheTTL)
	}
}

// A miss is harmless: with Redis down the upload still works (the replica's own memory answers) and does not wait.
func TestUploadIDs_RedisDownFallsBackToMemory(t *testing.T) {
	c, svcs, _ := uploadReplicas(t, 2, true)
	first := upload(t, svcs[0], "alice", true)
	c.Kill()
	start := time.Now()
	again := upload(t, svcs[0], "alice", true)
	if again != first {
		t.Errorf("same replica, Redis down: minted %s, want its own memory's %s", again, first)
	}
	other := upload(t, svcs[1], "alice", true)
	if other == first {
		t.Error("the other replica found the id with Redis down")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("uploads took %v with Redis down", d)
	}
}
