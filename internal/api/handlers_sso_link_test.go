package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"pad-analyzer/internal/sso"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// startLinkFlow performs the authenticated POST /sso/link/start and returns
// the flow cookie plus the state carried in the IdP URL.
func startLinkFlow(t *testing.T, rt *Router, bearer string) (*http.Cookie, string) {
	t.Helper()
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/sso/link/start", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var body map[string]string
	decodeJSON(t, rr, &body)
	loc, err := url.Parse(body["url"])
	if err != nil || loc.Query().Get("state") == "" {
		t.Fatalf("link/start must return the IdP URL carrying state, got %q", body["url"])
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == ssoFlowCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("link/start did not set the sso flow cookie")
	}
	return cookie, loc.Query().Get("state")
}

// TestSSOLink_AttachesIdentityToSignedInAccount is the flow resolveSSOUser has
// always pointed users at ("link <provider> from your account settings") and
// which did not exist: a password account links an IdP identity, can then
// list it, and can unlink it again while a password remains.
func TestSSOLink_AttachesIdentityToSignedInAccount(t *testing.T) {
	rt, client, ids := newSSOTestRig(t)
	session := login(t, rt, "owner@example.com", "StrongPass123!")
	bearer := "Bearer " + session["accessToken"].(string)
	owner, err := rt.security.Backend.LoadUserByEmail(context.Background(), "owner@example.com")
	if err != nil {
		t.Fatalf("load owner: %v", err)
	}

	cookie, state := startLinkFlow(t, rt, bearer)
	client.identities["code-1"] = &sso.Identity{Subject: "idp-sub-1", Email: "owner@example.com", EmailVerified: true}
	rr := completeCallback(t, rt, cookie, state, "code-1")
	checkStatus(t, rr, http.StatusFound)
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "#ssoLinked=fake-idp") {
		t.Fatalf("link callback must land on #ssoLinked=<provider>, got %q", loc)
	}
	link, ok := ids.links["fake-idp\x00idp-sub-1"]
	if !ok || link.UserID != owner.ID {
		t.Fatalf("identity must be linked to the signed-in user, got %+v", link)
	}

	// The card lists it (without the opaque subject) …
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/sso/links", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var links []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &links); err != nil {
		t.Fatalf("decode links: %v", err)
	}
	if len(links) != 1 || links[0]["provider"] != "fake-idp" {
		t.Fatalf("expected one fake-idp link, got %v", links)
	}
	if _, leaked := links[0]["subject"]; leaked {
		t.Errorf("the IdP subject must not be exposed to the client")
	}

	// … and the identity now logs the owner in through the normal SSO path.
	loginCookie, loginState := startSSOFlow(t, rt)
	client.identities["code-2"] = &sso.Identity{Subject: "idp-sub-1", Email: "owner@example.com", EmailVerified: true}
	rr = completeCallback(t, rt, loginCookie, loginState, "code-2")
	checkStatus(t, rr, http.StatusFound)
	ticketFromRedirect(t, rr)

	// Unlink is allowed because a password remains as a sign-in method.
	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/sso/links/fake-idp", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	if _, still := ids.links["fake-idp\x00idp-sub-1"]; still {
		t.Errorf("unlink must remove the identity link")
	}
}

// TestSSOLink_RefusesIdentityLinkedElsewhere pins the one-identity-one-account
// rule: an IdP subject already attached to a different user cannot be linked
// to the caller (that would be an account takeover via the link flow).
func TestSSOLink_RefusesIdentityLinkedElsewhere(t *testing.T) {
	rt, client, ids := newSSOTestRig(t)
	session := login(t, rt, "owner@example.com", "StrongPass123!")
	bearer := "Bearer " + session["accessToken"].(string)
	ids.links["fake-idp\x00idp-sub-9"] = &storageif.IdentityLink{Provider: "fake-idp", Subject: "idp-sub-9", UserID: "someone-else"}

	cookie, state := startLinkFlow(t, rt, bearer)
	client.identities["code-9"] = &sso.Identity{Subject: "idp-sub-9", Email: "other@example.com", EmailVerified: true}
	rr := completeCallback(t, rt, cookie, state, "code-9")
	checkStatus(t, rr, http.StatusFound)
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "ssoError=") {
		t.Fatalf("linking a foreign identity must fail, got redirect %q", loc)
	}
	if ids.links["fake-idp\x00idp-sub-9"].UserID != "someone-else" {
		t.Errorf("the existing link must be untouched")
	}
}

// TestSSOLink_RequiresAuthentication: the link endpoints are session-scoped.
func TestSSOLink_RequiresAuthentication(t *testing.T) {
	rt, _, _ := newSSOTestRig(t)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/auth/sso/link/start", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("link/start without a session: expected 401, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/auth/sso/links", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("links without a session: expected 401, got %d", rr.Code)
	}
}
