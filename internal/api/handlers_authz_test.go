package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestRequireRole_CloudMode verifies that admin-gated endpoints enforce the
// admin role server-side in cloud (JWT) mode.
func TestRequireRole_CloudMode(t *testing.T) {
	rt := newTestRouter(nil, true)

	cases := []struct {
		name     string
		role     auth.Role
		want     bool
		wantCode int
	}{
		{"admin allowed", auth.RoleAdmin, true, http.StatusOK},
		{"member forbidden", auth.RoleMember, false, http.StatusForbidden},
		{"viewer forbidden", auth.RoleViewer, false, http.StatusForbidden},
		{"guest forbidden", auth.RoleGuest, false, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/admin/users/list", nil)
			req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{Role: tc.role}))
			rec := httptest.NewRecorder()

			got := rt.security.RequireRole(rec, req, auth.RoleAdmin)
			if got != tc.want {
				t.Fatalf("requireRole(role=%s) = %v, want %v", tc.role, got, tc.want)
			}
			if !tc.want && rec.Code != tc.wantCode {
				t.Fatalf("requireRole(role=%s) status = %d, want %d", tc.role, rec.Code, tc.wantCode)
			}
		})
	}
}

// TestRequireRole_MissingClaims verifies that a request with no claims in
// context (no/invalid token) is rejected with 401 in cloud mode.
func TestRequireRole_MissingClaims(t *testing.T) {
	rt := newTestRouter(nil, true)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/list", nil)
	rec := httptest.NewRecorder()

	if rt.security.RequireRole(rec, req, auth.RoleAdmin) {
		t.Fatal("requireRole with nil claims should be denied")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("requireRole with nil claims status = %d, want 401", rec.Code)
	}
}

// TestRequireRole_LocalModeBypass documents the intentional behaviour that in
// local (single-user, token-gated) mode role checks are skipped.
func TestRequireRole_LocalModeBypass(t *testing.T) {
	rt := newTestRouter(nil, false)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/list", nil)
	rec := httptest.NewRecorder()

	if !rt.security.RequireRole(rec, req, auth.RoleAdmin) {
		t.Fatal("local mode should allow admin actions without JWT claims")
	}
}

// seedCollaborator grants userID the given permission on flowID.
func seedCollaborator(t *testing.T, rt *Router, flowID, userID, perm string) {
	t.Helper()
	err := rt.security.Backend.AddCollaborator(context.Background(), flowID, &storageif.Collaborator{
		UserID:     userID,
		Email:      userID + "@example.com",
		Permission: perm,
		GrantedAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed collaborator: %v", err)
	}
}

// A non-owner, non-collaborator must not read another user's chat history.
func TestChatGet_NonOwnerForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	bobToken := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get", bobToken, map[string]any{
		"flowId": "flow-a", "provider": "openai",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

// The flow owner may read their own chat history.
func TestChatGet_OwnerAllowed(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	aliceToken := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get", aliceToken, map[string]any{
		"flowId": "flow-a", "provider": "openai",
	})
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusNotFound {
		t.Errorf("owner should be allowed, got %d — %s", rr.Code, rr.Body.String())
	}
}

// An unknown flow id yields 404 (and reveals nothing about other flows).
func TestChatGet_MissingFlowNotFound(t *testing.T) {
	rt := newJWTTestRouter(t)
	tok := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get", tok, map[string]any{
		"flowId": "ghost", "provider": "openai",
	})
	checkStatus(t, rr, http.StatusNotFound)
}

// A viewer collaborator must not overwrite chat (save requires editor).
func TestChatSave_ViewerCollaboratorForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	seedCollaborator(t, rt, "flow-a", "bob", "viewer")
	bobToken := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/save", bobToken, map[string]any{
		"flowId": "flow-a", "provider": "openai", "messages": []any{},
	})
	checkStatus(t, rr, http.StatusForbidden)
}

// An editor collaborator may save.
func TestChatSave_EditorCollaboratorAllowed(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	seedCollaborator(t, rt, "flow-a", "bob", "editor")
	bobToken := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/save", bobToken, map[string]any{
		"flowId": "flow-a", "provider": "openai", "messages": []any{},
	})
	if rr.Code == http.StatusForbidden {
		t.Errorf("editor should be allowed to save, got 403 — %s", rr.Body.String())
	}
}

// Clearing another user's chat must be forbidden.
func TestChatClear_NonOwnerForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	bobToken := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/clear", bobToken, map[string]any{
		"flowId": "flow-a", "provider": "openai",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

// Regression: a flow carrying an OrganizationID must NOT be readable by a
// non-member.
func TestLibraryGet_OrgFlowNonMemberForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	if err := rt.security.Backend.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID: "flow-org", Name: "test", OwnerID: "alice", OrganizationID: "org-1",
	}); err != nil {
		t.Fatalf("seed org flow: %v", err)
	}
	bobToken := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow-org", bobToken, nil)
	checkStatus(t, rr, http.StatusForbidden)
}

// Export-to-local-path is desktop-only and must be refused in cloud mode.
func TestExportMarkdown_ForbiddenInCloudMode(t *testing.T) {
	rt := newJWTTestRouter(t)
	tok := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/export/markdown", tok, map[string]any{"path": "x", "flowId": "x"})
	checkStatus(t, rr, http.StatusForbidden)
}

// Analysis endpoints resolve+authorize the target flow by id in cloud mode.
func TestAnalyze_NonOwnerForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	bobToken := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/analyze", bobToken, map[string]any{"flowId": "flow-a"})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestAnalyzeLineage_NonOwnerForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	bobToken := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/lineage", bobToken, map[string]any{
		"flowId": "flow-a", "varName": "X",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestAnalyzeGraph_NonOwnerForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	bobToken := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/graph", bobToken, map[string]any{"flowId": "flow-a"})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestAnalyze_MissingFlowNotFound(t *testing.T) {
	rt := newJWTTestRouter(t)
	tok := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/analyze", tok, map[string]any{"flowId": "ghost"})
	checkStatus(t, rr, http.StatusNotFound)
}
