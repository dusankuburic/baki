package middleware

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/reqinfo"
)

// withPolicy returns r carrying p as its resolved route policy, as the router's outer layer records it.
func withPolicy(r *http.Request, p *route.Policy) *http.Request {
	return r.WithContext(reqinfo.With(r.Context(), &reqinfo.Info{Policy: p}))
}

func TestNormalizeMethod(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE"} {
		if got := normalizeMethod(m); got != m {
			t.Errorf("normalizeMethod(%q) = %q, want it verbatim", m, got)
		}
	}
	for _, m := range []string{"FOO", "get", "PROPFIND", "", strings.Repeat("X", 4096)} {
		if got := normalizeMethod(m); got != methodOther {
			t.Errorf("normalizeMethod(%q) = %q, want %q", m, got, methodOther)
		}
	}
}

// TestMetrics_CustomMethodsAreBounded is the regression test for the method
// half of the cardinality bomb: the route label was bounded, but r.Method was
// labelled verbatim, so `LEAK1 /`, `LEAK2 /`, ... each minted a permanent
// series. Drive the real middleware and read the real exposition.
func TestMetrics_CustomMethodsAreBounded(t *testing.T) {
	h := Metrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	root := &route.Policy{Kind: route.KindNoRoute, Label: "/"}
	for i := 0; i < 20; i++ {
		req := withPolicy(httptest.NewRequest("GET", "/", nil), root)
		req.Method = fmt.Sprintf("CARDTEST%d", i)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	rr := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if strings.Contains(body, `method="CARDTEST`) {
		t.Error("a custom request method reached the method label verbatim")
	}
	if !strings.Contains(body, `method="OTHER",route="/",status="404"`) {
		t.Error(`custom methods were not recorded under method="OTHER"`)
	}
}

// TestMetrics_LabelsTheResolvedPolicy: the route label is the label of the policy the outer layer resolved, taken
// before the handler runs. It does not depend on the path's spelling or on whether the router got to rewrite the
// URL: a request refused before the router (a 429) used to be labelled on its raw path, /api/v1/library/x as
// /api/other. A request no resolver has seen is "unresolved", never its path.
func TestMetrics_LabelsTheResolvedPolicy(t *testing.T) {
	library := &route.Policy{Kind: route.KindEndpoint, Method: http.MethodGet, Pattern: "/api/library/{id}", Label: "/api/library/{id}"}
	serve := func(r *http.Request, status int, rewrite bool) {
		Metrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rewrite {
				r.URL.Path = "/rewritten/canon-elsewhere"
			}
			w.WriteHeader(status)
		})).ServeHTTP(httptest.NewRecorder(), r)
	}
	serve(withPolicy(httptest.NewRequest(http.MethodGet, "/api/v1/library/canon-abc", nil), library), http.StatusTooManyRequests, false)
	serve(withPolicy(httptest.NewRequest(http.MethodGet, "/api/library/canon-def", nil), library), http.StatusTeapot, true)
	for i := 0; i < 20; i++ {
		serve(httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/canon-%d", i), nil), http.StatusGone, false)
	}

	rr := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`method="GET",route="/api/library/{id}",status="429"`,
		`method="GET",route="/api/library/{id}",status="418"`,
		`method="GET",route="unresolved",status="410"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no series %s", want)
		}
	}
	if strings.Contains(body, "canon-") {
		t.Error("a raw path segment reached the route label")
	}
}

func TestRequestPolicy_FallsBackToUnresolved(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	if p, ok := RequestPolicy(r); ok || p != route.Unresolved {
		t.Errorf("no Info: RequestPolicy = %p, %v; want route.Unresolved, false", p, ok)
	}
	r = r.WithContext(reqinfo.With(r.Context(), &reqinfo.Info{}))
	if p, ok := RequestPolicy(r); ok || p != route.Unresolved {
		t.Errorf("Info without a policy: RequestPolicy = %p, %v; want route.Unresolved, false", p, ok)
	}
	want := &route.Policy{Label: "/api/x"}
	if p, ok := RequestPolicy(withPolicy(r, want)); !ok || p != want {
		t.Errorf("RequestPolicy = %p, %v; want the recorded policy", p, ok)
	}
	if u := route.Unresolved; u.Access != route.AuthRequired || !u.TokensDenied || u.Tx != route.TxRequest || u.Deadline != route.DeadlineDefault {
		t.Errorf("route.Unresolved must fail closed: %+v", u)
	}
}

// TestReportPolicyMismatch_CountsEveryTimeLogsOnce: shadow mode counts each disagreement, and logs each (route,
// decision) pair once per process, so a hot route cannot flood the log.
func TestReportPolicyMismatch_CountsEveryTimeLogsOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := &route.Policy{Kind: route.KindEndpoint, Label: "/api/mismatch-probe/{id}"}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/mismatch-probe/x", nil)
	for i := 0; i < 3; i++ {
		ReportPolicyMismatch(r, p, DecisionTx, "tx", "none")
	}
	ReportPolicyMismatch(r, p, DecisionAuth, "jwt", "none")
	if got := metrics.RoutePolicyMismatchCount(p.Label, DecisionTx); got != 3 {
		t.Errorf("tx mismatches counted %v, want 3", got)
	}
	if got := metrics.RoutePolicyMismatchCount(p.Label, DecisionAuth); got != 1 {
		t.Errorf("auth mismatches counted %v, want 1", got)
	}
	logged := buf.String()
	if n := strings.Count(logged, "decision=tx"); n != 1 {
		t.Errorf("tx mismatch logged %d times, want once:\n%s", n, logged)
	}
	if n := strings.Count(logged, "decision=auth"); n != 1 {
		t.Errorf("auth mismatch logged %d times, want once:\n%s", n, logged)
	}
	if !strings.Contains(logged, "path=/api/mismatch-probe/x") || !strings.Contains(logged, "legacy=tx") || !strings.Contains(logged, "declared=none") {
		t.Errorf("the log line lacks the canonical path or the two decisions:\n%s", logged)
	}
}

// TestReportGuardMismatch_RedactsInvites: the guard logs the path of the mismatched request, which for an invite
// accept is the emailed single-use credential.
func TestReportGuardMismatch_RedactsInvites(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	resolved := &route.Policy{Kind: route.KindNoRoute, Label: "/api/other"}
	ReportGuardMismatch(httptest.NewRequest(http.MethodPost, "/api/invites/s3cret-invite/accept", nil), resolved, "/api/invites/{token}/accept")
	if strings.Contains(buf.String(), "s3cret-invite") {
		t.Errorf("the invite credential reached the log:\n%s", buf.String())
	}
	if got := metrics.RouteGuardMismatchCount("/api/invites/{token}/accept"); got != 1 {
		t.Errorf("guard mismatches counted %v, want 1", got)
	}
}
