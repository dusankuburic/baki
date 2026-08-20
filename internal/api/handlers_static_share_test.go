package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// staticShareRouter builds a JWT-mode router with a temp static dir containing
// the SPA shell, mirroring a production cloud deploy.
func staticShareRouter(t *testing.T) *Router {
	t.Helper()
	rt := newJWTTestRouter(t)
	dir := t.TempDir()
	shell := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta name="theme-color" content="#0a0a0b"/>
    <title>PAD Analyzer</title>
</head>
<body>
<div id="root"></div>
</body>
</html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(shell), 0600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	rt.staticDir = dir
	return rt
}

// seedShare stores a cloud flow whose parsed Content carries flowName, plus a
// share token binding it to token.
func seedShare(t *testing.T, rt *Router, flowID, flowName, token string) {
	t.Helper()
	content, err := json.Marshal(&models.FlowDocument{ID: flowID, Name: flowName})
	if err != nil {
		t.Fatalf("marshal flow content: %v", err)
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID:      flowID,
		Name:    flowID,
		OwnerID: "alice",
		Content: content,
		Source:  "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'hi'\n#EndRegion\n",
	}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	hash := sha256.Sum256([]byte(token))
	if err := rt.security.Backend.CreateShareToken(context.Background(), &storageif.ShareToken{
		ID:        "st-1",
		FlowID:    flowID,
		TokenHash: hex.EncodeToString(hash[:]),
	}); err != nil {
		t.Fatalf("seed share token: %v", err)
	}
}

func TestServeSharedShell_InjectsFlowName(t *testing.T) {
	rt := staticShareRouter(t)
	seedShare(t, rt, "flow-shared-1", "Invoices <Flow>", "tok123")

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/shared?token=tok123", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "<title>Invoices &lt;Flow&gt; — PAD Analyzer</title>") {
		t.Errorf("expected injected escaped title, got:\n%s", body)
	}
	if !strings.Contains(body, `property="og:title" content="Invoices &lt;Flow&gt;"`) {
		t.Errorf("expected injected og:title meta, got:\n%s", body)
	}
	if !strings.Contains(body, `name="twitter:card" content="summary"`) {
		t.Errorf("expected injected twitter:card meta, got:\n%s", body)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("expected no-cache on the injected shell, got %q", got)
	}
}

func TestServeSharedShell_BadTokenServesPlainShell(t *testing.T) {
	rt := staticShareRouter(t)
	seedShare(t, rt, "flow-shared-2", "Named Flow", "tok123")

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/shared?token=wrong", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "<title>PAD Analyzer</title>") {
		t.Errorf("expected plain shell title, got:\n%s", body)
	}
	if strings.Contains(body, "og:title") {
		t.Error("unexpected injected meta for invalid token")
	}
}

func TestServeSharedShell_NoTokenServesPlainShell(t *testing.T) {
	rt := staticShareRouter(t)

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/shared", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<title>PAD Analyzer</title>") {
		t.Errorf("expected plain shell title, got:\n%s", rr.Body.String())
	}
}
