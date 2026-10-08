package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// captureModuleAudit points the recorder of rt's module M at a fresh capture channel.
func captureModuleAudit[M Module](t *testing.T, rt *Router, set func(M, *audit.Recorder)) chan *storageif.AuditEvent {
	t.Helper()
	ch := make(chan *storageif.AuditEvent, 8)
	set(moduleOf[M](t, rt), audit.NewCapture(ch))
	return ch
}

// serveChain serves requests through the production stack (BuildHandler), whose RequestInfo layer records the
// client address the recorder reads; Router.ServeHTTP alone leaves it empty.
func serveChain(t *testing.T, rt *Router) func(method, path, authHeader string, body any) *httptest.ResponseRecorder {
	t.Helper()
	h, limiters := BuildHandler(rt, config.Default(), nil)
	for _, l := range limiters {
		l.Stop()
	}
	return func(method, path, authHeader string, body any) *httptest.ResponseRecorder {
		var b bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&b).Encode(body); err != nil {
				t.Fatalf("encode body: %v", err)
			}
		}
		req := httptest.NewRequest(method, path, &b)
		req.Header.Set("Authorization", authHeader)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
}

func onlyAudit(t *testing.T, ch chan *storageif.AuditEvent) *storageif.AuditEvent {
	t.Helper()
	select {
	case ev := <-ch:
		select {
		case extra := <-ch:
			t.Fatalf("a second audit row: %+v", extra)
		default:
		}
		return ev
	default:
		t.Fatal("no audit row recorded")
		return nil
	}
}

// The rows the injected recorder writes keep every column they had before internal/audit (the caller's claims,
// the client address middleware.ClientIP gives for an untrusted peer, the action, resource and meta) and add the
// flow's organisation when the handler has the flow's header in hand.
func TestRecordSites_KeepTheColumnsAndAddTheFlowOrg(t *testing.T) {
	store := testutil.NewFakeBackend()
	rt := newTestRouter(store, true)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	ctx := context.Background()
	for _, f := range []*storageif.FlowDocument{
		{ID: "f-org", Name: "Org flow", OwnerID: "alice", OrganizationID: "org-1", Content: []byte(`{}`)},
		{ID: "f-own", Name: "Own flow", OwnerID: "alice", Content: []byte(`{}`)},
	} {
		if err := store.SaveFlow(ctx, f); err != nil {
			t.Fatalf("seed flow: %v", err)
		}
	}
	analysis := captureModuleAudit(t, rt, func(h *AnalysisHandler, r *audit.Recorder) { h.audit = r })
	library := captureModuleAudit(t, rt, func(h *LibraryHandler, r *audit.Recorder) { h.audit = r })
	do := serveChain(t, rt)

	check := func(name string, ev *storageif.AuditEvent, action, resourceID, orgID string, meta map[string]string) {
		t.Helper()
		if ev.UserID != "alice" || ev.Email != "alice@example.com" || ev.IP != "192.0.2.1" {
			t.Errorf("%s: user %q email %q ip %q, want alice / alice@example.com / 192.0.2.1", name, ev.UserID, ev.Email, ev.IP)
		}
		if ev.Action != action || ev.ResourceType != "flow" || ev.ResourceID != resourceID || ev.OrgID != orgID {
			t.Errorf("%s: %s %s/%s org %q, want %s flow/%s org %q", name, ev.Action, ev.ResourceType, ev.ResourceID, ev.OrgID, action, resourceID, orgID)
		}
		if !reflect.DeepEqual(ev.Meta, meta) {
			t.Errorf("%s: meta %v, want %v", name, ev.Meta, meta)
		}
	}

	rr := do(http.MethodPost, "/api/analysis/comments/add", bearer,
		map[string]string{"flowId": "f-org", "findingKey": "k1", "body": "looks fine"})
	checkStatus(t, rr, http.StatusOK)
	var comment storageif.FindingComment
	decodeJSON(t, rr, &comment)
	check("comment add (header in hand)", onlyAudit(t, analysis), audit.ActionCommentAdd, "f-org", "org-1",
		map[string]string{"findingKey": "k1", "commentId": comment.ID})

	checkStatus(t, do(http.MethodPost, "/api/analysis/baseline/clear", bearer,
		map[string]string{"flowId": "f-org"}), http.StatusOK)
	check("baseline clear (permission check only)", onlyAudit(t, analysis), audit.ActionBaselineClear, "f-org", "", nil)

	checkStatus(t, do(http.MethodDelete, "/api/library/f-org", bearer, nil), http.StatusOK)
	check("library delete (the service's header)", onlyAudit(t, library), audit.ActionFlowDelete, "f-org", "org-1", nil)

	checkStatus(t, do(http.MethodDelete, "/api/library/f-own", bearer, nil), http.StatusOK)
	check("library delete of a personal flow", onlyAudit(t, library), audit.ActionFlowDelete, "f-own", "", nil)
}

// Local mode with a store (local+db): the user comes from the claims, which local mode has none of, so these rows
// carry the empty user_id they always had although the request acts as the local actor.
func TestRecordSites_LocalModeKeepsAnEmptyUser(t *testing.T) {
	store := testutil.NewFakeBackend()
	rt := newTestRouter(store, false)
	ch := captureModuleAudit(t, rt, func(h *AnalysisHandler, r *audit.Recorder) { h.audit = r })
	do := serveChain(t, rt)

	checkStatus(t, do(http.MethodDelete, "/api/governance/alerts", "Bearer "+testToken, nil), http.StatusOK)
	ev := onlyAudit(t, ch)
	if ev.UserID != "" || ev.Email != "" || ev.IP != "192.0.2.1" || ev.Action != audit.ActionAlertClear || ev.ResourceType != "alert" {
		t.Errorf("row %+v, want user \"\", email \"\", ip 192.0.2.1, %s alert", ev, audit.ActionAlertClear)
	}
}
