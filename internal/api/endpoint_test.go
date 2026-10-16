package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/platform/reqinfo"
	"pad-analyzer/internal/service"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// Test-only typed endpoints, mounted with mountTestRoutes so they run through the router's real chain and policy.

type probeRequest struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	// Fail names the error the endpoint returns.
	Fail string `json:"fail,omitempty"`
}

func (r probeRequest) Validate() error {
	switch {
	case r.Name == "":
		return errors.New("name is required")
	case r.Count < 0:
		return apperr.Unprocessable("NEGATIVE_COUNT", "count must not be negative").WithDetails(map[string]any{"field": "count"})
	}
	return nil
}

type probeResponse struct {
	Greeting string `json:"greeting"`
	Count    int    `json:"count"`
}

// probeEndpoint answers a greeting, or the error req.Fail names.
func probeEndpoint(_ context.Context, req probeRequest) (probeResponse, error) {
	switch req.Fail {
	case "not-found":
		return probeResponse{}, fmt.Errorf("load the flow: %w", service.ErrNotFound)
	case "permission":
		return probeResponse{}, service.ErrPermissionDenied
	case "scope":
		return probeResponse{}, service.ErrScopeDenied
	case "version":
		return probeResponse{}, storageif.ErrVersionConflict
	case "invalid-input":
		return probeResponse{}, fmt.Errorf("%w: the rule has no id", service.ErrInvalidInput)
	case "apperr":
		return probeResponse{}, apperr.Conflict("FLOW_LOCKED", "the flow is locked").WithDetails(map[string]any{"lockedBy": "u-2"})
	case "plain":
		return probeResponse{}, errors.New("db: connection reset by peer")
	}
	return probeResponse{Greeting: "hello " + req.Name, Count: req.Count}, nil
}

// errorBody decodes an error envelope.
func errorBody(t *testing.T, rr *httptest.ResponseRecorder) render.ErrorResponse {
	t.Helper()
	var body render.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope %q: %v", rr.Body.String(), err)
	}
	return body
}

func sendRaw(rt *Router, method, target, authHeader, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", authHeader)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

// The JSON adapter: strict decoding, Validate, the errors fn returns, and the success status.
func TestJSONEndpoint_DecodesValidatesAndAnswers(t *testing.T) {
	rt := newJWTTestRouter(t)
	var calls atomic.Int32
	counted := func(ctx context.Context, req probeRequest) (probeResponse, error) {
		calls.Add(1)
		return probeEndpoint(ctx, req)
	}
	mountTestRoutes(t, rt, func(rb *route.Builder) {
		rb.Post("/api/typed-probe/echo", JSON(counted))
		rb.Post("/api/typed-probe/create", JSON(counted, Status(http.StatusCreated)))
		rb.Post("/api/typed-probe/silent", JSON(counted, Status(http.StatusNoContent)))
	})
	bearer := jwtBearer(t, rt, "u-1", "u1@example.com")

	tests := []struct {
		name, path, body string
		wantStatus       int
		wantBody         string // the exact success body
		wantCode         string // the error envelope's code
		wantMessage      string // a substring of the error message
		wantDetails      map[string]any
		wantCalled       bool
	}{
		{name: "200 with the response", path: "echo", body: `{"name":"ada","count":2}`, wantStatus: 200, wantBody: `{"greeting":"hello ada","count":2}`, wantCalled: true},
		{name: "201 from Status", path: "create", body: `{"name":"ada","count":2}`, wantStatus: 201, wantBody: `{"greeting":"hello ada","count":2}`, wantCalled: true},
		{name: "204 without a body", path: "silent", body: `{"name":"ada"}`, wantStatus: 204, wantCalled: true},

		{name: "malformed JSON", path: "echo", body: `{"name":`, wantStatus: 400, wantCode: "BAD_REQUEST", wantMessage: "unexpected EOF"},
		{name: "not JSON", path: "echo", body: `not-json`, wantStatus: 400, wantCode: "BAD_REQUEST", wantMessage: "invalid character"},
		{name: "an unknown field", path: "echo", body: `{"name":"ada","nickname":"a"}`, wantStatus: 400, wantCode: "BAD_REQUEST", wantMessage: `json: unknown field "nickname"`},
		{name: "a wrong type", path: "echo", body: `{"name":"ada","count":"two"}`, wantStatus: 400, wantCode: "BAD_REQUEST", wantMessage: "cannot unmarshal string"},
		{name: "an empty body", path: "echo", body: ``, wantStatus: 400, wantCode: "BAD_REQUEST", wantMessage: "the request body is empty"},
		{name: "data after the value", path: "echo", body: `{"name":"ada"} {"name":"bob"}`, wantStatus: 400, wantCode: "BAD_REQUEST", wantMessage: "unexpected data after the JSON value"},

		{name: "Validate's plain error is a 400", path: "echo", body: `{"count":1}`, wantStatus: 400, wantCode: "BAD_REQUEST", wantMessage: "name is required"},
		{name: "Validate's apperr is used as is", path: "echo", body: `{"name":"ada","count":-1}`, wantStatus: 422, wantCode: "NEGATIVE_COUNT", wantMessage: "count must not be negative", wantDetails: map[string]any{"field": "count"}},

		{name: "ErrNotFound", path: "echo", body: `{"name":"ada","fail":"not-found"}`, wantStatus: 404, wantCode: "NOT_FOUND", wantMessage: "load the flow: resource not found", wantCalled: true},
		{name: "ErrPermissionDenied", path: "echo", body: `{"name":"ada","fail":"permission"}`, wantStatus: 403, wantCode: "FORBIDDEN", wantMessage: "permission denied", wantCalled: true},
		{name: "ErrScopeDenied", path: "echo", body: `{"name":"ada","fail":"scope"}`, wantStatus: 403, wantCode: "FORBIDDEN", wantMessage: "missing API token scope", wantCalled: true},
		{name: "ErrVersionConflict", path: "echo", body: `{"name":"ada","fail":"version"}`, wantStatus: 409, wantCode: "CONFLICT", wantCalled: true},
		{name: "ErrInvalidInput", path: "echo", body: `{"name":"ada","fail":"invalid-input"}`, wantStatus: 400, wantCode: "BAD_REQUEST", wantMessage: "the rule has no id", wantCalled: true},
		{name: "an apperr from fn", path: "create", body: `{"name":"ada","fail":"apperr"}`, wantStatus: 409, wantCode: "FLOW_LOCKED", wantMessage: "the flow is locked", wantDetails: map[string]any{"lockedBy": "u-2"}, wantCalled: true},
		{name: "a plain error is a masked 500", path: "echo", body: `{"name":"ada","fail":"plain"}`, wantStatus: 500, wantCode: "INTERNAL_ERROR", wantMessage: "internal server error", wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls.Store(0)
			rr := sendRaw(rt, http.MethodPost, "/api/typed-probe/"+tt.path, bearer, tt.body)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d; body %s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if called := calls.Load() == 1; called != tt.wantCalled {
				t.Errorf("fn called: %v, want %v", called, tt.wantCalled)
			}
			switch {
			case tt.wantStatus == http.StatusNoContent:
				if rr.Body.Len() != 0 {
					t.Errorf("a 204 carried a body: %q", rr.Body.String())
				}
			case tt.wantCode == "":
				if got := strings.TrimSpace(rr.Body.String()); got != tt.wantBody {
					t.Errorf("body %s, want %s", got, tt.wantBody)
				}
				if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type %q", ct)
				}
			default:
				body := errorBody(t, rr)
				if body.Code != tt.wantCode || !strings.Contains(body.Message, tt.wantMessage) || !reflect.DeepEqual(body.Details, tt.wantDetails) {
					t.Errorf("envelope %+v, want code %s, a message containing %q and details %v", body, tt.wantCode, tt.wantMessage, tt.wantDetails)
				}
			}
		})
	}
}

// A body over the router's size limit gets the answer decodeBody has always given it.
func TestJSONEndpoint_OversizedBodyAnswersAsDecodeBody(t *testing.T) {
	rt := newJWTTestRouter(t)
	mountTestRoutes(t, rt, func(rb *route.Builder) {
		rb.Post("/api/typed-probe/echo", JSON(probeEndpoint))
		rb.Post("/api/typed-probe/hand-written", func(w http.ResponseWriter, r *http.Request) {
			var req probeRequest
			if decodeBody(w, r, &req) {
				render.JSON(w, req)
			}
		})
	})
	bearer := jwtBearer(t, rt, "u-1", "u1@example.com")
	huge := `{"name":"` + strings.Repeat("a", 10<<20) + `"}`

	typed := sendRaw(rt, http.MethodPost, "/api/typed-probe/echo", bearer, huge)
	legacy := sendRaw(rt, http.MethodPost, "/api/typed-probe/hand-written", bearer, huge)
	if typed.Code != http.StatusBadRequest || typed.Code != legacy.Code || typed.Body.String() != legacy.Body.String() {
		t.Errorf("typed endpoint: %d %s; decodeBody: %d %s", typed.Code, typed.Body.String(), legacy.Code, legacy.Body.String())
	}
	if body := errorBody(t, typed); body.Code != "BAD_REQUEST" || body.Message != "http: request body too large" {
		t.Errorf("envelope %+v", body)
	}
}

// fn runs on the request's own context: the actor jwtAuth authenticated, the request's reqinfo.Info with the
// route's policy, and the request's lazy RLS scope, which fn's first store access begins.
func TestTypedEndpoint_RunsOnTheRequestContext(t *testing.T) {
	// A transaction that starts: a read whose start fails is refused (503) since reads fail closed.
	backend := newLazyRLSBackend(t)
	rt := newTestRouterSSO(backend, true, nil, nil)
	var actor *auth.Actor
	var info *reqinfo.Info
	var rlsErr error
	mountTestRoutes(t, rt, func(rb *route.Builder) {
		rb.Get("/api/typed-probe/context", Query(func(ctx context.Context, _ struct{}) (struct{}, error) {
			actor, info, rlsErr = auth.ActorFromContext(ctx), reqinfo.From(ctx), storagedb.EnsureRLS(ctx)
			return struct{}{}, nil
		}))
	})

	backend.begins.Store(0)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/typed-probe/context", jwtBearer(t, rt, "u-ctx", "ctx@example.com"), nil), http.StatusOK)
	if actor == nil || actor.UserID != "u-ctx" || actor.Kind != auth.ActorUser {
		t.Errorf("fn's actor %+v, want the session of u-ctx", actor)
	}
	if want := rt.routes.Lookup(http.MethodGet, "/api/typed-probe/context"); info == nil || info.Policy != want || !reflect.DeepEqual(info.Actor(), actor) {
		t.Errorf("fn's reqinfo %+v, want the request's, with the route's policy and actor", info)
	}
	if n := backend.begins.Load(); n != 1 || rlsErr != nil {
		t.Errorf("fn's first store access began %d RLS transaction(s) (err %v), want the request's lazy scope begun once", n, rlsErr)
	}
}

type probeListRequest struct {
	PageRequest
	Flow   string   `path:"id"`
	Org    string   `query:"orgId"`
	Tags   []string `query:"tags"`
	IDs    []int    `query:"ids"`
	Active bool     `query:"active"`
	Since  int64    `query:"since"`
}

func (r probeListRequest) Validate() error {
	if r.Org == "o-closed" {
		return apperr.Forbidden("ORG_CLOSED", "the organisation is closed")
	}
	return nil
}

// The Query adapter fills strings, ints, bools and slices from the query string and the path, bounds an embedded
// PageRequest, validates, and answers a Page.
func TestQueryEndpoint_DecodesTheURL(t *testing.T) {
	rt := newJWTTestRouter(t)
	var got probeListRequest
	list := func(_ context.Context, req probeListRequest) (Page[string], error) {
		got = req
		total := len(req.Tags)
		return Page[string]{Items: req.Tags, NextCursor: "c-next", Total: &total}, nil
	}
	mountTestRoutes(t, rt, func(rb *route.Builder) {
		rb.Get("/api/typed-probe/flows/{id}/items", Query(list))
		rb.Get("/api/typed-probe/flows/{id}/small-items", Query(list, PageLimits(10, 20)))
	})
	bearer := jwtBearer(t, rt, "u-1", "u1@example.com")

	tests := []struct {
		name, target string
		want         probeListRequest
		wantStatus   int
		wantError    string // the error envelope's code and message
	}{
		{name: "every kind of parameter", target: "flows/f-9/items?orgId=o-1&tags=a,b&tags=c&ids=1,%202&active=true&since=-5&limit=10&cursor=c-1&unknown=x",
			want: probeListRequest{PageRequest: PageRequest{Limit: 10, Cursor: "c-1"}, Flow: "f-9", Org: "o-1", Tags: []string{"a", "b", "c"}, IDs: []int{1, 2}, Active: true, Since: -5}},
		{name: "nothing given", target: "flows/f-9/items",
			want: probeListRequest{PageRequest: PageRequest{Limit: DefaultPageLimit}, Flow: "f-9"}},
		{name: "empty values are absent", target: "flows/f-9/items?orgId=&tags=&tags=,&active=&limit=",
			want: probeListRequest{PageRequest: PageRequest{Limit: DefaultPageLimit}, Flow: "f-9"}},
		{name: "a limit below 1 is the default", target: "flows/f-9/items?limit=-3",
			want: probeListRequest{PageRequest: PageRequest{Limit: DefaultPageLimit}, Flow: "f-9"}},
		{name: "a limit above the maximum is the maximum", target: "flows/f-9/items?limit=5000",
			want: probeListRequest{PageRequest: PageRequest{Limit: MaxPageLimit}, Flow: "f-9"}},
		{name: "the endpoint's own default", target: "flows/f-9/small-items",
			want: probeListRequest{PageRequest: PageRequest{Limit: 10}, Flow: "f-9"}},
		{name: "the endpoint's own maximum", target: "flows/f-9/small-items?limit=25",
			want: probeListRequest{PageRequest: PageRequest{Limit: 20}, Flow: "f-9"}},

		{name: "a malformed int", target: "flows/f-9/items?limit=ten", wantStatus: 400, wantError: `BAD_REQUEST invalid "limit" parameter: must be an integer`},
		{name: "a malformed int in a list", target: "flows/f-9/items?ids=1,x", wantStatus: 400, wantError: `BAD_REQUEST invalid "ids" parameter: must be an integer`},
		{name: "a malformed bool", target: "flows/f-9/items?active=maybe", wantStatus: 400, wantError: `BAD_REQUEST invalid "active" parameter: use true or false`},
		{name: "Validate", target: "flows/f-9/items?orgId=o-closed", wantStatus: 403, wantError: "ORG_CLOSED the organisation is closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got = probeListRequest{}
			rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/typed-probe/"+tt.target, bearer, nil)
			if tt.wantError != "" {
				body := errorBody(t, rr)
				if rr.Code != tt.wantStatus || body.Code+" "+body.Message != tt.wantError {
					t.Errorf("%d %+v, want %d %s", rr.Code, body, tt.wantStatus, tt.wantError)
				}
				return
			}
			checkStatus(t, rr, http.StatusOK)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decoded %+v, want %+v", got, tt.want)
			}
		})
	}

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/typed-probe/flows/f-9/items?tags=a,b,c", bearer, nil)
	if want := `{"items":["a","b","c"],"nextCursor":"c-next","total":3}`; strings.TrimSpace(rr.Body.String()) != want {
		t.Errorf("page %s, want %s", rr.Body.String(), want)
	}

	// Through the whole stack, the error envelope carries the request's ID.
	h := fullChain(t, rt, nil)
	rr = serve(h, http.MethodGet, "/api/typed-probe/flows/f-9/items?limit=ten", bearer)
	if id := rr.Header().Get(middleware.RequestIDHeader); id == "" || errorBody(t, rr).RequestID != id {
		t.Errorf("requestId %q, X-Request-ID %q", errorBody(t, rr).RequestID, id)
	}
}

type probeFilter struct {
	Tag string `query:"tag"`
}

// A path tag naming none of the route's parameters cannot be caught when the handler is built (the adapter never
// sees the pattern): it fails every request loudly instead of keeping what the body put in the field.
func TestJSONEndpoint_PathTagTheRouteLacksIsAnError(t *testing.T) {
	rt := newJWTTestRouter(t)
	ran := false
	rename := func(_ context.Context, req struct {
		Flow string `json:"flowId" path:"flowid"`
	}) (struct{}, error) {
		ran = true
		return struct{}{}, nil
	}
	mountTestRoutes(t, rt, func(rb *route.Builder) {
		rb.Post("/api/typed-probe/misnamed/{id}", JSON(rename))
	})
	bearer := jwtBearer(t, rt, "u-1", "u1@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/typed-probe/misnamed/f-real", bearer, map[string]string{"flowId": "f-body"})
	if rr.Code != http.StatusInternalServerError || ran {
		t.Errorf("%d (handler ran: %v), want 500 before the handler", rr.Code, ran)
	}
}

func TestPage_JSONShape(t *testing.T) {
	t.Parallel()
	zero := 0
	tests := []struct {
		page any
		want string
	}{
		{Page[string]{}, `{"items":[]}`},
		{&Page[string]{Items: []string{}}, `{"items":[]}`},
		{Page[string]{Items: []string{"a"}, NextCursor: "c-2"}, `{"items":["a"],"nextCursor":"c-2"}`},
		{Page[int]{Items: []int{1, 2}, Total: &zero}, `{"items":[1,2],"total":0}`},
		{Page[probeResponse]{Items: []probeResponse{{Greeting: "hi"}}}, `{"items":[{"greeting":"hi","count":0}]}`},
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.page)
		if err != nil || string(got) != tt.want {
			t.Errorf("%#v: %s (%v), want %s", tt.page, got, err, tt.want)
		}
	}
}

type probeFlowBody struct {
	FlowID string `json:"flowId"`
}

func (r probeFlowBody) FlowRef() string { return r.FlowID }

type probeFlowPath struct {
	FlowID string `path:"id"`
}

func (r probeFlowPath) FlowRef() string { return r.FlowID }

// AuthorizeFlow: the owner's request reaches fn with the stored header, while an outsider gets 403 and a missing
// flow 404 before fn runs. A scoped token the route admits is refused 403 when it lacks the action's scope. Local
// mode, which has no accounts, checks nothing. In every route-policy mode.
func TestTypedEndpoint_AuthorizeFlow(t *testing.T) {
	type outcome struct {
		status  int
		code    string
		message string // a substring of the error message
	}
	for _, mode := range allRoutePolicyModes {
		t.Run(string(mode), func(t *testing.T) {
			rt := newModeTestRouter(t, mode)
			authz := service.NewAuthzService(rt.security.Backend, rt.security.OrgSvc)
			seedOrgFlow(t, rt, "f-1", "u-owner", "")
			var calls atomic.Int32
			var header *storageif.FlowDocument
			byBody := func(ctx context.Context, req probeFlowBody) (probeResponse, error) {
				calls.Add(1)
				header = AuthorizedFlow(ctx)
				return probeResponse{Greeting: header.ID}, nil
			}
			byPath := func(ctx context.Context, req probeFlowPath) (probeResponse, error) {
				calls.Add(1)
				header = AuthorizedFlow(ctx)
				return probeResponse{Greeting: header.ID}, nil
			}
			mountTestRoutes(t, rt, func(rb *route.Builder) {
				rb.Post("/api/typed-probe/flow-read", JSON(byBody, AuthorizeFlow(rt.security, authz, service.FlowRead)))
				rb.Get("/api/typed-probe/flows/{id}/read", Query(byPath, AuthorizeFlow(rt.security, authz, service.FlowRead)))
				rb.Get("/api/typed-probe/flows/{id}/edit-check", Query(byPath, AuthorizeFlow(rt.security, authz, service.FlowEdit)))
			})
			owner := jwtBearer(t, rt, "u-owner", "owner@example.com")
			outsider := jwtBearer(t, rt, "u-outsider", "outsider@example.com")
			readToken := "Bearer " + mintToken(t, rt, "u-owner", "owner@example.com", auth.RoleMember, nil, auth.ScopeRead)

			tests := []struct {
				name, who, method, target string
				body                      any
				want                      outcome
			}{
				{"the owner", owner, http.MethodPost, "/api/typed-probe/flow-read", probeFlowBody{FlowID: "f-1"}, outcome{status: 200}},
				{"an outsider", outsider, http.MethodPost, "/api/typed-probe/flow-read", probeFlowBody{FlowID: "f-1"}, outcome{403, "FORBIDDEN", "permission denied"}},
				{"a missing flow", owner, http.MethodPost, "/api/typed-probe/flow-read", probeFlowBody{FlowID: "f-missing"}, outcome{404, "NOT_FOUND", "not found"}},
				{"the owner, by path", owner, http.MethodGet, "/api/typed-probe/flows/f-1/read", nil, outcome{status: 200}},
				{"an outsider, by path", outsider, http.MethodGet, "/api/typed-probe/flows/f-1/read", nil, outcome{403, "FORBIDDEN", "permission denied"}},
				{"a read token reading", readToken, http.MethodGet, "/api/typed-probe/flows/f-1/read", nil, outcome{status: 200}},
				{"a read token on an edit action", readToken, http.MethodGet, "/api/typed-probe/flows/f-1/edit-check", nil, outcome{403, "FORBIDDEN", "flow.edit needs write"}},
			}
			for _, tt := range tests {
				calls.Store(0)
				header = nil
				rr := doRequestWithAuth(t, rt, tt.method, tt.target, tt.who, tt.body)
				if rr.Code != tt.want.status {
					t.Errorf("%s: status %d, want %d; body %s", tt.name, rr.Code, tt.want.status, rr.Body.String())
					continue
				}
				if tt.want.status == http.StatusOK {
					if calls.Load() != 1 || header == nil || header.ID != "f-1" || header.OwnerID != "u-owner" {
						t.Errorf("%s: fn ran %d time(s) with header %+v, want once with f-1's", tt.name, calls.Load(), header)
					}
					continue
				}
				if calls.Load() != 0 {
					t.Errorf("%s: fn ran on a refused request", tt.name)
				}
				if body := errorBody(t, rr); body.Code != tt.want.code || !strings.Contains(body.Message, tt.want.message) {
					t.Errorf("%s: envelope %+v, want %s with %q", tt.name, body, tt.want.code, tt.want.message)
				}
			}
		})
	}

	t.Run("local mode", func(t *testing.T) {
		store := testutil.NewFakeBackend()
		rt := newTestRouter(store, false)
		authz := service.NewAuthzService(store, rt.security.OrgSvc)
		ran := false
		mountTestRoutes(t, rt, func(rb *route.Builder) {
			rb.Post("/api/typed-probe/flow-read", JSON(func(ctx context.Context, req probeFlowBody) (probeResponse, error) {
				ran = AuthorizedFlow(ctx) == nil
				return probeResponse{}, nil
			}, AuthorizeFlow(rt.security, authz, service.FlowEdit)))
		})
		checkStatus(t, doRequest(t, rt, http.MethodPost, "/api/typed-probe/flow-read", probeFlowBody{FlowID: "no-such-flow"}), http.StatusOK)
		if !ran {
			t.Error("fn did not run without a header")
		}
	})
}

// JSON and Query refuse, when they build the handler, a request and options that do not fit.
func TestTypedEndpoint_RefusesMisdeclarations(t *testing.T) {
	sec := &SecurityConfig{JWTEnabled: true}
	authz := service.NewAuthzService(nil, nil)
	none := func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil }
	byBody := func(context.Context, probeFlowBody) (struct{}, error) { return struct{}{}, nil }
	paged := func(context.Context, probeListRequest) (struct{}, error) { return struct{}{}, nil }

	builds := map[string]func(){
		"a flow request without AuthorizeFlow":   func() { JSON(byBody) },
		"AuthorizeFlow without a flow request":   func() { JSON(probeEndpoint, AuthorizeFlow(sec, authz, service.FlowRead)) },
		"AuthorizeFlow without the AuthzService": func() { JSON(byBody, AuthorizeFlow(sec, nil, service.FlowRead)) },
		"AuthorizeFlow without the security":     func() { JSON(byBody, AuthorizeFlow(nil, authz, service.FlowRead)) },
		"a pointer request":                      func() { JSON(func(context.Context, *probeRequest) (struct{}, error) { return struct{}{}, nil }) },
		"Query with a request that is no struct": func() { Query(func(context.Context, []string) (struct{}, error) { return struct{}{}, nil }) },
		"a parameter of another type": func() {
			Query(func(context.Context, struct {
				At float64 `query:"at"`
			}) (struct{}, error) {
				return struct{}{}, nil
			})
		},
		"an unexported parameter": func() {
			Query(func(context.Context, struct {
				at string `query:"at"`
			}) (struct{}, error) {
				return struct{}{}, nil
			})
		},
		"a parameter in both the query and the path": func() {
			Query(func(context.Context, struct {
				ID string `query:"id" path:"id"`
			}) (struct{}, error) {
				return struct{}{}, nil
			})
		},
		"PageRequest embedded by pointer": func() {
			Query(func(context.Context, struct{ *PageRequest }) (struct{}, error) { return struct{}{}, nil })
		},
		"URL parameters embedded by pointer": func() {
			Query(func(context.Context, struct{ *probeFilter }) (struct{}, error) { return struct{}{}, nil })
		},
		"PageLimits without a PageRequest": func() { Query(none, PageLimits(10, 20)) },
		"page limits out of order":         func() { Query(paged, PageLimits(30, 20)) },
		"a status that is no success":      func() { JSON(probeEndpoint, Status(http.StatusFound)) },
	}
	for name, build := range builds {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("the handler was built")
				}
			}()
			build()
		})
	}

	// What fits builds.
	JSON(byBody, AuthorizeFlow(sec, authz, service.FlowRead))
	Query(paged, PageLimits(20, 20))
	Query(func(context.Context, PageRequest) (Page[string], error) { return Page[string]{}, nil })
	JSON(func(context.Context, json.RawMessage) (struct{}, error) { return struct{}{}, nil }, Status(http.StatusAccepted))
}
