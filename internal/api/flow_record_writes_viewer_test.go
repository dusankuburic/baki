package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestFlowRecordWrites_ViewersRefusedAtTheService: the triage, baseline, comment and share-link writes need FlowEdit,
// and the service is what enforces it. Row-level security scopes these tables to the flow's visibility only, so a
// viewer's write would pass the database (ADR 0013). Every viewer kind (a viewer collaborator, an org viewer, an org
// guest) gets 403 on every write, and on the share-link list, which needs FlowEdit too, and leaves the stored records
// as they were; an org admin sending the same requests gets through, so the refusals are about the role, not the
// request.
func TestFlowRecordWrites_ViewersRefusedAtTheService(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	ctx := context.Background()
	backend := rt.security.Backend
	orgID := seedOrg(t, rt, "acme", "alice")
	addOrgMember(t, rt, orgID, "orgviewer", auth.RoleViewer)
	addOrgMember(t, rt, orgID, "orgguest", auth.RoleGuest)
	addOrgMember(t, rt, orgID, "orgadmin", auth.RoleAdmin)
	const flowID = "flow1"
	if err := backend.SaveFlow(ctx, &storageif.FlowDocument{
		ID: flowID, Name: "t", OwnerID: "alice", OrganizationID: orgID,
		Content: json.RawMessage(`{"id":"flow1","name":"t","subflows":[]}`),
	}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	seedUserWithRole(t, rt, "collabviewer", "cv@example.com", auth.RoleMember)
	if err := backend.AddCollaborator(ctx, flowID, &storageif.Collaborator{UserID: "collabviewer", Email: "cv@example.com", Permission: "viewer"}); err != nil {
		t.Fatalf("add collaborator: %v", err)
	}

	// The owner's records the viewers try to change.
	const key = "rule-x:b1"
	if err := backend.SetFindingStatus(ctx, &storageif.FindingStatus{FlowID: flowID, FindingKey: key, Status: "acknowledged", UpdatedBy: "alice"}); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	if err := backend.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: flowID, Keys: []string{key}, CreatedBy: "alice"}); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}
	comment := &storageif.FindingComment{FlowID: flowID, FindingKey: key, AuthorID: "alice", Body: "by alice"}
	if err := backend.AddFindingComment(ctx, comment); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	link := &storageif.ShareToken{FlowID: flowID, TokenHash: "seeded-hash", CreatedBy: "alice"}
	if err := backend.CreateShareToken(ctx, link); err != nil {
		t.Fatalf("seed share link: %v", err)
	}

	writes := []struct {
		path string
		body map[string]any
	}{
		{"/api/analysis/triage/set", map[string]any{"flowId": flowID, "findingKey": key, "status": "resolved"}},
		{"/api/analysis/triage/set-batch", map[string]any{"flowId": flowID, "items": []map[string]any{{"findingKey": key, "status": "resolved"}}}},
		{"/api/analysis/triage/clear", map[string]any{"flowId": flowID, "findingKey": key}},
		{"/api/analysis/baseline/set", map[string]any{"flowId": flowID}},
		{"/api/analysis/baseline/clear", map[string]any{"flowId": flowID}},
		{"/api/analysis/comments/add", map[string]any{"flowId": flowID, "findingKey": key, "body": "hi"}},
		{"/api/analysis/comments/delete", map[string]any{"flowId": flowID, "commentId": comment.ID}},
		{"/api/flow/share/create", map[string]any{"flowId": flowID}},
		{"/api/flow/share/list", map[string]any{"flowId": flowID}},
		{"/api/flow/share/revoke", map[string]any{"flowId": flowID, "tokenId": link.ID}},
	}

	for _, viewer := range []string{"collabviewer", "orgviewer", "orgguest"} {
		bearer := jwtBearer(t, rt, viewer, viewer+"@example.com")
		for _, w := range writes {
			rr := doRequestWithAuth(t, rt, http.MethodPost, w.path, bearer, w.body)
			if rr.Code != http.StatusForbidden {
				t.Errorf("%s %s: status %d, want 403 — body: %s", viewer, w.path, rr.Code, rr.Body.String())
			}
		}
	}

	statuses, err := backend.ListFindingStatuses(ctx, flowID)
	if err != nil || len(statuses) != 1 || statuses[0].Status != "acknowledged" || statuses[0].UpdatedBy != "alice" {
		t.Errorf("finding statuses after the viewers' writes = %+v, %v; want alice's one row", statuses, err)
	}
	if bl, err := backend.GetFlowBaseline(ctx, flowID); err != nil || bl == nil || bl.CreatedBy != "alice" {
		t.Errorf("baseline after the viewers' writes = %+v, %v; want alice's", bl, err)
	}
	if cs, err := backend.ListFindingComments(ctx, flowID, key); err != nil || len(cs) != 1 || cs[0].ID != comment.ID {
		t.Errorf("comments after the viewers' writes = %+v, %v; want alice's one", cs, err)
	}
	if ls, err := backend.ListShareTokens(ctx, flowID); err != nil || len(ls) != 1 || ls[0].ID != link.ID {
		t.Errorf("share links after the viewers' writes = %+v, %v; want alice's one", ls, err)
	}

	adminBearer := jwtBearer(t, rt, "orgadmin", "orgadmin@example.com")
	for _, w := range writes {
		rr := doRequestWithAuth(t, rt, http.MethodPost, w.path, adminBearer, w.body)
		checkStatus(t, rr, http.StatusOK)
	}
}
