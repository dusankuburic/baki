package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// headerOnlyBackend is a FakeBackend whose LoadFlow (the content read) always
// fails and is counted: a route that needs only the flow's name must get it
// from LoadFlowHeader and never touch the document.
type headerOnlyBackend struct {
	*testutil.FakeBackend
	loadFlowCalls atomic.Int64
}

func (b *headerOnlyBackend) LoadFlow(context.Context, string) (*storageif.FlowDocument, error) {
	b.loadFlowCalls.Add(1)
	return nil, errors.New("content read must not happen")
}

const sharedShellHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <title>PAD Analyzer</title>
</head>
<body><div id="root"></div></body>
</html>`

// newSharedShellRouter is a JWT-mode router over headerOnlyBackend serving a
// temp static dir with the SPA shell; it returns the shell's path too.
func newSharedShellRouter(t *testing.T) (*Router, *headerOnlyBackend, string) {
	t.Helper()
	backend := &headerOnlyBackend{FakeBackend: testutil.NewFakeBackend()}
	rt := newTestRouter(backend, true)
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.html")
	if err := os.WriteFile(indexPath, []byte(sharedShellHTML), 0600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	rt.staticDir = dir
	return rt, backend, indexPath
}

func seedHeaderShare(t *testing.T, backend *headerOnlyBackend, flowID, flowName, token string) {
	t.Helper()
	backend.Flows[flowID] = &storageif.FlowDocument{ID: flowID, Name: flowName, OwnerID: "alice", Content: []byte(`{}`)}
	hash := sha256.Sum256([]byte(token))
	if err := backend.CreateShareToken(context.Background(), &storageif.ShareToken{
		ID: "st-1", FlowID: flowID, TokenHash: hex.EncodeToString(hash[:]),
	}); err != nil {
		t.Fatalf("seed share token: %v", err)
	}
}

// TestServeSharedShell_HeaderOnly: the share viewer's shell takes the flow
// name from the header, never resolving the document (blob + unmarshal) for
// a <title> — on an unauthenticated route.
func TestServeSharedShell_HeaderOnly(t *testing.T) {
	rt, backend, _ := newSharedShellRouter(t)
	seedHeaderShare(t, backend, "flow-h1", "Invoices <Flow>", "tok-h1")

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/shared?token=tok-h1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "<title>Invoices &lt;Flow&gt; — PAD Analyzer</title>") {
		t.Fatalf("flow name not injected from the header:\n%s", rr.Body.String())
	}
	if n := backend.loadFlowCalls.Load(); n != 0 {
		t.Fatalf("LoadFlow (content) called %d times for the shell; want 0", n)
	}
}

// TestServeSharedShell_ShellReadOnce: index.html is a build artifact, read
// from disk once and served from memory after that.
func TestServeSharedShell_ShellReadOnce(t *testing.T) {
	rt, backend, indexPath := newSharedShellRouter(t)
	seedHeaderShare(t, backend, "flow-h2", "Cached", "tok-h2")

	first := httptest.NewRecorder()
	rt.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/shared?token=tok-h2", nil))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "<title>Cached — PAD Analyzer</title>") {
		t.Fatalf("first hit: status %d body %s", first.Code, first.Body.String())
	}
	// Rewrite the artifact: a per-hit read would serve the new bytes.
	if err := os.WriteFile(indexPath, []byte(strings.Replace(sharedShellHTML, "PAD Analyzer", "REWRITTEN", 1)), 0600); err != nil {
		t.Fatalf("rewrite index.html: %v", err)
	}
	second := httptest.NewRecorder()
	rt.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/shared?token=tok-h2", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second hit: status %d", second.Code)
	}
	if strings.Contains(second.Body.String(), "REWRITTEN") {
		t.Fatalf("second hit re-read index.html from disk:\n%s", second.Body.String())
	}
}

// TestRateLimitGroup_SharedShell: the share viewer's shell resolves a share
// and reads a flow header per hit, unauthenticated, so it rides the analysis
// bucket like the report it fronts.
func TestRateLimitGroup_SharedShell(t *testing.T) {
	for _, p := range []string{"/shared", "/shared?token=abc"} {
		u, err := url.Parse(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := rateLimitGroup(http.MethodGet, u); got != rlGroupAnalysis {
			t.Errorf("rateLimitGroup(GET %s) = %q, want %q", p, got, rlGroupAnalysis)
		}
	}
	// The SPA's other routes stay on general.
	if got := rateLimitGroup(http.MethodGet, &url.URL{Path: "/library"}); got != rlGroupGeneral {
		t.Errorf("rateLimitGroup(GET /library) = %q, want %q", got, rlGroupGeneral)
	}
}
