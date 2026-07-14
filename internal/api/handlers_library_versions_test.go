package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// makeFlowContent builds a serialized FlowDocument (the shape stored in
// FlowDocument.Content) with one "Main" subflow whose top-level blocks are
// named by names. Used to seed versioned content the diff/restore handlers
// unmarshal.
func makeFlowContent(t *testing.T, names ...string) json.RawMessage {
	t.Helper()
	blocks := make([]models.Block, len(names))
	for i, n := range names {
		blocks[i] = models.Block{ID: n, Name: n, Type: models.BlockTypeAction, RawType: n}
	}
	doc := models.FlowDocument{Subflows: []models.Subflow{{Name: "Main", Blocks: blocks}}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal flow content: %v", err)
	}
	return b
}

// seedVersionedFlow creates a filesystem-backed JWT router, seeds a flow owned
// by ownerID with the given content, and returns the router + backend so the
// test can mutate content directly between snapshots.
func seedVersionedFlow(t *testing.T, id, ownerID string, content json.RawMessage) (*Router, *filesystem.LocalStorageBackend) {
	t.Helper()
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("create local storage: %v", err)
	}
	rt := newTestRouter(fs, true)
	doc := &storageif.FlowDocument{ID: id, Name: "test", OwnerID: ownerID, Content: content}
	if err := fs.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
	return rt, fs
}

// setFlowContent updates a seeded flow's content in place (respecting OCC by
// loading the current version first).
func setFlowContent(t *testing.T, fs *filesystem.LocalStorageBackend, id string, content json.RawMessage) {
	t.Helper()
	doc, err := fs.LoadFlow(context.Background(), id)
	if err != nil {
		t.Fatalf("load flow %s: %v", id, err)
	}
	doc.Content = content
	if err := fs.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("update flow content: %v", err)
	}
}

// TestDiffFlowVersion_ReturnsChanges verifies GET /versions/{vn}/diff reports
// blocks added since the snapshot.
func TestDiffFlowVersion_ReturnsChanges(t *testing.T) {
	rt, fs := seedVersionedFlow(t, "flow1", "alice", makeFlowContent(t, "A"))
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// Snapshot v1 = content with block "A".
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/library/flow1/versions", bearer, map[string]any{"comment": "v1"}), http.StatusCreated)

	// Current now has "A" + "B".
	setFlowContent(t, fs, "flow1", makeFlowContent(t, "A", "B"))

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow1/versions/1/diff", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var diff models.FlowDiff
	decodeJSON(t, rr, &diff)
	var added int
	for _, sf := range diff.Subflows {
		for _, b := range sf.Blocks {
			if b.Change == models.ChangeAdded {
				added++
			}
		}
	}
	if added != 1 {
		t.Errorf("expected 1 added block since v1, got %d", added)
	}
}

// TestRestoreFlowVersion_RevertsContent verifies POST /versions/{vn}/restore
// reverts the current content to the snapshot and is reflected on /content.
func TestRestoreFlowVersion_RevertsContent(t *testing.T) {
	rt, fs := seedVersionedFlow(t, "flow1", "alice", makeFlowContent(t, "A"))
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// Snapshot v1 = "A".
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/library/flow1/versions", bearer, map[string]any{"comment": "v1"}), http.StatusCreated)
	// Current now = "A" + "B" + "C".
	setFlowContent(t, fs, "flow1", makeFlowContent(t, "A", "B", "C"))

	// Restore v1 → content reverts to "A".
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/library/flow1/versions/1/restore", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	// /content must now hold only block "A" (the snapshot), not B/C.
	contentRR := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow1/content", bearer, nil)
	checkStatus(t, contentRR, http.StatusOK)
	var restored models.FlowDocument
	decodeJSON(t, contentRR, &restored)
	if len(restored.Subflows) == 0 || len(restored.Subflows[0].Blocks) != 1 {
		t.Fatalf("expected 1 block after restore, got %d subflows", len(restored.Subflows))
	}
	gotName := restored.Subflows[0].Blocks[0].Name
	if gotName != "A" {
		t.Errorf("expected restored block 'A', got %q", gotName)
	}
}

// TestRestoreFlowVersion_ViewerForbidden verifies restore requires write
// access (a viewer collaborator is rejected).
func TestRestoreFlowVersion_ViewerForbidden(t *testing.T) {
	rt, _ := seedVersionedFlow(t, "flow1", "alice", makeFlowContent(t, "A"))
	seedCollaborator(t, rt, "flow1", "erin", "viewer")
	bearer := jwtBearer(t, rt, "erin", "erin@example.com")

	// Snapshot first (as owner) so v1 exists.
	ownerBearer := jwtBearer(t, rt, "alice", "alice@example.com")
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/library/flow1/versions", ownerBearer, map[string]any{"comment": "v1"}), http.StatusCreated)

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/library/flow1/versions/1/restore", bearer, nil)
	checkStatus(t, rr, http.StatusForbidden)
}
