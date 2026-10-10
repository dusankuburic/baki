package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/reqinfo"
	"pad-analyzer/internal/sso"
)

// lockedBuffer collects JSON log lines; background goroutines may log while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b.buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func captureJSONLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b
}

// serveWithInfo sends req through the Router with the reqinfo.Info that RequestInfo installs in production, and
// returns the Info for its recorded actor.
func serveWithInfo(rt *Router, req *http.Request) (*httptest.ResponseRecorder, *reqinfo.Info) {
	info := &reqinfo.Info{ID: "req-test"}
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req.WithContext(reqinfo.With(req.Context(), info)))
	return rr, info
}

func jsonRequest(t *testing.T, method, target, authz string, body any) *http.Request {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, &b)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	return req
}

// checkActor compares the identity fields of the recorded actor with want's; nil want means none recorded.
func checkActor(t *testing.T, what string, info *reqinfo.Info, want *auth.Actor) {
	t.Helper()
	got := info.Actor()
	if want == nil {
		if got != nil {
			t.Errorf("%s: recorded actor %+v, want none", what, got)
		}
		return
	}
	if got == nil || got.Kind != want.Kind || got.UserID != want.UserID || got.Email != want.Email ||
		got.Role != want.Role || got.TokenID != want.TokenID || !slices.Equal(got.Scopes, want.Scopes) {
		t.Errorf("%s: actor = %+v, want %+v", what, got, want)
	}
}

// TestBuildHandler_AccessLogCarriesTheActor: the access log runs outside jwtAuth, so the claims jwtAuth puts on
// its context never reached it and user_id was always empty. Through the whole production chain, the line now
// names the user a session or an API token acts for.
func TestBuildHandler_AccessLogCarriesTheActor(t *testing.T) {
	rt := newJWTTestRouter(t)
	h, limiters := BuildHandler(rt, config.Default(), nil)
	for _, l := range limiters {
		l.Stop()
	}
	pat := mintToken(t, rt, "u-pat", "pat@example.com", auth.RoleMember, nil, auth.ScopeRead)
	logs := captureJSONLogs(t)

	for _, c := range []struct{ name, authz, wantUser string }{
		{"session", jwtBearer(t, rt, "u-jwt", "jwt@example.com"), "u-jwt"},
		{"api token", "Bearer " + pat, "u-pat"},
		{"anonymous", "", ""},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, jsonRequest(t, http.MethodGet, "/api/auth/me", c.authz, nil))
		id := rr.Header().Get(middleware.RequestIDHeader)
		if id == "" {
			t.Fatalf("%s: no %s on the response", c.name, middleware.RequestIDHeader)
		}
		var line map[string]any
		for _, rec := range logs.records(t) {
			if rec["msg"] == "access" && rec["request_id"] == id {
				line = rec
			}
		}
		if line == nil {
			t.Fatalf("%s: no access line with request_id %q", c.name, id)
		}
		if line["user_id"] != c.wantUser {
			t.Errorf("%s: access line user_id = %v, want %q (status %v)", c.name, line["user_id"], c.wantUser, line["status"])
		}
		if rr.Code >= http.StatusBadRequest {
			// The error envelope still quotes the id RequestInfo stamped further out.
			var body struct {
				RequestID string `json:"requestId"`
			}
			decodeJSON(t, rr, &body)
			if body.RequestID != id {
				t.Errorf("%s: error envelope requestId = %q, want %q", c.name, body.RequestID, id)
			}
		}
	}
}

func TestJWTAuth_RecordsTheActor(t *testing.T) {
	rt := newJWTTestRouter(t)
	expires := time.Now().Add(time.Hour)
	pat := mintToken(t, rt, "u-ci", "ci@example.com", auth.RoleMember, &expires, auth.ScopeRead)

	_, info := serveWithInfo(rt, jsonRequest(t, http.MethodGet, "/api/auth/me", jwtBearer(t, rt, "u-1", "u1@example.com"), nil))
	checkActor(t, "session", info, &auth.Actor{Kind: auth.ActorUser, UserID: "u-1", Email: "u1@example.com", Role: auth.RoleAdmin})

	rr, info := serveWithInfo(rt, jsonRequest(t, http.MethodGet, "/api/auth/me", "Bearer "+pat, nil))
	checkStatus(t, rr, http.StatusOK)
	checkActor(t, "scoped API token", info, &auth.Actor{
		Kind: auth.ActorToken, UserID: "u-ci", Email: "ci@example.com", Role: auth.RoleMember,
		TokenID: "tok-u-ci", Scopes: []string{auth.ScopeRead},
	})

	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodGet, "/api/auth/me", "", nil))
	checkStatus(t, rr, http.StatusUnauthorized)
	checkActor(t, "no credentials", info, nil)

	// A token refused for its scope never acted: nothing is recorded for the 403.
	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodPost, "/api/auth/change-password", "Bearer "+pat, nil))
	checkStatus(t, rr, http.StatusForbidden)
	checkActor(t, "refused scoped token", info, nil)

	_, info = serveWithInfo(rt, jsonRequest(t, http.MethodGet, "/api/system/features", jwtBearer(t, rt, "u-1", "u1@example.com"), nil))
	checkActor(t, "public route", info, nil)
}

func TestJWTAuth_RecordsTheLocalUser(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr, info := serveWithInfo(rt, jsonRequest(t, http.MethodGet, "/api/auth/me", "Bearer "+testToken, nil))
	checkStatus(t, rr, http.StatusOK)
	checkActor(t, "local mode", info, &auth.Actor{Kind: auth.ActorUser, UserID: "local", Role: auth.RoleAdmin})

	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodGet, "/api/auth/me", "Bearer wrong", nil))
	checkStatus(t, rr, http.StatusUnauthorized)
	checkActor(t, "local mode, wrong bearer", info, nil)
}

func TestSignIn_RecordsTheActor(t *testing.T) {
	rt := newJWTTestRouter(t)
	const password = "Password123!"

	// The first account is created verified (bootstrap admin), so registering signs it in.
	rr, info := serveWithInfo(rt, jsonRequest(t, http.MethodPost, "/api/auth/register", "", map[string]any{"email": "first@example.com", "password": password}))
	checkStatus(t, rr, http.StatusOK)
	var reg struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		RefreshToken string `json:"refreshToken"`
	}
	decodeJSON(t, rr, &reg)
	first := &auth.Actor{Kind: auth.ActorUser, UserID: reg.User.ID, Email: "first@example.com", Role: auth.RoleAdmin}
	checkActor(t, "register (signs in)", info, first)

	// A later account must verify its email first: registered, not signed in.
	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodPost, "/api/auth/register", "", map[string]any{"email": "second@example.com", "password": password}))
	checkStatus(t, rr, http.StatusOK)
	checkActor(t, "register (verification pending)", info, nil)

	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodPost, "/api/auth/login", "", map[string]any{"email": "first@example.com", "password": password}))
	checkStatus(t, rr, http.StatusOK)
	checkActor(t, "login", info, first)

	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodPost, "/api/auth/login", "", map[string]any{"email": "first@example.com", "password": "Wrong-password-1"}))
	checkStatus(t, rr, http.StatusUnauthorized)
	checkActor(t, "failed login", info, nil)

	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodPost, "/api/auth/refresh", "", map[string]any{"refreshToken": reg.RefreshToken}))
	checkStatus(t, rr, http.StatusOK)
	checkActor(t, "refresh", info, first)

	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodPost, "/api/auth/refresh", "", map[string]any{"refreshToken": reg.RefreshToken}))
	checkStatus(t, rr, http.StatusUnauthorized)
	checkActor(t, "replayed refresh", info, nil)
}

func TestSSOSignIn_RecordsTheActor(t *testing.T) {
	rt, client, _ := newSSOTestRig(t)
	client.identities["code-1"] = &sso.Identity{Subject: "sub-1", Email: "sso@example.com", EmailVerified: true}

	cookie, state := startSSOFlow(t, rt)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/callback?state="+url.QueryEscape(state)+"&code=code-1", nil)
	req.AddCookie(cookie)
	rr, info := serveWithInfo(rt, req)
	checkStatus(t, rr, http.StatusFound)
	user, err := rt.security.Backend.LoadUserByEmail(req.Context(), "sso@example.com")
	if err != nil {
		t.Fatalf("SSO user not provisioned: %v", err)
	}
	want := &auth.Actor{Kind: auth.ActorUser, UserID: user.ID, Email: "sso@example.com", Role: user.Role}
	checkActor(t, "callback", info, want)

	rr, info = serveWithInfo(rt, jsonRequest(t, http.MethodPost, "/api/auth/sso/exchange", "", map[string]any{"ticket": ticketFromRedirect(t, rr)}))
	checkStatus(t, rr, http.StatusOK)
	checkActor(t, "exchange", info, want)
}

// TestWebSocket_RecordsTheActor: /ws authenticates with its ticket, outside jwtAuth. The upgrade itself fails
// on a recorder (it cannot hijack); the actor is recorded once the ticket is consumed.
func TestWebSocket_RecordsTheActor(t *testing.T) {
	rt := newJWTTestRouter(t)
	ticket, _, err := rt.security.AuthMgr.IssueWSTicket(auth.WSTicketSource{UserID: "u-ws", Email: "ws@example.com", Role: auth.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	_, info := serveWithInfo(rt, httptest.NewRequest(http.MethodGet, "/ws?flowId=f1&ticket="+ticket, nil))
	checkActor(t, "websocket", info, &auth.Actor{Kind: auth.ActorUser, UserID: "u-ws", Email: "ws@example.com", Role: auth.RoleMember})

	rr, info := serveWithInfo(rt, httptest.NewRequest(http.MethodGet, "/ws?flowId=f1&ticket="+ticket, nil))
	checkStatus(t, rr, http.StatusUnauthorized)
	checkActor(t, "replayed ticket", info, nil)

	// A PAT's ticket is that token: kind, id and scopes come from the source credential, not the ticket's own jti.
	ticket, _, err = rt.security.AuthMgr.IssueWSTicket(auth.WSTicketSource{
		UserID: "u-ws", Email: "ws@example.com", Role: auth.RoleMember, Scopes: []string{auth.ScopeRead}, AccessJTI: auth.PATJTI("tok-ws"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, info = serveWithInfo(rt, httptest.NewRequest(http.MethodGet, "/ws?flowId=f1&ticket="+ticket, nil))
	checkActor(t, "PAT websocket", info, &auth.Actor{
		Kind: auth.ActorToken, UserID: "u-ws", Email: "ws@example.com", Role: auth.RoleMember, TokenID: "tok-ws", Scopes: []string{auth.ScopeRead},
	})
}
