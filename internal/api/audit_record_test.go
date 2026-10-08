package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// existingColumns are the audit row's columns from before internal/audit, which Record must keep writing.
func existingColumns(ev *storageif.AuditEvent) string {
	return fmt.Sprintf("user=%q email=%q action=%q type=%q id=%q ip=%q meta=%v",
		ev.UserID, ev.Email, ev.Action, ev.ResourceType, ev.ResourceID, ev.IP, ev.Meta)
}

// legacyColumns is the row the handlers' audit helper wrote before internal/audit, computed the way it computed
// it: the user and email from the claims alone (none in local mode), the address from r by the handler's
// trusted proxies, the action, resource and meta as passed.
func legacyColumns(ctx context.Context, r *http.Request, proxies []string, e audit.Entry) *storageif.AuditEvent {
	ev := &storageif.AuditEvent{Action: e.Action, ResourceType: e.ResourceType, ResourceID: e.ResourceID, Meta: e.Meta,
		IP: middleware.ClientIP(r, proxies)}
	if c := auth.ClaimsFromContext(ctx); c != nil {
		ev.UserID, ev.Email = c.UserID, c.Email
	}
	return ev
}

// TestRecord_WritesLogAuditsColumns: for every actor, request and set of trusted proxies, Record's row has the
// user, email, action, resource, address and meta the handlers' audit helper wrote before internal/audit
// (legacyColumns). That helper computed the address from r with the handler's trusted proxies and Record reads
// it from the request's reqinfo.Info, which the outer chain (RequestInfo, on the same cfg.Server.TrustedProxies
// as SecurityConfig.TrustedProxies) or, without it, Router.withPolicy records by the same rules. The user comes
// from the claims alone: local mode's actor has none, and its rows stay without a user. The actor kind and token
// say who acted.
func TestRecord_WritesLogAuditsColumns(t *testing.T) {
	ch := captureAuditEvents(t, 1)
	backend := testutil.NewFakeBackend()
	rec := testAuditRecorder(backend)

	actors := []struct {
		name        string
		with        func(context.Context) context.Context
		kind, token string
	}{
		{"session", func(ctx context.Context) context.Context {
			return auth.WithClaims(ctx, &auth.Claims{UserID: "u-1", Email: "a@example.com", Role: auth.RoleMember})
		}, "user", ""},
		{"api token", func(ctx context.Context) context.Context {
			return auth.WithClaims(ctx, auth.ClaimsForPAT("u-1", "a@example.com", auth.RoleMember, "tok-1", nil, []string{"write"}))
		}, "token", "tok-1"},
		{"local user", func(ctx context.Context) context.Context { return auth.WithActor(ctx, auth.LocalActor("local")) }, "user", ""},
		{"nobody", func(ctx context.Context) context.Context { return ctx }, "", ""},
	}
	proxyConfigs := [][]string{nil, {"10.0.0.0/8"}, {"10.0.0.1", "::1"}, {"2001:db8::/32"}}
	remotes := []string{"203.0.113.5:1234", "10.0.0.1:443", "[::1]:5678", "[2001:db8::7]:80", "198.51.100.1", "not-an-address"}
	forwarded := []string{"", "198.51.100.7", "192.0.2.1, 198.51.100.7", " 198.51.100.8 "}
	// How the request reached the handler: through the outer chain, through the Router alone (handler tests).
	layers := map[string]func(proxies []string, h http.Handler) http.Handler{
		"outer chain": func(proxies []string, h http.Handler) http.Handler { return middleware.RequestInfo(proxies)(h) },
		"router alone": func(proxies []string, h http.Handler) http.Handler {
			rt := &Router{trustedProxies: proxies}
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, rt.withPolicy(r)) })
		},
	}
	entry := audit.Entry{Action: audit.ActionFlowSave, ResourceType: "flow", ResourceID: "f-1", Meta: map[string]string{"k": "v"}}

	for _, actor := range actors {
		for layerName, layer := range layers {
			for _, proxies := range proxyConfigs {
				for _, remote := range remotes {
					for _, xff := range forwarded {
						name := fmt.Sprintf("%s/%s/proxies=%v/remote=%s/xff=%q", actor.name, layerName, proxies, remote, xff)
						r := httptest.NewRequest(http.MethodPost, "/api/flows", nil)
						r.RemoteAddr = remote
						if xff != "" {
							r.Header.Set("X-Forwarded-For", xff)
						}
						var legacy, recorded *storageif.AuditEvent
						layer(proxies, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
							ctx := actor.with(r.Context())
							legacy = legacyColumns(ctx, r, proxies, entry)
							rec.Record(ctx, entry)
							recorded = nextAuditEvent(t, ch)
						})).ServeHTTP(httptest.NewRecorder(), r)
						if got, want := existingColumns(recorded), existingColumns(legacy); got != want {
							t.Errorf("%s:\nRecord %s\nlegacy %s", name, got, want)
						}
						if recorded.ActorKind != actor.kind || recorded.TokenID != actor.token {
							t.Errorf("%s: actor kind %q token %q, want %q %q", name, recorded.ActorKind, recorded.TokenID, actor.kind, actor.token)
						}
					}
				}
			}
		}
	}
}

// TestRecord_RowsThroughTheFullChain: a converted handler's row, served by BuildHandler behind a trusted proxy,
// carries the forwarded address and, in local mode with a database, no user (the actor kind says "user"); in
// cloud mode the session's user.
func TestRecord_RowsThroughTheFullChain(t *testing.T) {
	trusted := []string{"10.0.0.0/8"}
	withProxies := func(c *config.Config) { c.Server.TrustedProxies = trusted }
	store := testutil.NewFakeBackend()
	// The session's user has a row, as every account does; the settings row needs it.
	store.Users = map[string]*storageif.User{"u-1": {ID: "u-1", Email: "u@example.com"}}
	// The settings write stores a row for the caller, which must be a user; nothing creates the "local" user a
	// deployment without accounts writes as (on Postgres the write fails), and that is not what this test pins.
	local := testutil.NewFakeBackend()
	local.Users = map[string]*storageif.User{"local": {ID: "local"}}
	for _, tc := range []struct {
		name       string
		rt         *Router
		authz      func(*Router) string
		user, mail string
		resourceID string
	}{
		{"local+db", newTestRouterSSO(local, false, nil, nil, withProxies),
			func(*Router) string { return "Bearer " + testToken }, "", "", "local"},
		{"cloud", newTestRouterSSO(store, true, nil, nil, withProxies),
			func(rt *Router) string { return jwtBearer(t, rt, "u-1", "u@example.com") }, "u-1", "u@example.com", "u-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := captureAuditEvents(t, 1)
			cfg := config.Default()
			cfg.Server.TrustedProxies = trusted
			h, limiters := BuildHandler(tc.rt, cfg, nil)
			for _, l := range limiters {
				l.Stop()
			}
			req := httptest.NewRequest(http.MethodPost, "/api/system/settings", strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", tc.authz(tc.rt))
			req.RemoteAddr = "10.0.0.1:443"
			req.Header.Set("X-Forwarded-For", "192.0.2.1, 198.51.100.7")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			checkStatus(t, rr, http.StatusOK)

			ev := nextAuditEvent(t, ch)
			if ev.UserID != tc.user || ev.Email != tc.mail || ev.ActorKind != "user" || ev.IP != "198.51.100.7" ||
				ev.Action != audit.ActionSettingsChange || ev.ResourceType != "settings" || ev.ResourceID != tc.resourceID ||
				ev.Meta["scope"] != "user" || ev.OrgID != "" {
				t.Errorf("row = %+v", ev)
			}
		})
	}
}
