package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/sso"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// fakeSSOClient stands in for the OIDC relying party: AuthCodeURL returns a
// deterministic IdP URL and Exchange resolves codes via a lookup table.
type fakeSSOClient struct {
	identities map[string]*sso.Identity // code → identity
	exchanged  []string                 // codes seen by Exchange
}

func (f *fakeSSOClient) ProviderName() string { return "fake-idp" }

func (f *fakeSSOClient) AuthCodeURL(_ context.Context, state, nonce, pkceVerifier string) (string, error) {
	return "https://idp.example.test/authorize?state=" + url.QueryEscape(state), nil
}

func (f *fakeSSOClient) Exchange(_ context.Context, code, pkceVerifier, nonce string) (*sso.Identity, error) {
	f.exchanged = append(f.exchanged, code)
	if ident, ok := f.identities[code]; ok {
		return ident, nil
	}
	return nil, fmt.Errorf("unknown code")
}

func newSSOTestRig(t *testing.T) (*Router, *fakeSSOClient, *memIdentityStore) {
	t.Helper()
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("create local storage: %v", err)
	}
	client := &fakeSSOClient{identities: map[string]*sso.Identity{}}
	ids := newMemIdentityStore()
	rt := newTestRouterSSO(fs, true, client, ids)
	return rt, client, ids
}

// startSSOFlow performs GET /start and returns the flow cookie + the state the
// router round-tripped through the IdP redirect URL.
func startSSOFlow(t *testing.T, rt *Router) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/start", nil)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("start: expected 302, got %d — %s", rr.Code, rr.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == ssoFlowCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("start did not set the sso flow cookie")
	}
	loc, _ := url.Parse(rr.Header().Get("Location"))
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("start redirect carries no state")
	}
	return cookie, state
}

// completeCallback hits /callback with the given cookie/state/code and returns
// the recorder.
func completeCallback(t *testing.T, rt *Router, cookie *http.Cookie, state, code string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/auth/sso/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

// ticketFromRedirect extracts the ssoTicket fragment from a 302 Location.
func ticketFromRedirect(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	loc := rr.Header().Get("Location")
	const marker = "#ssoTicket="
	i := strings.Index(loc, marker)
	if i < 0 {
		t.Fatalf("expected #ssoTicket= in redirect, got %q", loc)
	}
	ticket, err := url.QueryUnescape(loc[i+len(marker):])
	if err != nil {
		t.Fatalf("unescape ticket: %v", err)
	}
	return ticket
}

func TestSSOInfo_DisabledWithoutClient(t *testing.T) {
	rt := newJWTTestRouter(t) // no sso client wired
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/sso/info", "", nil)
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["enabled"] != false {
		t.Errorf("expected enabled=false, got %v", resp["enabled"])
	}
}

func TestSSOInfo_Enabled(t *testing.T) {
	rt, _, _ := newSSOTestRig(t)
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/sso/info", "", nil)
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["enabled"] != true || resp["provider"] != "fake-idp" {
		t.Errorf("expected enabled fake-idp, got %v", resp)
	}
}

func TestSSOStart_RedirectsWithCookie(t *testing.T) {
	rt, _, _ := newSSOTestRig(t)
	cookie, state := startSSOFlow(t, rt)
	if !cookie.HttpOnly {
		t.Error("flow cookie must be HttpOnly")
	}
	if state == "" {
		t.Error("state must be present in IdP redirect")
	}
}

func TestSSOCallback_JITProvisionsUserAndLink(t *testing.T) {
	rt, client, ids := newSSOTestRig(t)
	client.identities["code-1"] = &sso.Identity{
		Subject: "sub-1", Email: "new@example.com", EmailVerified: true, DisplayName: "New User",
	}

	cookie, state := startSSOFlow(t, rt)
	rr := completeCallback(t, rt, cookie, state, "code-1")
	checkStatus(t, rr, http.StatusFound)
	ticket := ticketFromRedirect(t, rr)

	// Exchange the ticket for a token pair.
	xr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/sso/exchange", "", map[string]any{"ticket": ticket})
	checkStatus(t, xr, http.StatusOK)
	var pair map[string]any
	decodeJSON(t, xr, &pair)
	if pair["accessToken"] == "" || pair["refreshToken"] == "" {
		t.Fatalf("expected token pair, got %v", pair)
	}

	// The user exists and the identity is linked.
	u, err := rt.security.Backend.LoadUserByEmail(context.Background(), "new@example.com")
	if err != nil {
		t.Fatalf("JIT user not created: %v", err)
	}
	link, err := ids.LoadIdentityLink(context.Background(), "fake-idp", "sub-1")
	if err != nil || link.UserID != u.ID {
		t.Fatalf("identity link missing or wrong: %v %v", link, err)
	}

	// The access token works on an authenticated route.
	me := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+pair["accessToken"].(string), nil)
	checkStatus(t, me, http.StatusOK)
}

func TestSSOCallback_LinksExistingUserByVerifiedEmail(t *testing.T) {
	rt, client, ids := newSSOTestRig(t)
	seedUserWithRole(t, rt, "u-existing", "existing@example.com", "member")
	client.identities["code-2"] = &sso.Identity{
		Subject: "sub-2", Email: "existing@example.com", EmailVerified: true,
	}

	cookie, state := startSSOFlow(t, rt)
	rr := completeCallback(t, rt, cookie, state, "code-2")
	checkStatus(t, rr, http.StatusFound)
	ticketFromRedirect(t, rr) // must be a success redirect

	link, err := ids.LoadIdentityLink(context.Background(), "fake-idp", "sub-2")
	if err != nil || link.UserID != "u-existing" {
		t.Fatalf("expected link to existing user, got %v %v", link, err)
	}
}

func TestSSOCallback_UnverifiedEmailRefused(t *testing.T) {
	rt, client, ids := newSSOTestRig(t)
	seedUserWithRole(t, rt, "u-victim", "victim@example.com", "member")
	client.identities["code-3"] = &sso.Identity{
		Subject: "sub-3", Email: "victim@example.com", EmailVerified: false,
	}

	cookie, state := startSSOFlow(t, rt)
	rr := completeCallback(t, rt, cookie, state, "code-3")
	checkStatus(t, rr, http.StatusFound)
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "ssoError=") {
		t.Errorf("expected error redirect for unverified email, got %q", loc)
	}
	if _, err := ids.LoadIdentityLink(context.Background(), "fake-idp", "sub-3"); err == nil {
		t.Error("no identity link must be created for an unverified email")
	}
}

func TestSSOCallback_StateMismatchRejected(t *testing.T) {
	rt, client, _ := newSSOTestRig(t)
	client.identities["code-4"] = &sso.Identity{Subject: "sub-4", Email: "x@example.com", EmailVerified: true}

	cookie, _ := startSSOFlow(t, rt)
	rr := completeCallback(t, rt, cookie, "forged-state", "code-4")
	checkStatus(t, rr, http.StatusFound)
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "ssoError=") {
		t.Errorf("expected error redirect on state mismatch, got %q", loc)
	}
	if len(client.exchanged) != 0 {
		t.Error("code must not be exchanged when state does not match")
	}
}

func TestSSOCallback_MissingCookieRejected(t *testing.T) {
	rt, client, _ := newSSOTestRig(t)
	client.identities["code-5"] = &sso.Identity{Subject: "sub-5", Email: "y@example.com", EmailVerified: true}

	rr := completeCallback(t, rt, nil, "some-state", "code-5")
	checkStatus(t, rr, http.StatusFound)
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "ssoError=") {
		t.Errorf("expected error redirect without flow cookie, got %q", loc)
	}
}

func TestSSOExchange_TicketIsSingleUse(t *testing.T) {
	rt, client, _ := newSSOTestRig(t)
	client.identities["code-6"] = &sso.Identity{Subject: "sub-6", Email: "once@example.com", EmailVerified: true}

	cookie, state := startSSOFlow(t, rt)
	ticket := ticketFromRedirect(t, completeCallback(t, rt, cookie, state, "code-6"))

	first := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/sso/exchange", "", map[string]any{"ticket": ticket})
	checkStatus(t, first, http.StatusOK)

	second := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/sso/exchange", "", map[string]any{"ticket": ticket})
	checkStatus(t, second, http.StatusUnauthorized)
}

func TestSSOExchange_AccessTokenNotAcceptedAsTicket(t *testing.T) {
	rt, _, _ := newSSOTestRig(t)
	seedUserWithRole(t, rt, "u1", "u1@example.com", "member")
	bearer := jwtBearer(t, rt, "u1", "u1@example.com")
	accessToken := strings.TrimPrefix(bearer, "Bearer ")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/sso/exchange", "", map[string]any{"ticket": accessToken})
	checkStatus(t, rr, http.StatusUnauthorized)
}

func TestSSOLogin_RepeatLoginReusesUser(t *testing.T) {
	rt, client, _ := newSSOTestRig(t)
	client.identities["code-7"] = &sso.Identity{Subject: "sub-7", Email: "repeat@example.com", EmailVerified: true}

	for i := 0; i < 2; i++ {
		cookie, state := startSSOFlow(t, rt)
		rr := completeCallback(t, rt, cookie, state, "code-7")
		checkStatus(t, rr, http.StatusFound)
		ticketFromRedirect(t, rr)
	}

	users, err := rt.security.Backend.ListUsers(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	count := 0
	for _, u := range users {
		if u.Email == "repeat@example.com" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one user after repeat SSO logins, got %d", count)
	}
}

func TestPasswordLogin_SSOOnlyUserRejected(t *testing.T) {
	rt, client, _ := newSSOTestRig(t)
	client.identities["code-8"] = &sso.Identity{Subject: "sub-8", Email: "ssoonly@example.com", EmailVerified: true}

	cookie, state := startSSOFlow(t, rt)
	completeCallback(t, rt, cookie, state, "code-8")

	// The JIT-provisioned account has no usable password — any password login
	// attempt must fail closed.
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/login", "", map[string]any{
		"email": "ssoonly@example.com", "password": "anything-at-all-123!",
	})
	checkStatus(t, rr, http.StatusUnauthorized)
}

func TestSSOEndpoints_404WhenNotConfigured(t *testing.T) {
	rt := newJWTTestRouter(t) // SSO not wired
	for _, path := range []string{"/api/auth/sso/start", "/api/auth/sso/callback"} {
		rr := doRequestWithAuth(t, rt, http.MethodGet, path, "", nil)
		checkStatus(t, rr, http.StatusNotFound)
	}
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/sso/exchange", "", map[string]any{"ticket": "x"})
	checkStatus(t, rr, http.StatusNotFound)
}

// TestSSOCallback_HardeningInvalidatesOutstandingResetToken is the regression
// test for the SSO account-takeover gap: when a verified SSO identity links an
// existing UNVERIFIED local account, resolveSSOUser strips the shadow password
// to prevent the original (attacker) credential from being used. But until the
// fix that path used SaveUser, not UpdateUserPassword, so outstanding
// password-reset tokens for the shadow account stayed valid — letting an
// attacker who requested a reset link before the SSO link re-arm a known
// password AFTER the legitimate owner took the account over. The reset token
// must be invalidated when the password is stripped.
func TestSSOCallback_HardeningInvalidatesOutstandingResetToken(t *testing.T) {
	rt, client, _ := newSSOTestRig(t)
	ctx := context.Background()

	// Shadow account: unverified email (seedUserWithRole leaves EmailVerified
	// false) carrying an attacker-set password.
	seedUserWithRole(t, rt, "u-shadow", "shadow@example.com", "member")

	// Attacker has an outstanding reset token for this account.
	resetTok := &storageif.UserToken{
		TokenHash: "shadow-reset-hash",
		Purpose:   storageif.TokenPurposePasswordReset,
		UserID:    "u-shadow",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := rt.security.Backend.CreateUserToken(ctx, resetTok); err != nil {
		t.Fatalf("CreateUserToken: %v", err)
	}

	// Legitimate owner authenticates via SSO with a verified email — this
	// triggers the hardening branch in resolveSSOUser.
	client.identities["code-harden"] = &sso.Identity{
		Subject: "sub-harden", Email: "shadow@example.com", EmailVerified: true,
	}
	cookie, state := startSSOFlow(t, rt)
	rr := completeCallback(t, rt, cookie, state, "code-harden")
	checkStatus(t, rr, http.StatusFound)
	ticketFromRedirect(t, rr) // success redirect → linking + hardening completed

	// The attacker's reset token must now be unredeemable.
	_, err := rt.security.Backend.ConsumeUserToken(ctx, storageif.TokenPurposePasswordReset, "shadow-reset-hash")
	if !errors.Is(err, storageif.ErrNotFound) {
		t.Errorf("expected reset token invalidated after SSO hardening, got err=%v", err)
	}
}
