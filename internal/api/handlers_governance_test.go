package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// Helper: seed a governance alert directly through the backend.
func seedGovAlert(t *testing.T, rt *Router, id, flowID, flowName, typ string) {
	t.Helper()
	if err := rt.security.Backend.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID: flowID, Name: flowName, Content: []byte(`{}`), OwnerID: "alice",
	}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	if err := rt.security.Backend.RecordGovernanceAlert(context.Background(), &storageif.GovernanceAlert{
		ID: id, FlowID: flowID, FlowName: flowName, Type: typ,
		Title: "New findings in " + flowName, Severity: "error",
		NewErrors: 2, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
}

func TestGovernanceAlerts_ListAndUnreadCount(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "alice", "alice@example.com", "admin")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	seedGovAlert(t, rt, "f1|drift|e2w1", "f1", "Flow One", "drift")
	seedGovAlert(t, rt, "f2|health_regression|h70<85", "f2", "Flow Two", "health_regression")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/governance/alerts", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var alerts []storageif.GovernanceAlert
	decodeJSON(t, rr, &alerts)
	if len(alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(alerts))
	}

	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/governance/alerts/unread-count", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var cnt struct {
		Count int `json:"count"`
	}
	decodeJSON(t, rr, &cnt)
	if cnt.Count != 2 {
		t.Errorf("expected unread count 2, got %d", cnt.Count)
	}
}

func TestGovernanceAlerts_MarkReadClearsBadge(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "alice", "alice@example.com", "admin")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	seedGovAlert(t, rt, "f1|drift|e1w0", "f1", "Flow One", "drift")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/governance/alerts/read", bearer, map[string]string{"id": "f1|drift|e1w0"})
	checkStatus(t, rr, http.StatusOK)

	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/governance/alerts/unread-count", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var cnt struct {
		Count int `json:"count"`
	}
	decodeJSON(t, rr, &cnt)
	if cnt.Count != 0 {
		t.Errorf("expected 0 unread after read, got %d", cnt.Count)
	}
}

func TestGovernanceAlerts_MarkAllReadAndDismiss(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "alice", "alice@example.com", "admin")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	seedGovAlert(t, rt, "f1|drift|e2w0", "f1", "Flow One", "drift")
	seedGovAlert(t, rt, "f2|drift|e1w0", "f2", "Flow Two", "drift")

	// Mark all read clears the badge.
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/governance/alerts/read-all", bearer, map[string]string{})
	checkStatus(t, rr, http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/governance/alerts/unread-count", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var cnt struct {
		Count int `json:"count"`
	}
	decodeJSON(t, rr, &cnt)
	if cnt.Count != 0 {
		t.Fatalf("expected 0 unread after read-all, got %d", cnt.Count)
	}

	// Dismiss one → it disappears from the default list.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/governance/alerts/dismiss", bearer, map[string]string{"id": "f1|drift|e2w0"})
	checkStatus(t, rr, http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/governance/alerts", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var alerts []storageif.GovernanceAlert
	decodeJSON(t, rr, &alerts)
	if len(alerts) != 1 || alerts[0].ID != "f2|drift|e1w0" {
		t.Errorf("expected only the non-dismissed alert, got %+v", alerts)
	}

	// Clear removes dismissed alerts permanently.
	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/governance/alerts", bearer, map[string]string{})
	checkStatus(t, rr, http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/governance/alerts?includeDismissed=true", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	decodeJSON(t, rr, &alerts)
	if len(alerts) != 1 {
		t.Errorf("expected 1 alert after clear (dismissed removed), got %d", len(alerts))
	}
}

func TestGovernanceAlerts_MarkReadMissingIdReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "alice", "alice@example.com", "admin")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/governance/alerts/read", bearer, map[string]string{})
	checkStatus(t, rr, http.StatusBadRequest)
}
