package service

import (
	"testing"

	"pad-analyzer/internal/testutil"
)

// TestNewFlowService_SnapshotBudgetByMode: the undo ring's byte budget is
// the desktop's 256 MiB only on the desktop; a cloud FlowService (storage
// wired, durable flow_versions) is built with the 64 MiB budget.
func TestNewFlowService_SnapshotBudgetByMode(t *testing.T) {
	if maxSnapshotBytesCloud != 64<<20 || maxSnapshotBytes != 256<<20 {
		t.Fatalf("budget constants = %d cloud / %d desktop, want 64 MiB / 256 MiB", maxSnapshotBytesCloud, maxSnapshotBytes)
	}
	backend := testutil.NewFakeBackend()
	cloud := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, nil, nil)
	if got := cloud.snapshots.maxBytes; got != maxSnapshotBytesCloud {
		t.Errorf("cloud snapshot budget = %d, want %d (64 MiB)", got, maxSnapshotBytesCloud)
	}
	local := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), nil, nil, nil)
	if got := local.snapshots.maxBytes; got != maxSnapshotBytes {
		t.Errorf("desktop snapshot budget = %d, want %d (256 MiB)", got, maxSnapshotBytes)
	}
	if st := newSnapshotStore(); st.maxBytes != maxSnapshotBytes {
		t.Errorf("newSnapshotStore() budget = %d, want the desktop default %d", st.maxBytes, maxSnapshotBytes)
	}
}
