package api

import (
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/storage/filesystem"
)

// load-path and load-folder read server-local filesystem paths, so in cloud
// (JWT) mode they would be an authenticated arbitrary-path read (LFI) — and they
// also Clear() the process-global analyzer cache, which would wipe every tenant's
// session reports. They must be forbidden in cloud mode; uploads are the cloud
// equivalent. Mirrors handlers_analysis_gate_test.go.
func TestFlowLocalPathEndpoints_ForbiddenInCloud(t *testing.T) {
	rt := newJWTTestRouter(t) // JWTEnabled = true (cloud)
	bearer := jwtBearer(t, rt, "user-1", "u1@example.com")

	fromPath := doRequestWithAuth(t, rt, "POST", "/api/flow/load-path", bearer, map[string]string{"path": "/etc/passwd"})
	checkStatus(t, fromPath, 403)

	folder := doRequestWithAuth(t, rt, "POST", "/api/flow/load-folder", bearer, map[string]string{"path": "/etc"})
	checkStatus(t, folder, 403)
}

// In local/desktop mode (JWTEnabled = false) the same endpoints stay available
// and successfully load a real on-disk flow.
func TestFlowLocalPathEndpoints_AvailableInLocal(t *testing.T) {
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	rt := newTestRouter(fs, false) // local mode

	// A minimal valid PAD flow (one subflow with one action).
	flow := "#Region \"Main\"\n    Display.ShowMessageBox Message: $'''hi'''\n#EndRegion\n"

	dir := t.TempDir()
	file := filepath.Join(dir, "flow.txt")
	if err := os.WriteFile(file, []byte(flow), 0600); err != nil {
		t.Fatalf("write flow: %v", err)
	}

	fromPath := doRequest(t, rt, "POST", "/api/flow/load-path", map[string]string{"path": file})
	checkStatus(t, fromPath, 200)

	folder := doRequest(t, rt, "POST", "/api/flow/load-folder", map[string]string{"path": dir})
	checkStatus(t, folder, 200)
}
