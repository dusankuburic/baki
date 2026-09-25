package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
)

func getJSON(t *testing.T, rt *Router, path, bearer string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	body := map[string]any{}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec, body
}

func TestHandleAnalytics_RequiresAuth(t *testing.T) {
	rt := newJWTTestRouter(t)
	rec, _ := getJSON(t, rt, "/api/dashboard/analytics", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAnalytics_BadQueryIs400(t *testing.T) {
	rt := newJWTTestRouter(t)
	tok := jwtBearer(t, rt, "ana-u1", "ana-u1@example.com")
	for _, q := range []string{"range=1y", "scope=org", "range=custom&from=2026-09-10&to=2026-09-01", "severity=critical"} {
		rec, _ := getJSON(t, rt, "/api/dashboard/analytics?"+q, tok)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", q, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleAnalytics_OK(t *testing.T) {
	rt := newJWTTestRouter(t)
	tok := jwtBearer(t, rt, "ana-u2", "ana-u2@example.com")
	rec, body := getJSON(t, rt, "/api/dashboard/analytics?range=7d&tz=Europe/Belgrade&compare=0", tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	rng, _ := body["range"].(map[string]any)
	if rng["preset"] != "7d" || rng["days"] != float64(7) {
		t.Errorf("range = %v", rng)
	}
	applied, _ := body["applied"].(map[string]any)
	if applied["scope"] != "all" || applied["tz"] != "Europe/Belgrade" || applied["compare"] != false {
		t.Errorf("applied = %v", applied)
	}
	if partial, ok := body["partial"].([]any); !ok || len(partial) != 0 {
		t.Errorf("partial = %v, want an empty list", body["partial"])
	}
	if _, ok := body["facets"].(map[string]any); !ok {
		t.Errorf("facets missing: %v", body)
	}
}

func TestHandleAnalytics_LocalModeIs501(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/dashboard/analytics", nil)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 in local mode: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleHome_SecurityAdminFlag: the locked-account count is instance-wide,
// so the home payload marks whether the caller may see it and zeroes it for
// everyone else.
func TestHandleHome_SecurityAdminFlag(t *testing.T) {
	rt := newJWTTestRouter(t)
	admin := jwtBearer(t, rt, "home-adm", "home-adm@example.com")
	rec, body := getJSON(t, rt, "/api/dashboard/home", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d", rec.Code)
	}
	sec, _ := body["security"].(map[string]any)
	if sec["admin"] != true {
		t.Errorf("admin payload security = %v, want admin:true", sec)
	}

	pair, err := rt.security.AuthMgr.Issue("home-mem", "home-mem@example.com", auth.RoleMember)
	if err != nil {
		t.Fatalf("issue member jwt: %v", err)
	}
	rec, body = getJSON(t, rt, "/api/dashboard/home", "Bearer "+pair.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("member status = %d", rec.Code)
	}
	sec, _ = body["security"].(map[string]any)
	if sec["admin"] != false || sec["lockedAccounts"] != float64(0) {
		t.Errorf("member payload security = %v, want admin:false and no locked-account count", sec)
	}
}
