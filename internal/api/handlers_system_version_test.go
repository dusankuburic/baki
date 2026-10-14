package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// versionedBackend is a backend that can tell its schema version, as the Postgres one does.
type versionedBackend struct {
	*testutil.FakeBackend
	version int
	err     error
}

func (b *versionedBackend) CurrentSchemaVersion(context.Context) (int, error) {
	return b.version, b.err
}

func adminHealthBody(t *testing.T, h *SystemHandler) map[string]json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleAdminSystemHealth(w, httptest.NewRequest(http.MethodGet, "/api/admin/system/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("admin health: %d %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func jsonString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s is not a string: %v", raw, err)
	}
	return s
}

// During a rolling deploy the replicas run two schemas and two binaries: the health page says which this one is.
func TestAdminSystemHealth_ReportsSchemaAndBinaryVersion(t *testing.T) {
	h := &SystemHandler{health: service.NewHealthService(&versionedBackend{FakeBackend: testutil.NewFakeBackend(), version: 46}, nil)}
	body := adminHealthBody(t, h)

	var schema int
	if err := json.Unmarshal(body["schemaVersion"], &schema); err != nil || schema != 46 {
		t.Errorf("schemaVersion = %s, want 46", body["schemaVersion"])
	}
	if got := jsonString(t, body["binaryVersion"]); got != service.Version {
		t.Errorf("binaryVersion = %q, want %q", got, service.Version)
	}
	if got := jsonString(t, body["instanceId"]); got != lifecycle.InstanceID() {
		t.Errorf("instanceId = %q, want %q", got, lifecycle.InstanceID())
	}
	// Additive: what the page showed before is still there.
	for _, k := range []string{"database", "blob", "redis", "overall"} {
		if _, ok := body[k]; !ok {
			t.Errorf("response lost %q", k)
		}
	}
}

// A backend that cannot tell its schema (the fake), or fails to, leaves the field out; the rest of the answer stands.
func TestAdminSystemHealth_SchemaVersionIsOptional(t *testing.T) {
	cases := map[string]*SystemHandler{
		"backend without a schema version": {health: service.NewHealthService(testutil.NewFakeBackend(), nil)},
		"schema query fails":               {health: service.NewHealthService(&versionedBackend{FakeBackend: testutil.NewFakeBackend(), err: errors.New("down")}, nil)},
		"no store (desktop)":               {health: service.NewHealthService(nil, nil)},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			body := adminHealthBody(t, h)
			if v, ok := body["schemaVersion"]; ok {
				t.Errorf("schemaVersion = %s, want it left out", v)
			}
			if got := jsonString(t, body["binaryVersion"]); got != service.Version {
				t.Errorf("binaryVersion = %q, want %q", got, service.Version)
			}
			if got := jsonString(t, body["overall"]); got != "ok" {
				t.Errorf("overall = %q, want ok", got)
			}
		})
	}
}
