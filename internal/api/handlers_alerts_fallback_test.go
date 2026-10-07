package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestAddComment_AlertsSurviveSaturatedPool: the in-app bell rows never could
// be dropped when the fan-out ran inline; detached on h.alerts they must not
// be either. When the pool refuses (saturated, or draining at shutdown) the
// post-commit hook pays the round trips itself instead of losing the rows.
func TestAddComment_AlertsSurviveSaturatedPool(t *testing.T) {
	backend := newGatedAlertBackend()
	close(backend.release) // alert writes return at once
	backend.Flows["flow-c"] = &storageif.FlowDocument{ID: "flow-c", Name: "Invoices", OwnerID: "alice", Content: []byte(`{}`)}
	if err := backend.SetFindingStatus(context.Background(), &storageif.FindingStatus{
		FlowID: "flow-c", FindingKey: "unused-variable:b1", RuleID: "unused-variable", Status: "open", AssigneeID: "u-assignee", UpdatedBy: "alice",
	}); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	backend.prior = []*storageif.FindingComment{
		{ID: "c-1", FlowID: "flow-c", FindingKey: "unused-variable:b1", AuthorID: "u-prev1", Body: "first"},
	}
	rt := newTestRouter(backend, true)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// Drain the alert pool: from here on every Go returns false.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := moduleOf[*AnalysisHandler](t, rt).alerts.Drain(ctx); err != nil {
		t.Fatalf("drain alerts: %v", err)
	}

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add",
		bearer, map[string]any{"flowId": "flow-c", "findingKey": "unused-variable:b1", "body": "still here"}), http.StatusOK)

	// No RLS transaction on this backend, so the hook ran inline before the
	// response: both rows (assignee + prior commenter) are already there.
	if n := backend.alerts.Load(); n != 2 {
		t.Fatalf("alerts recorded with the pool refusing = %d, want 2 (hook must run the fan-out itself)", n)
	}
}
