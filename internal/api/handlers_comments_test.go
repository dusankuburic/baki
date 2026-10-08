package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

func TestComments_AddListDelete(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// Add two comments
	add1 := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "rule-x:block1", "body": "First comment",
	})
	checkStatus(t, add1, http.StatusOK)
	var c1 storageif.FindingComment
	decodeJSON(t, add1, &c1)
	if c1.Body != "First comment" || c1.AuthorID != "alice" {
		t.Errorf("unexpected comment: %+v", c1)
	}
	// The thread is ordered by creation time; on a coarse clock (Windows) two quick requests can share one.
	for !time.Now().After(c1.CreatedAt) {
		time.Sleep(time.Millisecond)
	}

	add2 := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "rule-x:block1", "body": "Second comment",
	})
	checkStatus(t, add2, http.StatusOK)

	// List — both should appear in order
	list := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/list", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "rule-x:block1",
	})
	checkStatus(t, list, http.StatusOK)
	var comments []*storageif.FindingComment
	decodeJSON(t, list, &comments)
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
	if comments[0].Body != "First comment" || comments[1].Body != "Second comment" {
		t.Errorf("comments out of order: %+v", comments)
	}

	// Delete the first
	del := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/delete", bearer, map[string]any{
		"flowId": "flow1", "commentId": c1.ID,
	})
	checkStatus(t, del, http.StatusOK)

	// List — only one remains
	list2 := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/list", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "rule-x:block1",
	})
	checkStatus(t, list2, http.StatusOK)
	var comments2 []*storageif.FindingComment
	decodeJSON(t, list2, &comments2)
	if len(comments2) != 1 {
		t.Fatalf("expected 1 comment after delete, got %d", len(comments2))
	}
	if comments2[0].Body != "Second comment" {
		t.Errorf("wrong comment survived: %+v", comments2[0])
	}
}

// TestComments_DeleteAuthorScoped: an editor-tier collaborator may delete only
// their own comments; the flow owner (admin rank) may moderate any comment.
func TestComments_DeleteAuthorScoped(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	aliceBearer := jwtBearer(t, rt, "alice", "alice@example.com")
	bobBearer := jwtBearer(t, rt, "bob", "bob@example.com")

	// Grant bob editor access to alice's flow.
	ensureUser(t, rt, "bob")
	if err := rt.security.Backend.AddCollaborator(context.Background(), "flow1",
		&storageif.Collaborator{UserID: "bob", Email: "bob@example.com", Permission: "editor"}); err != nil {
		t.Fatalf("add collaborator: %v", err)
	}

	addComment := func(bearer, body string) storageif.FindingComment {
		resp := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add", bearer, map[string]any{
			"flowId": "flow1", "findingKey": "rule-x:block1", "body": body,
		})
		checkStatus(t, resp, http.StatusOK)
		var c storageif.FindingComment
		decodeJSON(t, resp, &c)
		return c
	}
	aliceComment := addComment(aliceBearer, "by alice")
	bobComment := addComment(bobBearer, "by bob")

	// Bob (editor, not admin) cannot delete alice's comment.
	del := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/delete", bobBearer, map[string]any{
		"flowId": "flow1", "commentId": aliceComment.ID,
	})
	checkStatus(t, del, http.StatusForbidden)

	// Bob can delete his own comment.
	del = doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/delete", bobBearer, map[string]any{
		"flowId": "flow1", "commentId": bobComment.ID,
	})
	checkStatus(t, del, http.StatusOK)

	// Alice (owner → admin rank) can delete any remaining comment.
	del = doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/delete", aliceBearer, map[string]any{
		"flowId": "flow1", "commentId": aliceComment.ID,
	})
	checkStatus(t, del, http.StatusOK)

	list := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/list", aliceBearer, map[string]any{
		"flowId": "flow1", "findingKey": "rule-x:block1",
	})
	checkStatus(t, list, http.StatusOK)
	var remaining []*storageif.FindingComment
	decodeJSON(t, list, &remaining)
	if len(remaining) != 0 {
		t.Errorf("expected no comments left, got %d", len(remaining))
	}
}

func TestComments_EmptyBody(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	resp := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "k", "body": "",
	})
	checkStatus(t, resp, http.StatusBadRequest)
}

func TestComments_UnauthorizedUser(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	// bob has no access to alice's flow
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	resp := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "k", "body": "hi",
	})
	if resp.Code != http.StatusForbidden && resp.Code != http.StatusNotFound {
		t.Errorf("expected 403/404 for unauthorized user, got %d", resp.Code)
	}
}

func TestShareTokens_CreateListRevoke(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// Create a share token
	create := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/share/create", bearer, map[string]any{
		"flowId": "flow1",
	})
	checkStatus(t, create, http.StatusOK)
	var result struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decodeJSON(t, create, &result)
	if result.Token == "" {
		t.Fatal("expected non-empty token")
	}

	// List tokens — should show 1 (without the raw token)
	list := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/share/list", bearer, map[string]any{
		"flowId": "flow1",
	})
	checkStatus(t, list, http.StatusOK)

	// The viewer endpoint should work with the raw token
	view := doRequest(t, rt, http.MethodGet, "/api/shared?token="+result.Token, nil)
	checkStatus(t, view, http.StatusOK)

	// Revoke
	revoke := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/share/revoke", bearer, map[string]any{
		"flowId": "flow1", "tokenId": result.ID,
	})
	checkStatus(t, revoke, http.StatusOK)

	// Viewer should now fail (404)
	view2 := doRequest(t, rt, http.MethodGet, "/api/shared?token="+result.Token, nil)
	if view2.Code != http.StatusNotFound {
		t.Errorf("expected 404 after revoke, got %d", view2.Code)
	}
}

func TestShareTokens_InvalidToken(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)

	view := doRequest(t, rt, http.MethodGet, "/api/shared?token=nonexistent", nil)
	if view.Code != http.StatusNotFound {
		t.Errorf("expected 404 for invalid token, got %d", view.Code)
	}
}

func TestShareTokens_MissingToken(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)

	view := doRequest(t, rt, http.MethodGet, "/api/shared", nil)
	if view.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing token, got %d", view.Code)
	}
}

func TestExportSARIF(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	resp := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/export/sarif", bearer, map[string]any{
		"flowId": "flow1",
	})
	checkStatus(t, resp, http.StatusOK)
	// SARIF response should be valid JSON with a "runs" array
	var sarif map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sarif); err != nil {
		t.Fatalf("failed to decode SARIF response: %v", err)
	}
	if sarif["version"] == nil {
		t.Error("expected SARIF response to have a version field")
	}
}

func TestWebhookNotifier_DisabledByDefault(t *testing.T) {
	wn := service.NewWebhookNotifier(nil)
	if wn.Enabled() {
		t.Error("expected webhook notifier to be disabled when PAD_WEBHOOK_URL is unset")
	}
	// NotifyAnalysis should be a safe no-op
	wn.NotifyAnalysis("test-flow", nil)
}

// commentFanoutBackend wraps the fake backend in ATOMIC counters for the fan-out's three storage
// touches: the fan-out runs on detached pools, so plain counters would race the email leg, and
// alertWrites counts completions so a test can wait out the store's writes.
type commentFanoutBackend struct {
	*testutil.FakeBackend
	probes      atomic.Int64 // GetFindingStatus calls, fan-out included
	lists       atomic.Int64 // ListFindingStatuses calls, fan-out included
	alerts      atomic.Int64 // RecordGovernanceAlert calls
	alertWrites atomic.Int64 // RecordGovernanceAlert completions
}

func (b *commentFanoutBackend) GetFindingStatus(ctx context.Context, flowID, findingKey string) (*storageif.FindingStatus, error) {
	b.probes.Add(1)
	return b.FakeBackend.GetFindingStatus(ctx, flowID, findingKey)
}

func (b *commentFanoutBackend) ListFindingStatuses(ctx context.Context, flowID string) ([]*storageif.FindingStatus, error) {
	b.lists.Add(1)
	return b.FakeBackend.ListFindingStatuses(ctx, flowID)
}

func (b *commentFanoutBackend) RecordGovernanceAlert(ctx context.Context, a *storageif.GovernanceAlert) error {
	b.alerts.Add(1)
	err := b.FakeBackend.RecordGovernanceAlert(ctx, a)
	b.alertWrites.Add(1)
	return err
}

// newCommentFanoutRouter builds a JWT test router over the counting backend.
func newCommentFanoutRouter(t *testing.T) (*Router, *commentFanoutBackend) {
	t.Helper()
	store := testutil.NewFakeBackend()
	fb := &commentFanoutBackend{FakeBackend: store}
	return newTestRouter(fb, true), fb
}

// waitForFanoutQuiescence returns once no fan-out counter has moved for 150 ms: the fan-outs run on
// detached pools, so the test must not read the counters while a counted call (or the store write inside
// it) can still land; after the last counted call the remaining work only reads.
func waitForFanoutQuiescence(t *testing.T, fb *commentFanoutBackend) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		before := fb.probes.Load() + fb.lists.Load() + fb.alertWrites.Load()
		time.Sleep(150 * time.Millisecond)
		if fb.probes.Load()+fb.lists.Load()+fb.alertWrites.Load() == before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("comment fan-out never quiesced (probes=%d lists=%d alertWrites=%d)",
				fb.probes.Load(), fb.lists.Load(), fb.alertWrites.Load())
		}
	}
}

// TestComments_AssigneeLookupSingleRow: the fan-out used to walk every finding_status row twice per
// comment (bell alerts + assignee email) to find the one assignee; with the single-row probe both
// lookups must read exactly that row and the flow's status list must never be walked.
func TestComments_AssigneeLookupSingleRow(t *testing.T) {
	rt, fb := newCommentFanoutRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// Give the finding an assignee first (the triage write's own probe call is
	// not this test's subject; only the deltas after it are asserted).
	set := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "r1:b1", "ruleId": "r1", "status": "in_progress", "assigneeId": "bob",
	})
	checkStatus(t, set, http.StatusOK)
	// The assignment's own alert to bob is detached too; let it land before taking the baseline.
	waitForFanoutQuiescence(t, fb)
	alertsBefore := fb.alerts.Load()

	add := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "r1:b1", "body": "looks fixed to me",
	})
	checkStatus(t, add, http.StatusOK)
	// The fan-out is detached; bob (the assignee, not the actor) gets exactly
	// one comment_added bell alert, which also proves the assignee was found.
	waitUntil(t, "assignee comment alert", func() bool { return fb.alerts.Load() == alertsBefore+1 })
	waitForFanoutQuiescence(t, fb)

	if got := fb.lists.Load(); got != 0 {
		t.Errorf("comment fan-out listed the flow's statuses %d times, want 0 (single-row probe)", got)
	}
	if got := fb.probes.Load(); got < 1 {
		t.Errorf("comment fan-out used the single-row probe %d times, want at least 1", got)
	}
}
