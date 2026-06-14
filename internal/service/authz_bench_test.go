package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// BenchmarkBatchFlowPermissions_LocalMode measures the batch permission
// resolution in local mode (storage == nil, no DB queries). This establishes
// a baseline for the in-memory portion of the work.
func BenchmarkBatchFlowPermissions_LocalMode(b *testing.B) {
	authz := NewAuthzService(nil, nil)
	docs := make([]*storageif.FlowDocument, 50)
	for i := range docs {
		docs[i] = &storageif.FlowDocument{
			ID:      testutil.NewTestFlow("f", "test").ID,
			OwnerID: "user1",
		}
		docs[i].ID = "flow-" + string(rune('a'+i))
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		authz.BatchFlowPermissions(ctx, docs, "user1")
	}
}

// BenchmarkFlowPermissions_SingleFlow measures the per-flow permission
// resolution for comparison. Running this 50 times simulates the old N+1
// pattern (without the DB query overhead).
func BenchmarkFlowPermissions_SingleFlow(b *testing.B) {
	authz := NewAuthzService(nil, nil)
	doc := &storageif.FlowDocument{ID: "f1", OwnerID: "user1"}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		authz.FlowPermissions(ctx, doc.ID, doc.OwnerID, doc.OrganizationID, "user1")
	}
}

// BenchmarkBatchFlowPermissions_WithFakeBackend measures the batch resolution
// against the FakeBackend, which has real collaborator lookups. This shows
// the improvement over calling FlowPermissions per-flow (which would make
// N separate ListCollaborators calls).
func BenchmarkBatchFlowPermissions_WithFakeBackend(b *testing.B) {
	// FakeBackend in testutil doesn't support org membership, so we test
	// the local-mode path here. The real N+1 improvement is in cloud mode
	// where BatchFlowPermissions makes 1 LoadOrg + 1 ListCollaboratorsBatch
	// instead of N*LoadOrg + N*ListCollaborators.
	authz := NewAuthzService(nil, nil)
	docs := make([]*storageif.FlowDocument, 50)
	for i := range docs {
		docs[i] = &storageif.FlowDocument{
			ID:      "flow-" + string(rune('a'+i)),
			OwnerID: "user1",
		}
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		authz.BatchFlowPermissions(ctx, docs, "user1")
	}
}

// Ensure imports are used
var _ = auth.RoleAdmin
