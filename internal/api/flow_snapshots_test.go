package api

import (
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

const snapshotAPISource = "#Region \"Main\"\nWAIT 1\n#EndRegion\n"

// The undo endpoints with the durable ring: a save recorded through one replica lists and restores through
// another, an empty ring is an empty array, and an id that names no snapshot of the flow (unknown, or taken from
// another flow) is a 404 instead of the 500 it was.
func TestSnapshotRoutes_DurableRingAcrossReplicas(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	backend, ok := rt.security.Backend.(*testutil.FakeBackend)
	if !ok {
		t.Fatalf("test router backend is %T, want the fake", rt.security.Backend)
	}
	seedUserWithRole(t, rt, "u-owner", "u-owner@acme.io", auth.RoleMember)
	seedCloudFlowWithSource(t, rt, "f-undo", "u-owner", snapshotAPISource)
	seedCloudFlowWithSource(t, rt, "f-other", "u-owner", snapshotAPISource)
	h := moduleOf[*FlowHandler](t, rt)
	replicaA, replicaB := h.flowSvc, service.NewFlowService(&testutil.CountingNotifier{}, nil, h.flowSvc.DocProvider(), backend, service.NewAuthzService(backend, nil), nil)
	service.WithSnapshotStore(backend)(replicaA)
	service.WithSnapshotStore(backend)(replicaB)
	session := jwtBearer(t, rt, "u-owner", "u-owner@acme.io")

	list := func(flow string) []map[string]any {
		t.Helper()
		rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/flow/snapshots?flowId="+flow, session, nil)
		checkStatus(t, rr, http.StatusOK)
		var out struct {
			Snapshots []map[string]any `json:"snapshots"`
		}
		decodeJSON(t, rr, &out)
		if out.Snapshots == nil {
			t.Fatalf("snapshots decoded as null from %s", rr.Body.String())
		}
		return out.Snapshots
	}
	if got := list("f-undo"); len(got) != 0 {
		t.Fatalf("a flow without a save lists %v", got)
	}
	if !strings.Contains(doRequestWithAuth(t, rt, http.MethodGet, "/api/flow/snapshots?flowId=f-undo", session, nil).Body.String(), `"snapshots":[]`) {
		t.Error("an empty ring is not an empty JSON array")
	}

	h.flowSvc = replicaA
	save := func(flow, source string) {
		t.Helper()
		checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/save-source", session, map[string]string{"flowId": flow, "source": source}), http.StatusOK)
	}
	save("f-undo", strings.Replace(snapshotAPISource, "WAIT 1", "WAIT 2", 1))
	save("f-other", strings.Replace(snapshotAPISource, "WAIT 1", "WAIT 3", 1))

	h.flowSvc = replicaB // the next request lands on another replica
	snaps := list("f-undo")
	if len(snaps) != 1 || snaps[0]["label"] != "before source save" {
		t.Fatalf("replica B lists %v after A's save", snaps)
	}
	id := snaps[0]["id"].(string)

	restore := func(flow, snapshot string) int {
		return doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/snapshots/restore", session, map[string]string{"flowId": flow, "snapshotId": snapshot}).Code
	}
	if code := restore("f-undo", "no-such-snapshot"); code != http.StatusNotFound {
		t.Errorf("restoring an unknown snapshot = %d, want 404", code)
	}
	otherID := list("f-other")[0]["id"].(string)
	if code := restore("f-undo", otherID); code != http.StatusNotFound {
		t.Errorf("restoring another flow's snapshot = %d, want 404", code)
	}
	if code := restore("f-undo", id); code != http.StatusOK {
		t.Fatalf("restore on replica B = %d, want 200", code)
	}
	if stored, err := backend.LoadFlow(t.Context(), "f-undo"); err != nil || stored.Source != snapshotAPISource {
		t.Errorf("stored source after the restore = %q, %v; want the original", stored.Source, err)
	}
	if got := list("f-undo"); len(got) != 2 || got[1]["label"] != "before restore" {
		t.Errorf("after the restore the ring lists %v, want the save's snapshot and 'before restore'", got)
	}
}
