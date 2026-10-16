package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/platform/hmacsig"
	secretv "pad-analyzer/internal/platform/secret"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

// signBody computes the legacy X-Baki-Signature (no timestamp, body-only).
func signBody(secret string, body []byte) string {
	return hmacsig.SignUntimed([]byte(secret), body)
}

// signBodyWithTimestamp computes the X-Baki-Signature including a timestamp in the signed payload (the
// replay-protected form); the timestamp must also be sent in X-Baki-Timestamp.
func signBodyWithTimestamp(secret string, body []byte, ts int64) string {
	return hmacsig.Sign([]byte(secret), ts, body)
}

// setCIGlobalSecret configures the deployment-wide PAD_CI_WEBHOOK_SECRET on rt's webhook, keeping what the
// router's deployment offers (organisation credentials with accounts).
func setCIGlobalSecret(t testing.TB, rt *Router, secret string) {
	t.Helper()
	h := moduleOf[*AnalysisHandler](t, rt)
	h.ci = service.NewCIService(h.backend, h.flowSvc.DocProvider(), service.NewAuthzService(h.backend, rt.security.OrgSvc), service.CIOptions{GlobalSecret: secretv.New(secret), OrgCredentials: rt.caps.Has(capability.Accounts)})
}

// ciCall is a webhook request for body signed with key; keyID goes in X-Baki-Key-Id when set.
func ciCall(t *testing.T, key, keyID string, body map[string]string) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(raw))
	req.Header.Set(hmacsig.SignatureHeader, hmacsig.Sign([]byte(key), ts, raw))
	req.Header.Set(hmacsig.TimestampHeader, strconv.FormatInt(ts, 10))
	if keyID != "" {
		req.Header.Set(hmacsig.KeyIDHeader, keyID)
	}
	return req
}

func serveCI(rt *Router, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

// Without accounts and without PAD_CI_WEBHOOK_SECRET nothing can authenticate a call: 503.
func TestCIWebhook_Unconfigured_Returns503(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), false)
	rr := serveCI(rt, httptest.NewRequest(http.MethodPost, "/api/integrations/ci", strings.NewReader(`{"flowId":"x"}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when CI webhook unconfigured, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// The desktop has no server store: the route itself answers 503 (route.Needs), even with a secret configured.
func TestCIWebhook_Desktop_Returns503(t *testing.T) {
	rt := newTestRouter(nil, false)
	setCIGlobalSecret(t, rt, "topsecret")
	rr := serveCI(rt, ciCall(t, "topsecret", "", map[string]string{"flowId": "x"}))
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "SERVICE_UNAVAILABLE") {
		t.Errorf("desktop: %d %s, want 503", rr.Code, rr.Body.String())
	}
}

// recordingHandler is a slog.Handler that keeps every record so a test can
// assert on levels without parsing rendered output.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

// TestCIWebhook_Unconfigured_NoErrorLogFlood: the endpoint is public, and
// with PAD_CI_WEBHOOK_SECRET unset render.Error logged every 503 at ERROR —
// any unauthenticated caller could flood the error log by repeating the
// request. Every call must still get the byte-identical 503 envelope it got
// before, with no ERROR record and a single WARN for the process.
func TestCIWebhook_Unconfigured_NoErrorLogFlood(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), false)
	// The WARN is once per process; start from a fresh gate so the count
	// below does not depend on which test reached the endpoint first.
	ciWebhookUnconfiguredWarn = sync.Once{}

	logs := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// The response the endpoint has always sent: 503, the status-derived
	// code and the 5xx-masked message (no requestId — the test router has no
	// access-log layer to stamp X-Request-ID).
	const wantBody = `{"code":"SERVICE_UNAVAILABLE","message":"internal server error"}` + "\n"
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader([]byte(`{"flowId":"x"}`)))
		rr := serveCI(rt, req)
		if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != wantBody || rr.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("call %d: got %d %q (Content-Type %q), want 503 %q (application/json)",
				i, rr.Code, rr.Body.String(), rr.Header().Get("Content-Type"), wantBody)
		}
	}
	slog.SetDefault(prev)

	var warns int
	for _, r := range logs.snapshot() {
		switch {
		case r.Level >= slog.LevelError:
			t.Errorf("ERROR record logged for the unconfigured CI webhook: %q", r.Message)
		case r.Level == slog.LevelWarn && strings.Contains(r.Message, "PAD_CI_WEBHOOK_SECRET"):
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("misconfiguration WARN logged %d times over 50 calls, want exactly once", warns)
	}
}

// With accounts the webhook is always configured (organisation credentials): a call nothing verifies is a 401.
func TestCIWebhook_WithAccounts_UnsignedIs401(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := serveCI(rt, httptest.NewRequest(http.MethodPost, "/api/integrations/ci", strings.NewReader(`{"flowId":"x"}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("got %d (%s), want 401", rr.Code, rr.Body.String())
	}
}

func TestCIWebhook_MissingSignature_Returns401(t *testing.T) {
	rt := newJWTTestRouter(t)
	setCIGlobalSecret(t, rt, "topsecret")

	body, _ := json.Marshal(map[string]string{"flowId": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(body))
	if rr := serveCI(rt, req); rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing signature, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestCIWebhook_InvalidSignature_Returns401(t *testing.T) {
	rt := newJWTTestRouter(t)
	setCIGlobalSecret(t, rt, "topsecret")

	body, _ := json.Marshal(map[string]string{"flowId": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(body))
	req.Header.Set("X-Baki-Signature", "sha256=deadbeef")
	req.Header.Set("X-Baki-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	if rr := serveCI(rt, req); rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid signature, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestCIWebhook_ValidSignature_MissingFlowID_Returns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	setCIGlobalSecret(t, rt, "topsecret")
	if rr := serveCI(rt, ciCall(t, "topsecret", "", map[string]string{})); rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing flowId, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// An unknown format is refused instead of falling back to the summary, with a code a pipeline can switch on and the
// formats it may send; bakicli gates every value but sarif, so no release of it sends one expecting a verdict.
func TestCIWebhook_UnknownFormat_Returns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	setCIGlobalSecret(t, rt, "topsecret")
	seedCloudFlowWithSource(t, rt, "flow-ci-format", "alice", rawSampleFlow)
	for _, format := range []string{"json", "junit", "Summary2"} {
		rr := serveCI(rt, ciCall(t, "topsecret", "", map[string]string{"flowId": "flow-ci-format", "format": format}))
		var env struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Formats []string `json:"formats"`
			} `json:"details"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		if rr.Code != http.StatusBadRequest || env.Code != "UNKNOWN_FORMAT" || !strings.Contains(env.Message, "unknown format") ||
			strings.Join(env.Details.Formats, ",") != "summary,report,sarif" {
			t.Errorf("format %q: %d %s, want 400 UNKNOWN_FORMAT listing summary, report and sarif", format, rr.Code, rr.Body.String())
		}
	}
	for _, format := range []string{"", "SUMMARY", " report "} {
		if rr := serveCI(rt, ciCall(t, "topsecret", "", map[string]string{"flowId": "flow-ci-format", "format": format})); rr.Code != http.StatusOK {
			t.Errorf("format %q: %d %s, want 200", format, rr.Code, rr.Body.String())
		}
	}
}

// B1.9: a legacy (timestamp-less) signature is refused — replay protection.
func TestCIWebhook_LegacyNoTimestamp_Refused(t *testing.T) {
	rt := newJWTTestRouter(t)
	setCIGlobalSecret(t, rt, "topsecret")

	body, _ := json.Marshal(map[string]string{"flowId": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(body))
	req.Header.Set("X-Baki-Signature", signBody("topsecret", body))
	rr := serveCI(rt, req)
	if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "X-Baki-Timestamp") {
		t.Errorf("expected 401 naming the missing timestamp, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// ciOrgFixture is a cloud router with an organisation (mirrored into the store, as on Postgres), its admin, a flow
// of it, another organisation's flow and a personal flow.
type ciOrgFixture struct {
	rt                     *Router
	store                  *testutil.FakeBackend
	org, other, admin      string
	orgFlow, otherFlow     string
	personalFlow, adminJWT string
}

func newCIOrgFixture(t *testing.T) *ciOrgFixture {
	t.Helper()
	store := testutil.NewFakeBackend()
	rt := newTestRouter(store, true)
	f := &ciOrgFixture{rt: rt, store: store, admin: "u-ci-admin", orgFlow: "flow-ci-org", otherFlow: "flow-ci-other", personalFlow: "flow-ci-personal"}
	seedUserWithRole(t, rt, f.admin, "ci-admin@acme.io", auth.RoleMember)
	f.org = seedOrg(t, rt, "Acme", f.admin)
	f.other = seedOrg(t, rt, "Other", "u-other")
	mirrorOrg(t, rt, f.org)
	mirrorOrg(t, rt, f.other)
	for id, org := range map[string]string{f.orgFlow: f.org, f.otherFlow: f.other, f.personalFlow: ""} {
		doc, err := parser.ParseText(rawSampleFlow, id+".txt", int64(len(rawSampleFlow)))
		if err != nil {
			t.Fatal(err)
		}
		content, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveFlow(context.Background(), &storageif.FlowDocument{ID: id, Name: id, OwnerID: f.admin, OrganizationID: org,
			Content: content, Source: rawSampleFlow}); err != nil {
			t.Fatal(err)
		}
	}
	f.adminJWT = sessionBearer(t, rt, f.admin, auth.RoleMember)
	return f
}

// mint creates a credential through the API as the org admin.
func (f *ciOrgFixture) mint(t *testing.T, org, name string) ciCredentialCreated {
	t.Helper()
	rr := doRequestWithAuth(t, f.rt, http.MethodPost, "/api/orgs/"+org+"/ci-credentials", f.adminJWT, map[string]any{"name": name})
	checkStatus(t, rr, http.StatusCreated)
	var out ciCredentialCreated
	decodeJSON(t, rr, &out)
	if out.Credential == "" || out.ID == "" {
		t.Fatalf("create answer = %+v", out)
	}
	return out
}

func TestCIWebhook_OrgCredential(t *testing.T) {
	f := newCIOrgFixture(t)
	cred := f.mint(t, f.org, "pipeline")
	body := map[string]string{"flowId": f.orgFlow, "format": "summary"}

	// New bakicli: the key id in X-Baki-Key-Id.
	rr := serveCI(f.rt, ciCall(t, cred.Credential, cred.ID, body))
	checkStatus(t, rr, http.StatusOK)
	var v ciWebhookResponse
	decodeJSON(t, rr, &v)
	if v.FlowID != f.orgFlow || v.Gate != "error" {
		t.Errorf("verdict = %+v", v)
	}
	if rr.Header().Get("Deprecation") != "" {
		t.Error("an org credential's answer is marked deprecated")
	}
	// Old bakicli: no key id; the credential is found through the flow's organisation.
	checkStatus(t, serveCI(f.rt, ciCall(t, cred.Credential, "", body)), http.StatusOK)

	// The credential reaches its organisation's flows only: after authentication, the same 404 as an unknown flow.
	for _, flow := range []string{f.otherFlow, f.personalFlow, "flow-missing"} {
		rr := serveCI(f.rt, ciCall(t, cred.Credential, cred.ID, map[string]string{"flowId": flow}))
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "flow not found") {
			t.Errorf("%s through another organisation's credential: %d %s, want the static 404", flow, rr.Code, rr.Body.String())
		}
	}
	// Without a key id, another organisation's flow cannot even pick the credential: 401.
	if rr := serveCI(f.rt, ciCall(t, cred.Credential, "", map[string]string{"flowId": f.otherFlow})); rr.Code != http.StatusUnauthorized {
		t.Errorf("fallback onto another organisation's flow: %d, want 401", rr.Code)
	}

	// A second credential works alongside; revoking the first leaves the second.
	second := f.mint(t, f.org, "second")
	checkStatus(t, doRequestWithAuth(t, f.rt, http.MethodDelete, "/api/orgs/"+f.org+"/ci-credentials/"+cred.ID, f.adminJWT, nil), http.StatusNoContent)
	for _, keyID := range []string{cred.ID, ""} {
		if rr := serveCI(f.rt, ciCall(t, cred.Credential, keyID, body)); rr.Code != http.StatusUnauthorized {
			t.Errorf("revoked credential (key id %q): %d, want 401", keyID, rr.Code)
		}
	}
	checkStatus(t, serveCI(f.rt, ciCall(t, second.Credential, second.ID, body)), http.StatusOK)
	checkStatus(t, serveCI(f.rt, ciCall(t, second.Credential, "", body)), http.StatusOK)
}

// Every pre-authentication failure is the same 401, whatever was wrong.
func TestCIWebhook_PreAuthFailuresAreOne401(t *testing.T) {
	f := newCIOrgFixture(t)
	cred := f.mint(t, f.org, "pipeline")
	expired := f.mint(t, f.org, "expired")
	c, err := f.store.GetCICredential(context.Background(), expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	c.ExpiresAt = &past
	if err := f.store.DeleteCICredential(context.Background(), f.org, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateCICredential(context.Background(), c, service.MaxActiveCICredentials); err != nil {
		t.Fatal(err)
	}

	stale := ciCall(t, cred.Credential, cred.ID, map[string]string{"flowId": f.orgFlow})
	stale.Header.Set(hmacsig.TimestampHeader, strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))
	cases := map[string]*http.Request{
		"unknown key id":       ciCall(t, cred.Credential, "aaaaaaaaaaaaaaaa", map[string]string{"flowId": f.orgFlow}),
		"bad signature":        ciCall(t, cred.Credential+"x", cred.ID, map[string]string{"flowId": f.orgFlow}),
		"expired by key id":    ciCall(t, expired.Credential, expired.ID, map[string]string{"flowId": f.orgFlow}),
		"expired via the flow": ciCall(t, expired.Credential, "", map[string]string{"flowId": f.orgFlow}),
		"unknown flow":         ciCall(t, cred.Credential, "", map[string]string{"flowId": "flow-missing"}),
		"personal flow":        ciCall(t, cred.Credential, "", map[string]string{"flowId": f.personalFlow}),
		"stale timestamp":      stale,
	}
	var want string
	for name, req := range cases {
		rr := serveCI(f.rt, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s, want 401", name, rr.Code, rr.Body.String())
			continue
		}
		if want == "" {
			want = rr.Body.String()
		} else if rr.Body.String() != want {
			t.Errorf("%s answers %q, another failure %q: the failures must not be told apart", name, rr.Body.String(), want)
		}
	}
}

// The deployment secret keeps working where organisations have credentials, marked deprecated; without accounts it
// is the only key and is not.
func TestCIWebhook_GlobalSecretDeprecation(t *testing.T) {
	f := newCIOrgFixture(t)
	setCIGlobalSecret(t, f.rt, "topsecret")
	rr := serveCI(f.rt, ciCall(t, "topsecret", "", map[string]string{"flowId": f.otherFlow}))
	checkStatus(t, rr, http.StatusOK)
	if rr.Header().Get("Deprecation") != "true" || !strings.Contains(rr.Header().Get("Link"), `rel="deprecation"`) {
		t.Errorf("deprecation headers = %q / %q", rr.Header().Get("Deprecation"), rr.Header().Get("Link"))
	}

	local := newTestRouter(testutil.NewFakeBackend(), false)
	setCIGlobalSecret(t, local, "topsecret")
	seedCloudFlowWithSource(t, local, "flow-local", "local", rawSampleFlow)
	rr = serveCI(local, ciCall(t, "topsecret", "", map[string]string{"flowId": "flow-local"}))
	checkStatus(t, rr, http.StatusOK)
	if rr.Header().Get("Deprecation") != "" {
		t.Error("the single-tenant secret is marked deprecated")
	}
}

// local+db resolves the flow the call names from the store; it used to analyse whatever the process had open.
func TestCIWebhook_LocalDB_ResolvesByID(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), false)
	setCIGlobalSecret(t, rt, "topsecret")
	seedCloudFlowWithSource(t, rt, "flow-stored", "local", rawSampleFlow)
	moduleOf[*AnalysisHandler](t, rt).flowSvc.DocProvider().SetCurrentDoc(&models.FlowDocument{ID: "open", Name: "Open flow"})

	rr := serveCI(rt, ciCall(t, "topsecret", "", map[string]string{"flowId": "flow-stored"}))
	checkStatus(t, rr, http.StatusOK)
	var v ciWebhookResponse
	decodeJSON(t, rr, &v)
	if v.FlowID != "flow-stored" || v.FlowName != "flow-stored" {
		t.Errorf("verdict = %+v; want the stored flow, not the open one", v)
	}
	if rr := serveCI(rt, ciCall(t, "topsecret", "", map[string]string{"flowId": "open"})); rr.Code != http.StatusNotFound {
		t.Errorf("the open document is no stored flow: %d", rr.Code)
	}
}

// Each call is audited as the integration (never a user); the dashboard is recorded once per report, detached, not
// on every poll of an unchanged flow; the credential's use is recorded off the response path.
func TestCIWebhook_AuditRecordAndTouch(t *testing.T) {
	f := newCIOrgFixture(t)
	cred := f.mint(t, f.org, "pipeline")
	h := moduleOf[*AnalysisHandler](t, f.rt)
	baseline := len(f.store.SavedAuditEvents())

	for range 2 {
		checkStatus(t, serveCI(f.rt, ciCall(t, cred.Credential, cred.ID, map[string]string{"flowId": f.orgFlow, "failOn": "none"})), http.StatusOK)
	}
	if err := h.records.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var runs []*storageif.AuditEvent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs = runs[:0]
		for _, ev := range f.store.SavedAuditEvents()[baseline:] {
			if ev.Action == "ci.webhook_run" {
				runs = append(runs, ev)
			}
		}
		if len(runs) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(runs) != 2 {
		t.Fatalf("%d ci.webhook_run rows for 2 calls", len(runs))
	}
	for _, ev := range runs {
		if ev.ActorKind != "integration" || ev.TokenID != cred.ID || ev.UserID != "" || ev.OrgID != f.org || ev.ResourceID != f.orgFlow ||
			ev.Meta["method"] != "key_id" || ev.Meta["gate"] != "none" || ev.Meta["passed"] != "true" || ev.Meta["format"] != "summary" {
			t.Errorf("audit row = %+v", ev)
		}
	}
	if snap := f.store.FlowHealth[f.orgFlow]; snap == nil {
		t.Error("the CI run was not recorded on the dashboard")
	}
	for time.Now().Before(deadline) {
		if c, _ := f.store.GetCICredential(context.Background(), cred.ID); c.LastUsedAt != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the credential's use was never recorded")
}

func TestGatePasses(t *testing.T) {
	counts := func(e, w, i int) analyzer.GateCounts {
		return analyzer.GateCounts{Errors: e, Warnings: w, Info: i}
	}
	cases := []struct {
		name   string
		counts analyzer.GateCounts
		gate   string
		want   bool
	}{
		{"none always passes", counts(5, 5, 5), "none", true},
		{"error gate: zero errors passes", counts(0, 5, 5), "error", true},
		{"error gate: errors fail", counts(1, 0, 0), "error", false},
		{"warning gate: warnings fail", counts(0, 1, 0), "warning", false},
		{"warning gate: errors also fail", counts(1, 0, 0), "warning", false},
		{"info gate: info fails", counts(0, 0, 1), "info", false},
		{"info gate: clean passes", counts(0, 0, 0), "info", true},
		{"unknown gate defaults to error behavior", counts(1, 0, 0), "bogus", false},
		{"empty gate is the error gate", counts(1, 0, 0), "", false},
		{"preview findings are not counted", analyzer.GateCounts{Preview: 3}, "info", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gatePasses(c.counts, c.gate); got != c.want {
				t.Errorf("gatePasses(gate=%q) = %v, want %v", c.gate, got, c.want)
			}
		})
	}
}

// The webhook gates on analyzer.CountGating, so a preview rule's error neither fails the gate nor
// leaves the severity counts: stamped "preview", or unstamped from a rule that is a preview built-in
// (an older report). A stable rule's error still fails, an unknown stamp included (fail closed).
func TestGatePasses_PreviewFindingsNeverGate(t *testing.T) {
	m, _ := analyzer.LookupMeta("insecure-http-url")
	m.Stage = analyzer.StagePreview
	defer analyzer.SetRuleMetaForTesting("insecure-http-url", m)()

	findings := []models.Finding{
		{RuleID: "hardcoded-credential", Severity: models.SeverityError, Stage: "preview"},
		{RuleID: "insecure-http-url", Severity: models.SeverityError},
	}
	c := analyzer.CountGating(findings)
	if !gatePasses(c, "info") || c.Preview != 2 {
		t.Errorf("preview errors gated: counts %+v", c)
	}
	findings = append(findings, models.Finding{RuleID: "hardcoded-credential", Severity: models.SeverityError, Stage: "beta"})
	if c := analyzer.CountGating(findings); gatePasses(c, "error") {
		t.Errorf("a stable rule's error with an unknown stamp passed the gate: counts %+v", c)
	}
}
