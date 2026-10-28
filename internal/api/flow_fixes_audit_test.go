package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
)

// TestFixRoutes_RecordFlowFixApply (R-security-4): the HTTP fix routes leave a flow.fix_apply row naming the fix,
// whether it was the unsafe opt-in, and how it ended; the batch route also the number applied.
func TestFixRoutes_RecordFlowFixApply(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "u-owner", "owner@acme.io", auth.RoleMember)
	seedFixableFlow(t, rt, "f-fix-audit", "u-owner")
	seedThreeFixableFlow(t, rt, "f-fix-audit-batch", "u-owner")
	bearer := jwtBearer(t, rt, "u-owner", "owner@acme.io")
	rows := captureModuleAudit(t, rt, func(h *FlowHandler, r *audit.Recorder) { h.audit = r })

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix", bearer, map[string]any{
		"flowId": "f-fix-audit", "blockId": "b1", "fixType": "wrap-error-handler", "ruleId": "unhandled-error",
	})
	checkStatus(t, rr, http.StatusOK)
	ev := onlyAudit(t, rows)
	if ev.Action != audit.ActionFlowFixApply || ev.ResourceID != "f-fix-audit" || ev.UserID != "u-owner" {
		t.Errorf("single fix row = %+v", ev)
	}
	for k, v := range map[string]string{"source": "http", "ruleId": "unhandled-error", "fixType": "wrap-error-handler", "blockId": "b1", "unsafe": "false", "outcome": "applied"} {
		if ev.Meta[k] != v {
			t.Errorf("single fix meta[%s] = %q, want %q (meta %v)", k, ev.Meta[k], v, ev.Meta)
		}
	}

	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix-batch", bearer, map[string]any{
		"flowId": "f-fix-audit-batch", "rules": []string{"unhandled-error"},
	})
	checkStatus(t, rr, http.StatusOK)
	ev = onlyAudit(t, rows)
	if ev.Action != audit.ActionFlowFixApply || ev.ResourceID != "f-fix-audit-batch" {
		t.Errorf("batch row = %+v", ev)
	}
	for k, v := range map[string]string{"source": "http", "rules": "unhandled-error", "applied": "3", "outcome": "applied"} {
		if ev.Meta[k] != v {
			t.Errorf("batch meta[%s] = %q, want %q (meta %v)", k, ev.Meta[k], v, ev.Meta)
		}
	}
}
