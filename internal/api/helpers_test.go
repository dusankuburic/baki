package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/manager"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// newJWTTestRouter creates a Router in cloud/JWT mode.
func newJWTTestRouter(t *testing.T) *Router {
	t.Helper()
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	return NewRouter(manager.NewApp(fs), testToken, true, nil, "")
}

// jwtBearer issues a token for the given user and returns a ready Bearer header value.
func jwtBearer(t *testing.T, rt *Router, userID, email string) string {
	t.Helper()
	pair, err := rt.authMgr.Issue(userID, email, auth.RoleAdmin)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return "Bearer " + pair.AccessToken
}

// doRequest sends a request authenticated with the static test token.
func doRequest(t *testing.T, rt *Router, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithAuth(t, rt, method, path, "Bearer "+testToken, body)
}

// doRequestWithAuth sends a request with an explicit Authorization header.
// Pass an empty string to omit the header.
func doRequestWithAuth(t *testing.T, rt *Router, method, path, authHeader string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &b)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

// decodeJSON parses rr.Body into v.
func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rr.Body).Decode(v); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

// newLibraryTestRouter returns a JWT-mode Router backed by a temp filesystem store.
// The returned seed function inserts a flow document directly into the store.
func newLibraryTestRouter(t *testing.T) (*Router, func(id, ownerID string)) {
	t.Helper()
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("create local storage: %v", err)
	}
	app := manager.NewApp(fs)
	rt := NewRouter(app, testToken, true, nil, "")
	seed := func(id, ownerID string) {
		doc := &storageif.FlowDocument{
			ID:      id,
			Name:    "test",
			OwnerID: ownerID,
		}
		if err := fs.SaveFlow(context.Background(), doc); err != nil {
			t.Fatalf("seed flow %s: %v", id, err)
		}
	}
	return rt, seed
}

// checkStatus fails the test if rr.Code != want.
func checkStatus(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Errorf("expected status %d, got %d — body: %s", want, rr.Code, rr.Body.String())
	}
}

// badBody returns a request body that is not valid JSON.
var badBody = bytes.NewBufferString("not-json")

// newBadBodyRequest creates a request with an invalid JSON body.
func newBadBodyRequest(t *testing.T, rt *Router, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString("not-json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}
