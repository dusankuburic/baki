package api

import (
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/storage/filesystem"
)

// Desktop-era analysis endpoints operate on server-local folders or the
// process-global analyzer cache, so they must be unavailable in cloud (JWT) mode:
//   - /api/analysis/batch would be an authenticated arbitrary-directory read (LFI)
//   - /api/analysis/dashboard would leak cross-tenant aggregates from the shared cache
func TestAnalysisDesktopEndpoints_ForbiddenInCloud(t *testing.T) {
	rt := newJWTTestRouter(t) // JWTEnabled = true (cloud)
	bearer := jwtBearer(t, rt, "user-1", "u1@example.com")

	batch := doRequestWithAuth(t, rt, "POST", "/api/analysis/batch", bearer, map[string]string{"folderPath": "/etc"})
	checkStatus(t, batch, 403)

	dash := doRequestWithAuth(t, rt, "GET", "/api/analysis/dashboard", bearer, nil)
	checkStatus(t, dash, 403)
}

// In local/desktop mode (JWTEnabled = false) the same endpoints stay available.
func TestAnalysisDesktopEndpoints_AvailableInLocal(t *testing.T) {
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	rt := newTestRouter(fs, false) // local mode

	// Session analytics return aggregate stats (empty cache → zeros), 200.
	dash := doRequest(t, rt, "GET", "/api/analysis/dashboard", nil)
	checkStatus(t, dash, 200)

	// Batch reads a real folder containing a .txt; non-PAD content surfaces as an
	// error row but the request itself still succeeds (200).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("not a pad flow"), 0600); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	res := doRequest(t, rt, "POST", "/api/analysis/batch", map[string]string{"folderPath": dir})
	checkStatus(t, res, 200)
}
