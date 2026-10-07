package api

import (
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/audit"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestAuditEventView_UsesTheKeysClientsRead pins the admin audit payload. The
// handler used to render the storage struct, which has no JSON tags, so the
// table — which reads action, email, createdAt and meta — showed blank cells
// and "Invalid Date" on every row.
func TestAuditEventView_UsesTheKeysClientsRead(t *testing.T) {
	views := toAuditEventViews([]*storageif.AuditEvent{{
		ID: "evt-1", UserID: "user-1", Email: "admin@example.com", Action: audit.ActionFlowUpload,
		ResourceType: "flow", ResourceID: "flow-1", IP: "127.0.0.1",
		Meta: map[string]string{"name": "Billing"}, CreatedAt: time.Now().UTC(),
	}})
	if len(views) != 1 {
		t.Fatalf("expected one view, got %d", len(views))
	}
	out := marshalToMap(t, views[0])
	for _, key := range []string{"id", "userId", "email", "action", "resourceType", "resourceId", "ip", "meta", "createdAt"} {
		if _, ok := out[key]; !ok {
			t.Errorf("audit view is missing %q: got %v", key, keysOf(out))
		}
	}
}

// TestUserStatsView_UsesTheKeysTheOverviewReads pins the admin overview's
// stats payload, including the "none" bucket for accounts without a valid role.
func TestUserStatsView_UsesTheKeysTheOverviewReads(t *testing.T) {
	out := marshalToMap(t, toUserStatsView(&storageif.UserStats{
		Total:      7,
		ByRole:     map[string]int{"admin": 1, "member": 4, "viewer": 1, "guest": 0, storageif.UserRoleNone: 1},
		Unverified: 2,
		Locked:     1,
	}))
	for _, key := range []string{"total", "byRole", "unverified", "locked"} {
		if _, ok := out[key]; !ok {
			t.Errorf("stats view is missing %q: got %v", key, keysOf(out))
		}
	}
	byRole, _ := out["byRole"].(map[string]any)
	for _, key := range []string{"admin", "member", "viewer", "guest", "none"} {
		if _, ok := byRole[key]; !ok {
			t.Errorf("byRole is missing %q: got %v", key, byRole)
		}
	}
	if byRole["none"] != float64(1) {
		t.Errorf("none must carry the roleless count, got %v", byRole["none"])
	}
}

// TestFlowVersionView_UsesTheKeysThePanelReads: version history rendered
// "v undefined" and "No comment" for every snapshot for the same reason.
func TestFlowVersionView_UsesTheKeysThePanelReads(t *testing.T) {
	out := marshalToMap(t, toFlowVersionView(&storageif.FlowVersion{
		ID: "v1", FlowID: "flow-1", Version: 3, Comment: "before batch fix",
		Metadata:  storageif.FlowMetadata{BlockCount: 12, SubflowCount: 2},
		CreatedBy: "user-1", CreatedAt: time.Now().UTC(),
	}))
	for _, key := range []string{"id", "flowId", "version", "comment", "metadata", "createdBy", "createdAt"} {
		if _, ok := out[key]; !ok {
			t.Errorf("version view is missing %q: got %v", key, keysOf(out))
		}
	}
	meta, _ := out["metadata"].(map[string]any)
	if _, ok := meta["blockCount"]; !ok {
		t.Errorf("version metadata is missing blockCount: got %v", meta)
	}
	// The snapshot body is not part of the history list.
	if _, ok := out["content"]; ok {
		t.Errorf("the version view must not carry content: got %v", out["content"])
	}
}

// TestStoredShapesStaySQLCompatible guards the other direction. flows.metadata
// is READ BY SQL — ORDER BY metadata->>'BlockCount', the dashboard's
// SubflowCount sums, and an index built on that expression — and the audit
// spill queue persists AuditEvent as JSON lines. Tagging either struct for the
// wire (which is what the views above are for) silently turns those queries
// into zeroes and makes spooled events unreadable.
func TestStoredShapesStaySQLCompatible(t *testing.T) {
	meta := marshalToMap(t, storageif.FlowMetadata{BlockCount: 4, SubflowCount: 1})
	for _, key := range []string{"BlockCount", "SubflowCount"} {
		if _, ok := meta[key]; !ok {
			t.Errorf("flows.metadata must keep the %q key SQL reads: got %v", key, keysOf(meta))
		}
	}
	event := marshalToMap(t, storageif.AuditEvent{Action: "auth.login"})
	if _, ok := event["Action"]; !ok {
		t.Errorf("spilled audit events must keep their stored field names: got %v", keysOf(event))
	}
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
