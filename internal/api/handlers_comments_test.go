package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// ── Finding Comments API ──────────────────────────────────────────

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

// ── Share Token API ───────────────────────────────────────────────

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

// ── Preview-Fix: cloud mode should reject ─────────────────────────

// Cloud-mode preview-fix is now ENABLED (was 403) — see
// TestPreviewFix_CloudMode_Works in handlers_flow_cloudfix_test.go.
func TestPreviewFix_CloudModeForbidden(t *testing.T) {
	t.Skip("cloud-mode preview-fix is now enabled; see TestPreviewFix_CloudMode_Works")
}

// Cloud-mode apply-fix is now ENABLED (was 403) — see TestApplyFix_CloudMode_Works
// in handlers_flow_cloudfix_test.go. This stub kept the import set stable.
func TestApplyFix_CloudModeForbidden(t *testing.T) {
	t.Skip("cloud-mode apply-fix is now enabled; see TestApplyFix_CloudMode_Works")
}

// ── SARIF Export ──────────────────────────────────────────────────

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

// ── Webhook Notifier ──────────────────────────────────────────────

func TestWebhookNotifier_DisabledByDefault(t *testing.T) {
	wn := service.NewWebhookNotifier(nil)
	if wn.Enabled() {
		t.Error("expected webhook notifier to be disabled when PAD_WEBHOOK_URL is unset")
	}
	// NotifyAnalysis should be a safe no-op
	wn.NotifyAnalysis("test-flow", nil)
}
