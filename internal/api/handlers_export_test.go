package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

func TestHandleCompareCurrentWith_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/export/compare")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleExportMarkdown_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/export/markdown")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleExportPDF_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/export/pdf")
	checkStatus(t, rr, http.StatusBadRequest)
}

// TestExportMarkdown_CloudModeIgnoresPath verifies the arbitrary-file-write fix:
// in cloud/JWT mode a caller-supplied `path` must NOT cause a server-side file
// write (only the base64 response is produced). Without the path-drop, a
// registered user could os.WriteFile to /etc/cron.d/*, ~/.ssh/authorized_keys,
// or source files under the repo.
func TestExportMarkdown_CloudModeIgnoresPath(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "alice", "alice@example.com", "member")
	const flowID = "flow-export-1"
	// Seed a flow WITH raw PAD source owned by alice.
	doc := &storageif.FlowDocument{
		ID:      flowID,
		Name:    flowID,
		OwnerID: "alice",
		Source:  "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'hi'\n#EndRegion\n",
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow %s: %v", flowID, err)
	}

	// Resolve the domain doc the same way the export handler does, then prime
	// its analysis report so the export handler has something to render.
	exp := moduleOf[*ExportHandler](t, rt)
	domainDoc, err := exp.flowSvc.GetAuthorized(userActorCtx("alice"), flowID, service.FlowRead)
	if err != nil {
		t.Fatalf("resolve seed flow: %v", err)
	}
	if _, err := exp.analysisSvc.AnalyzeFlow(context.Background(), domainDoc); err != nil {
		t.Fatalf("prime analysis: %v", err)
	}

	outFile := filepath.Join(t.TempDir(), "must-not-exist.md")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/export/markdown", bearer, map[string]any{
		"flowId": flowID,
		"path":   outFile,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (base64 export), got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// The path was supplied but must NOT have been written in cloud mode.
	if _, err := os.Stat(outFile); !os.IsNotExist(err) {
		t.Fatalf("cloud-mode export wrote to the caller-supplied path %q (arbitrary file write not blocked)", outFile)
	}
}

// TestHandleSaveClientExport pins C14's server half: on desktop the CSV/SARIF/
// JUnit exports go through the native save dialog and this route writes the
// browser-rendered text to the chosen path. Only the allowlisted export types
// are written, and cloud mode refuses the route outright.
func TestHandleSaveClientExport(t *testing.T) {
	dir := t.TempDir()

	t.Run("local writes the export", func(t *testing.T) {
		rt := newTestRouter(nil, false)
		path := filepath.Join(dir, "findings.sarif")
		rr := doRequest(t, rt, http.MethodPost, "/api/export/save", map[string]string{"path": path, "content": `{"version":"2.1.0"}`})
		checkStatus(t, rr, http.StatusOK)
		got, err := os.ReadFile(path) // #nosec G304 -- test temp dir
		if err != nil || string(got) != `{"version":"2.1.0"}` {
			t.Fatalf("export not written: %q, %v", got, err)
		}
	})

	t.Run("local rejects a non-export file type", func(t *testing.T) {
		rt := newTestRouter(nil, false)
		path := filepath.Join(dir, "authorized_keys")
		rr := doRequest(t, rt, http.MethodPost, "/api/export/save", map[string]string{"path": path, "content": "x"})
		checkStatus(t, rr, http.StatusBadRequest)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("rejected export was written anyway (stat err = %v)", err)
		}
	})

	t.Run("cloud refuses", func(t *testing.T) {
		rt := newJWTTestRouter(t)
		bearer := jwtBearer(t, rt, "alice", "alice@example.com")
		path := filepath.Join(dir, "cloud.csv")
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/export/save", bearer, map[string]string{"path": path, "content": "x"})
		checkStatus(t, rr, http.StatusForbidden)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cloud mode wrote a server path (stat err = %v)", err)
		}
	})
}
