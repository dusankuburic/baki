package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/parser"
)

// ciTokenFixture: alice owns a personal flow and an organisation with a flow; bob owns a personal flow and another
// organisation's flow. Alice's tokens are minted through the API.
type ciTokenFixture struct {
	rt                                     *Router
	store                                  *testutil.FakeBackend
	aliceFlow, acmeFlow, bobFlow, bobOrgFl string
	aliceSession                           string
}

func newCITokenFixture(t *testing.T) *ciTokenFixture {
	t.Helper()
	store := testutil.NewFakeBackend()
	rt := newTestRouter(store, true)
	f := &ciTokenFixture{rt: rt, store: store, aliceFlow: "flow-alice", acmeFlow: "flow-acme", bobFlow: "flow-bob", bobOrgFl: "flow-bob-org"}
	seedUserWithRole(t, rt, "u-alice", "alice@acme.io", auth.RoleMember)
	seedUserWithRole(t, rt, "u-bob", "bob@example.com", auth.RoleMember)
	acme, bobs := seedOrg(t, rt, "Acme", "u-alice"), seedOrg(t, rt, "Bob's", "u-bob")
	mirrorOrg(t, rt, acme)
	mirrorOrg(t, rt, bobs)
	for id, owner := range map[string][2]string{
		f.aliceFlow: {"u-alice", ""}, f.acmeFlow: {"u-alice", acme},
		f.bobFlow: {"u-bob", ""}, f.bobOrgFl: {"u-bob", bobs}, "flow-bob-shared": {"u-bob", ""},
	} {
		doc, err := parser.ParseText(rawSampleFlow, id+".txt", int64(len(rawSampleFlow)))
		if err != nil {
			t.Fatal(err)
		}
		content, _ := json.Marshal(doc)
		if err := store.SaveFlow(context.Background(), &storageif.FlowDocument{ID: id, Name: id, OwnerID: owner[0], OrganizationID: owner[1],
			Content: content, Source: rawSampleFlow}); err != nil {
			t.Fatal(err)
		}
	}
	f.aliceSession = sessionBearer(t, rt, "u-alice", auth.RoleMember)
	return f
}

// token mints one of alice's API tokens with scopes; it returns the token's id and its bearer header.
func (f *ciTokenFixture) token(t *testing.T, scopes ...string) (id, bearer string) {
	t.Helper()
	rr := doRequestWithAuth(t, f.rt, http.MethodPost, "/api/auth/tokens", f.aliceSession, map[string]any{"name": "ci", "scopes": scopes})
	checkStatus(t, rr, http.StatusOK)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decodeJSON(t, rr, &created)
	if created.Token == "" {
		t.Fatalf("mint %v: %s", scopes, rr.Body.String())
	}
	return created.ID, "Bearer " + created.Token
}

// tokenCI is a webhook call authenticated by authorization alone: no signature headers.
func tokenCI(t *testing.T, authorization string, body map[string]string) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(raw))
	req.Header.Set("Authorization", authorization)
	return req
}

func ciAuthCount(t *testing.T, method string) float64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range fams {
		if mf.GetName() != "pad_ci_webhook_auth_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "method" && lp.GetValue() == method {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// A personal access token with the ci scope runs the webhook as its user, on the flows the user can read: their
// own and their organisations'. Another user's flow, personal or not, is the same 404 as an unknown one.
func TestCIWebhook_PersonalToken(t *testing.T) {
	f := newCITokenFixture(t)
	tokID, ci := f.token(t, auth.ScopeCI)
	runs := captureAuditEvents(t, 8)
	before := ciAuthCount(t, "token")

	rr := serveCI(f.rt, tokenCI(t, ci, map[string]string{"flowId": f.aliceFlow, "failOn": "none"}))
	checkStatus(t, rr, http.StatusOK)
	var v ciWebhookResponse
	decodeJSON(t, rr, &v)
	if v.FlowID != f.aliceFlow || !v.Passed || v.Gate != "none" {
		t.Errorf("verdict = %+v", v)
	}
	if rr.Header().Get("Deprecation") != "" {
		t.Error("a token's answer is marked deprecated")
	}
	ev := nextAuditEvent(t, runs)
	if ev.Action != "ci.webhook_run" || ev.ActorKind != "token" || ev.UserID != "u-alice" || ev.TokenID != tokID ||
		ev.ResourceID != f.aliceFlow || ev.OrgID != "" || ev.Meta["method"] != "token" || ev.Meta["passed"] != "true" {
		t.Errorf("audit row = %+v", ev)
	}
	if got := ciAuthCount(t, "token") - before; got != 1 {
		t.Errorf("pad_ci_webhook_auth_total{method=token} grew by %v, want 1", got)
	}
	checkStatus(t, serveCI(f.rt, tokenCI(t, ci, map[string]string{"flowId": f.acmeFlow})), http.StatusOK)
	// Shared with alice: bob's personal flow is hers to run; his organisation's flow is not, as she is no member of it.
	for _, flow := range []string{"flow-bob-shared", f.bobOrgFl} {
		if err := f.store.AddCollaborator(context.Background(), flow, &storageif.Collaborator{UserID: "u-alice", Permission: "viewer"}); err != nil {
			t.Fatal(err)
		}
	}
	checkStatus(t, serveCI(f.rt, tokenCI(t, ci, map[string]string{"flowId": "flow-bob-shared"})), http.StatusOK)

	var notFound string
	for _, flow := range []string{"flow-missing", f.bobFlow, f.bobOrgFl} {
		rr := serveCI(f.rt, tokenCI(t, ci, map[string]string{"flowId": flow}))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s, want 404", flow, rr.Code, rr.Body.String())
			continue
		}
		if notFound == "" {
			notFound = rr.Body.String()
		} else if rr.Body.String() != notFound {
			t.Errorf("%s answers %q, an unknown flow %q: another user's flow must look unknown", flow, rr.Body.String(), notFound)
		}
	}
}

// Least privilege: only a token carrying the ci scope reaches the webhook. An unscoped token, a token without ci, a
// session, an unknown, revoked or expired token all get the webhook's one 401; a signature beside a refused bearer
// does not rescue it.
func TestCIWebhook_TokenWithoutCIScopeIs401(t *testing.T) {
	f := newCITokenFixture(t)
	_, unscoped := f.token(t)
	_, rw := f.token(t, auth.ScopeRead, auth.ScopeWrite)
	expiredID, expired := f.token(t, auth.ScopeCI)
	revokedID, revoked := f.token(t, auth.ScopeCI)
	checkStatus(t, doRequestWithAuth(t, f.rt, http.MethodDelete, "/api/auth/tokens/"+revokedID, f.aliceSession, nil), http.StatusOK)
	past := time.Now().Add(-time.Minute)
	f.store.APITokens[expiredID].ExpiresAt = &past

	cases := map[string]string{
		"unscoped token":   unscoped,
		"read+write token": rw,
		"session":          f.aliceSession,
		"unknown token":    "Bearer " + auth.APITokenPrefix + strings.Repeat("0", 64),
		"revoked token":    revoked,
		"expired token":    expired,
	}
	var want string
	for name, bearer := range cases {
		rr := serveCI(f.rt, tokenCI(t, bearer, map[string]string{"flowId": f.aliceFlow}))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s, want 401", name, rr.Code, rr.Body.String())
			continue
		}
		if want == "" {
			want = rr.Body.String()
		} else if rr.Body.String() != want {
			t.Errorf("%s answers %q, another refusal %q", name, rr.Body.String(), want)
		}
	}

	setCIGlobalSecret(t, f.rt, "topsecret")
	signed := ciCall(t, "topsecret", "", map[string]string{"flowId": f.aliceFlow})
	signed.Header.Set("Authorization", rw)
	if rr := serveCI(f.rt, signed); rr.Code != http.StatusUnauthorized {
		t.Errorf("a refused bearer with a valid signature: %d, want 401 (the bearer decides)", rr.Code)
	}
}

// The ci scope grants nothing outside the webhook, like chat outside chat.
func TestCIScope_GrantsNothingElse(t *testing.T) {
	f := newCITokenFixture(t)
	_, ci := f.token(t, auth.ScopeCI)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/library", nil},
		{http.MethodGet, "/api/auth/me", nil},
		{http.MethodPost, "/api/analysis/analyze", map[string]any{"flowId": f.aliceFlow}},
		{http.MethodPost, "/api/ws-ticket", nil},
		{http.MethodGet, "/api/auth/tokens", nil},
	} {
		if rr := doRequestWithAuth(t, f.rt, c.method, c.path, ci, c.body); rr.Code != http.StatusForbidden {
			t.Errorf("%s %s with a ci token: %d, want 403", c.method, c.path, rr.Code)
		}
	}
}

// A token's delivery ids are recorded per token: a replay is a 409, and another token may use the same id.
func TestCIWebhook_TokenDeliveryReplay(t *testing.T) {
	f := newCITokenFixture(t)
	_, first := f.token(t, auth.ScopeCI)
	_, second := f.token(t, auth.ScopeCI, auth.ScopeRead)
	body := map[string]string{"flowId": f.aliceFlow, "deliveryId": "token-delivery-0123456789abcdef"}
	checkStatus(t, serveCI(f.rt, tokenCI(t, first, body)), http.StatusOK)
	wantReplayed(t, "the same token", serveCI(f.rt, tokenCI(t, first, body)))
	checkStatus(t, serveCI(f.rt, tokenCI(t, second, body)), http.StatusOK)
}
