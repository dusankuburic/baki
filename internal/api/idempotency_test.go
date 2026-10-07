package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/route"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// The idempotency layer over the fake store (no database): each request runs inside one of the fake's request
// transactions, committed or rolled back as rlsMiddleware does (below 400 commits). The Postgres suite
// (idempotency_pg_test.go) runs the same flows through the real request transaction.

// idemProbe is a create route for the tests: it counts the requests that reach it and answers what reply says.
type idemProbe struct {
	hits  atomic.Int32
	reply func(w http.ResponseWriter, r *http.Request)
}

func (p *idemProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.hits.Add(1)
	p.reply(w, r)
}

// createdReply answers 201 with a body that differs per run, so a replay is told from a second run.
func createdReply(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Location", "/api/things/1")
	writeJSONStatus(w, http.StatusCreated, map[string]any{"id": time.Now().UnixNano(), "name": req["name"], "target": chi.URLParam(r, "id")})
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// idemTestRouter is a JWT router over the fake backend with the probe mounted at POST /api/idem-probe and
// /api/idem-probe/{id} (Idempotent) and POST /api/idem-plain (not).
func idemTestRouter(t *testing.T, jwt bool, probe *idemProbe) (*Router, *testutil.FakeBackend) {
	t.Helper()
	fb := testutil.NewFakeBackend()
	rt := newTestRouter(fb, jwt)
	mountTestRoutes(t, rt, func(rb *route.Builder) {
		rb.Post("/api/idem-probe", probe.ServeHTTP, route.Idempotent())
		rb.Post("/api/idem-probe/{id}", probe.ServeHTTP, route.Idempotent())
		rb.Post("/api/idem-plain", probe.ServeHTTP)
	})
	return rt, fb
}

// sendIdem sends a request with key (none when empty) inside a fresh fake request transaction, ended as the RLS
// middleware ends it: committed below 400, rolled back otherwise.
func sendIdem(t *testing.T, rt *Router, fb *testutil.FakeBackend, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, end := fb.WithRequestTx(context.Background())
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", idemBearer(t, rt))
	if key != "" {
		req.Header.Set(idempotencyKeyHeader, key)
	}
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	end(rr.Code < http.StatusBadRequest)
	return rr
}

func idemBearer(t *testing.T, rt *Router) string {
	t.Helper()
	if !rt.security.JWTEnabled {
		return "Bearer " + testToken
	}
	return jwtBearer(t, rt, "idem-user", "idem@example.com")
}

func TestIdempotency_FirstRunThenReplay(t *testing.T) {
	probe := &idemProbe{reply: createdReply}
	rt, fb := idemTestRouter(t, true, probe)

	first := sendIdem(t, rt, fb, "/api/idem-probe", "k-1", `{"name":"a"}`)
	if first.Code != http.StatusCreated || first.Header().Get(idempotentReplayedHeader) != "" {
		t.Fatalf("first: %d replayed=%q %s", first.Code, first.Header().Get(idempotentReplayedHeader), first.Body)
	}
	if first.Header().Get("Location") == "" {
		t.Error("the first response lost the route's own headers")
	}
	for range 2 {
		again := sendIdem(t, rt, fb, "/api/idem-probe", "k-1", `{"name":"a"}`)
		if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() {
			t.Fatalf("replay: %d %s, want 201 %s", again.Code, again.Body, first.Body)
		}
		if again.Header().Get(idempotentReplayedHeader) != "true" || again.Header().Get("Content-Type") != "application/json" {
			t.Errorf("replay headers: replayed %q content type %q", again.Header().Get(idempotentReplayedHeader), again.Header().Get("Content-Type"))
		}
	}
	if n := probe.hits.Load(); n != 1 {
		t.Errorf("the route ran %d times, want once", n)
	}

	// The same key with another body, on another route, or for another target of the same route is not a retry.
	if rr := sendIdem(t, rt, fb, "/api/idem-probe/one", "k-2", `{"name":"a"}`); rr.Code != http.StatusCreated {
		t.Fatalf("create on /one: %d %s", rr.Code, rr.Body)
	}
	for _, tc := range []struct{ path, key, body string }{
		{"/api/idem-probe", "k-1", `{"name":"b"}`},
		{"/api/idem-probe/one", "k-1", `{"name":"a"}`},
		{"/api/idem-probe/two", "k-2", `{"name":"a"}`},
	} {
		rr := sendIdem(t, rt, fb, tc.path, tc.key, tc.body)
		if rr.Code != http.StatusUnprocessableEntity || errorCode(t, rr) != codeIdempotencyKeyReused {
			t.Errorf("%s %s with key %s: %d %s, want 422 %s", tc.path, tc.body, tc.key, rr.Code, rr.Body, codeIdempotencyKeyReused)
		}
	}
	// Without the header, or with a fresh key, the route runs.
	sendIdem(t, rt, fb, "/api/idem-probe", "", `{"name":"a"}`)
	sendIdem(t, rt, fb, "/api/idem-probe", "k-3", `{"name":"a"}`)
	if n := probe.hits.Load(); n != 4 {
		t.Errorf("the route ran %d times, want 4", n)
	}
}

// TestIdempotency_ErrorResponseKeepsNoRecord: a failed create stores nothing; its retry runs the route again.
func TestIdempotency_ErrorResponseKeepsNoRecord(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError} {
		probe := &idemProbe{}
		fail := true
		probe.reply = func(w http.ResponseWriter, r *http.Request) {
			if fail {
				writeJSONStatus(w, status, map[string]string{"code": "NOPE"})
				return
			}
			createdReply(w, r)
		}
		rt, fb := idemTestRouter(t, true, probe)
		if rr := sendIdem(t, rt, fb, "/api/idem-probe", "k", `{}`); rr.Code != status {
			t.Fatalf("failing create: %d, want %d", rr.Code, status)
		}
		fail = false
		rr := sendIdem(t, rt, fb, "/api/idem-probe", "k", `{}`)
		if rr.Code != http.StatusCreated || rr.Header().Get(idempotentReplayedHeader) != "" || probe.hits.Load() != 2 {
			t.Errorf("after a %d: retry %d replayed=%q, route ran %d times; want a fresh run", status, rr.Code, rr.Header().Get(idempotentReplayedHeader), probe.hits.Load())
		}
	}
}

// TestIdempotency_ConcurrentRequestIsRefused: a second request with a key whose first request is still running is
// answered 409 at once; after the first finishes, retries replay it.
func TestIdempotency_ConcurrentRequestIsRefused(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	probe := &idemProbe{}
	probe.reply = func(w http.ResponseWriter, r *http.Request) {
		if probe.hits.Load() == 1 {
			close(started)
			<-release
		}
		createdReply(w, r)
	}
	rt, fb := idemTestRouter(t, true, probe)

	var first *httptest.ResponseRecorder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		first = sendIdem(t, rt, fb, "/api/idem-probe", "k", `{}`)
	}()
	<-started
	rr := sendIdem(t, rt, fb, "/api/idem-probe", "k", `{}`)
	if rr.Code != http.StatusConflict || errorCode(t, rr) != codeIdempotencyInProgress || rr.Header().Get("Retry-After") == "" {
		t.Errorf("concurrent request: %d %s Retry-After=%q, want 409 %s", rr.Code, rr.Body, rr.Header().Get("Retry-After"), codeIdempotencyInProgress)
	}
	close(release)
	wg.Wait()
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	again := sendIdem(t, rt, fb, "/api/idem-probe", "k", `{}`)
	if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() || again.Header().Get(idempotentReplayedHeader) != "true" {
		t.Errorf("retry after the first finished: %d %s", again.Code, again.Body)
	}
	if n := probe.hits.Load(); n != 1 {
		t.Errorf("the route ran %d times, want once", n)
	}
}

func TestIdempotency_InvalidKeyIsRefused(t *testing.T) {
	probe := &idemProbe{reply: createdReply}
	rt, fb := idemTestRouter(t, true, probe)
	for name, keys := range map[string][]string{
		"empty":         {""},
		"too long":      {strings.Repeat("k", maxIdempotencyKeyLen+1)},
		"control":       {"a\tb"},
		"not ascii":     {"schlüssel"},
		"two values":    {"a", "b"},
		"delete":        {"a\x7f"},
		"leading space": {" a"},
	} {
		ctx, end := fb.WithRequestTx(context.Background())
		req := httptest.NewRequest(http.MethodPost, "/api/idem-probe", strings.NewReader(`{}`)).WithContext(ctx)
		req.Header.Set("Authorization", idemBearer(t, rt))
		req.Header[idempotencyKeyHeader] = keys
		rr := httptest.NewRecorder()
		rt.ServeHTTP(rr, req)
		end(false)
		if name == "leading space" {
			// A space is printable: the key is " a" as sent.
			if rr.Code != http.StatusCreated {
				t.Errorf("%s: %d %s, want 201", name, rr.Code, rr.Body)
			}
			continue
		}
		if rr.Code != http.StatusBadRequest || errorCode(t, rr) != codeIdempotencyKeyInvalid {
			t.Errorf("%s: %d %s, want 400 %s", name, rr.Code, rr.Body, codeIdempotencyKeyInvalid)
		}
	}
	if !validIdempotencyKey(strings.Repeat("~", maxIdempotencyKeyLen)) || !validIdempotencyKey("8e03978e-40d5-43e8-bc93-6894a57f9324") {
		t.Error("a 255-character or UUID key is valid")
	}
	if n := probe.hits.Load(); n != 1 {
		t.Errorf("the route ran %d times, want only for the valid key", n)
	}
}

// TestIdempotency_LargeResponseKeepsTheStatusOnly: past the stored cap the response streams in full, and a replay
// answers its status with an empty body.
func TestIdempotency_LargeResponseKeepsTheStatusOnly(t *testing.T) {
	big := strings.Repeat("x", maxStoredResponseBytes+1)
	probe := &idemProbe{reply: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		for i := 0; i < len(big); i += 4096 {
			_, _ = w.Write([]byte(big[i:min(i+4096, len(big))]))
		}
	}}
	rt, fb := idemTestRouter(t, true, probe)
	first := sendIdem(t, rt, fb, "/api/idem-probe", "k", `{}`)
	if first.Code != http.StatusCreated || first.Body.String() != big {
		t.Fatalf("first: %d, %d bytes, want 201 and the whole body", first.Code, first.Body.Len())
	}
	again := sendIdem(t, rt, fb, "/api/idem-probe", "k", `{}`)
	if again.Code != http.StatusCreated || again.Body.Len() != 0 || again.Header().Get(idempotentReplayedHeader) != "true" || again.Header().Get("Content-Type") != "" {
		t.Errorf("replay: %d, %d bytes, content type %q, want 201 with no body", again.Code, again.Body.Len(), again.Header().Get("Content-Type"))
	}
	if n := probe.hits.Load(); n != 1 {
		t.Errorf("the route ran %d times, want once", n)
	}
}

// TestIdempotency_HeaderIgnoredWithoutTheMechanism: a route that does not declare Idempotent, a desktop without the
// request transaction, and a request the store cannot claim a key for (no request transaction on the context) all
// run the route every time. The first two never look at the key; the third, on a deployment that offers keys, still
// refuses a malformed one before it learns there is no transaction.
func TestIdempotency_HeaderIgnoredWithoutTheMechanism(t *testing.T) {
	send := func(rt *Router, fb *testutil.FakeBackend, path, key string, inTx bool) *httptest.ResponseRecorder {
		ctx := context.Background()
		end := func(bool) {}
		if inTx {
			ctx, end = fb.WithRequestTx(ctx)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)).WithContext(ctx)
		req.Header.Set("Authorization", idemBearer(t, rt))
		req.Header.Set(idempotencyKeyHeader, key)
		rr := httptest.NewRecorder()
		rt.ServeHTTP(rr, req)
		end(rr.Code < http.StatusBadRequest)
		return rr
	}
	for _, tc := range []struct {
		name       string
		jwt        bool
		path       string
		inTx       bool
		keyChecked bool
	}{
		{"route not declared idempotent", true, "/api/idem-plain", true, false},
		{"desktop (no accounts, no request transaction)", false, "/api/idem-probe", true, false},
		{"no request transaction on the context", true, "/api/idem-probe", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &idemProbe{reply: createdReply}
			rt, fb := idemTestRouter(t, tc.jwt, probe)
			for _, key := range []string{"k", "k"} {
				rr := send(rt, fb, tc.path, key, tc.inTx)
				if rr.Code != http.StatusCreated || rr.Header().Get(idempotentReplayedHeader) != "" {
					t.Errorf("key %q: %d replayed=%q %s, want the route's own 201", key, rr.Code, rr.Header().Get(idempotentReplayedHeader), rr.Body)
				}
			}
			bad := send(rt, fb, tc.path, "bad\tkey", tc.inTx)
			if refused := bad.Code == http.StatusBadRequest; refused != tc.keyChecked {
				t.Errorf("malformed key: %d %s, refused %v, want %v", bad.Code, bad.Body, refused, tc.keyChecked)
			}
			want := int32(3)
			if tc.keyChecked {
				want = 2
			}
			if n := probe.hits.Load(); n != want {
				t.Errorf("the route ran %d times, want %d", n, want)
			}
		})
	}
}

// failingCompleteStore is the fake whose CompleteIdempotencyKey fails, as a lost connection would.
type failingCompleteStore struct {
	*testutil.FakeBackend
	err error
}

func (f failingCompleteStore) CompleteIdempotencyKey(context.Context, string, string, storageif.IdempotentResponse) error {
	return f.err
}

// TestIdempotency_UnstoredResponseIsAnError: when the response cannot be stored with its key, the client gets an
// error instead of the created record (the error rolls the create back), so a success always has its record.
func TestIdempotency_UnstoredResponseIsAnError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{errors.New("connection reset"), http.StatusInternalServerError},
		{storageif.ErrNotFound, http.StatusInternalServerError}, // no claim held: a server fault, not a missing resource
		{storageif.ErrRLSUnavailable, http.StatusServiceUnavailable},
	} {
		fb := testutil.NewFakeBackend()
		rt := newTestRouter(failingCompleteStore{FakeBackend: fb, err: tc.err}, true)
		probe := &idemProbe{reply: createdReply}
		mountTestRoutes(t, rt, func(rb *route.Builder) { rb.Post("/api/idem-probe", probe.ServeHTTP, route.Idempotent()) })
		rr := sendIdem(t, rt, fb, "/api/idem-probe", "k", `{}`)
		if rr.Code != tc.want || strings.Contains(rr.Body.String(), `"id"`) || rr.Header().Get("Location") != "" {
			t.Errorf("%v: %d %s Location=%q, want %d without the route's response", tc.err, rr.Code, rr.Body, rr.Header().Get("Location"), tc.want)
		}
	}
}

// TestIdempotency_RefusedWhereItCannotBeHonoured: the router does not start with Idempotent on a route without the
// request transaction, without authentication, or on a read.
func TestIdempotency_RefusedWhereItCannotBeHonoured(t *testing.T) {
	noop := func(http.ResponseWriter, *http.Request) {}
	for _, tc := range []struct {
		name, method string
		opts         []route.Option
		want         string
	}{
		{"without the request transaction", http.MethodPost, []route.Option{route.Idempotent(), route.NoTx()}, "without the request transaction"},
		{"public", http.MethodPost, []route.Option{route.Idempotent(), route.Public()}, "not authenticated"},
		{"self-authenticated", http.MethodPost, []route.Option{route.Idempotent(), route.SelfAuthenticated()}, "not authenticated"},
		{"a read", http.MethodGet, []route.Option{route.Idempotent()}, "a read needs no Idempotency-Key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newTestRouter(testutil.NewFakeBackend(), true)
			rb := route.NewBuilder(rt.mux)
			rb.Method(tc.method, "/api/idem-refused", http.HandlerFunc(noop), tc.opts...)
			tbl, err := rb.Build()
			if err != nil {
				t.Fatal(err)
			}
			rt.routes = tbl
			err = rt.finishRoutes(rb)
			if err == nil || !strings.Contains(err.Error(), "/api/idem-refused") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("finishRoutes = %v, want a refusal of /api/idem-refused (%s)", err, tc.want)
			}
		})
	}
}

// TestIdempotency_CreateRoutesDeclareIt pins the create routes that honour Idempotency-Key.
func TestIdempotency_CreateRoutesDeclareIt(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)
	want := map[string]bool{
		"POST /api/library":                                       true,
		"POST /api/library/{id}/versions":                         true,
		"POST /api/orgs":                                          true,
		"POST /api/orgs/{id}/invites":                             true,
		"POST /api/orgs/{id}/channels":                            true,
		"POST /api/orgs/{id}/rules":                               true,
		"POST /api/orgs/{id}/providers/azure-foundry/connections": true,
		"POST /api/orgs/{id}/providers/azure-foundry/models":      true,
		"POST /api/auth/tokens":                                   true,
		"POST /api/analysis/comments/add":                         true,
		"POST /api/flow/share/create":                             true,
		"POST /api/flows/{flowId}/collaborators":                  true,
		"POST /api/providers/azure-foundry/connections":           true,
		"POST /api/providers/azure-foundry/models":                true,
	}
	for _, p := range rt.routes.Policies() {
		name := p.Method + " " + p.Pattern
		if p.Idempotent != want[name] {
			t.Errorf("%s: Idempotent %v, want %v", name, p.Idempotent, want[name])
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("%s: no such route", name)
	}
}
