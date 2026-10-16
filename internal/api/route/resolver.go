package route

import (
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// probeMethods are the methods chi routes. A request whose method has no route on its path probes the others to
// tell a 405 from a 404; the Allow list of a 405 policy follows this order.
var probeMethods = [...]string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
	http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace,
}

func routedMethod(method string) bool {
	for _, m := range probeMethods {
		if m == method {
			return true
		}
	}
	return false
}

// Labels of the requests that match no route: bounded, whatever the path.
const (
	labelAPIOther = "/api/other"
	labelStatic   = "/static/*"
	labelRoot     = "/"
)

// Unresolved stands in for the policy of a request no resolver has seen: a layer used outside the router's chain.
// It fails closed: authentication required, scoped tokens refused, the request transaction, the default deadline.
var Unresolved = &Policy{Kind: KindNoRoute, Access: AuthRequired, TokensDenied: true, RateLimit: RateGeneral, Label: "unresolved"}

// noRoutePolicy is the policy of requests that match no route, for one kind of path, and its twin for a method
// chi does not route at all: chi answers those with its 405 handler, whatever the path.
type noRoutePolicy struct{ notFound, unknownMethod *Policy }

func newNoRoute(access Access, bucket, label string) noRoutePolicy {
	p := &Policy{Kind: KindNoRoute, Access: access, RateLimit: bucket, Tx: TxNone, Label: label}
	twin := *p
	twin.Kind = KindMethodNotAllowed
	return noRoutePolicy{p, &twin}
}

var (
	noRouteAPI       = newNoRoute(AuthRequired, RateGeneral, labelAPIOther)
	noRouteAPIShared = newNoRoute(AuthRequired, RateAnalysis, labelAPIOther)
	noRouteRoot      = newNoRoute(AuthNone, RateGeneral, labelRoot)
	noRouteStatic    = newNoRoute(AuthNone, RateGeneral, labelStatic)
	noRouteShared    = newNoRoute(AuthNone, RateAnalysis, labelStatic)
	noRouteAsset     = newNoRoute(AuthNone, RateNone, labelStatic)
)

// noRoute picks the policy of a request that matches no route, from the path chi dispatches on and the decoded
// path the NotFound handler serves. Under /api/ it authenticates, so probing /api/anything anonymously is a 401.
// The share viewer's shell (/shared) resolves a share per hit and rides the analysis bucket, judged on either
// path, since NotFound serves the decoded one (/x%2F..%2Fshared included). Content-hashed assets are not rate
// limited, but only when both paths are under /assets/: an escape that leaves it is served from elsewhere.
func noRoute(method, dispatch, served string) noRoutePolicy {
	api := strings.HasPrefix(dispatch, "/api/")
	shared := dispatch == "/shared" || served == "/shared"
	switch {
	case api && shared:
		return noRouteAPIShared
	case api:
		return noRouteAPI
	case shared:
		return noRouteShared
	case (method == http.MethodGet || method == http.MethodHead) &&
		strings.HasPrefix(dispatch, "/assets/") && strings.HasPrefix(served, "/assets/"):
		return noRouteAsset
	case dispatch == labelRoot:
		return noRouteRoot
	}
	return noRouteStatic
}

// Resolver resolves a request to its policy: the policy of the route chi dispatches it to, the policy of a method
// its path does not register (a 405), or the policy of a path no route matches (a 404). It reads the router's
// tree with mux.Find on a context of its own, so it can run before the router, outside the mux, and it never
// touches the request.
type Resolver struct {
	mux   *chi.Mux
	table *Table
	ctxs  sync.Pool // *chi.Context for Find

	mu  sync.RWMutex
	mna map[mnaKey]*Policy
}

// mnaKey identifies a 405 policy: what the other methods resolve to on the path, and the request's kind.
type mnaKey struct {
	probed    [len(probeMethods)]*Policy
	preflight bool // OPTIONS: a CORS preflight
	unknown   bool // a method chi does not route
}

// NewResolver returns the resolver of mux, whose routes' policies t holds.
func NewResolver(mux *chi.Mux, t *Table) *Resolver {
	r := &Resolver{mux: mux, table: t, mna: map[mnaKey]*Policy{}}
	r.ctxs.New = func() any { return chi.NewRouteContext() }
	return r
}

// Resolve returns the policy of a request for method and u as received, before the router cleans the path and
// strips the /api/v1 alias; the same request resolves the same after Router.ServeHTTP's path.Clean. The result
// is a table policy, a memoized 405 policy or a no-route singleton, so pointers compare: the endpoint guard
// checks that chi dispatched to the route whose policy the middleware applied.
func (r *Resolver) Resolve(method string, u *url.URL) *Policy {
	dispatch := DispatchPath(u)
	routed := routedMethod(method)
	if routed {
		if p := r.find(method, dispatch); p != nil {
			return p
		}
	}
	key := mnaKey{preflight: method == http.MethodOptions, unknown: !routed}
	found := false
	for i, m := range probeMethods {
		if m == method {
			continue
		}
		if p := r.find(m, dispatch); p != nil {
			key.probed[i] = p
			found = true
		}
	}
	if found {
		return r.methodNotAllowed(key)
	}
	nr := noRoute(method, dispatch, CanonicalPath(u.Path))
	if !routed {
		return nr.unknownMethod
	}
	return nr.notFound
}

// find returns the policy of the route chi dispatches method and path to, or nil. A mount's stub answers every
// method, so a method the subrouter does not register finds the stub's pattern but no policy.
func (r *Resolver) find(method, path string) *Policy {
	rctx := r.ctxs.Get().(*chi.Context)
	rctx.Reset()
	pattern := r.mux.Find(rctx, method, path)
	r.ctxs.Put(rctx)
	if pattern == "" {
		return nil
	}
	return r.table.Lookup(method, pattern)
}

func (r *Resolver) methodNotAllowed(k mnaKey) *Policy {
	r.mu.RLock()
	p, ok := r.mna[k]
	r.mu.RUnlock()
	if ok {
		return p
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.mna[k]; ok {
		return p
	}
	p = newMethodNotAllowed(k)
	r.mna[k] = p
	return p
}

// newMethodNotAllowed builds the policy of a request whose path routes only other methods (k.probed). It is
// public only when every route on the path is, and it is charged to the strictest of their buckets, so changing
// the method buys no looser bucket; a CORS preflight stays on general unless that is the auth bucket, or clients
// would pay chat or analysis budget per preflight. Nothing runs, so there is no transaction and no scope to hold.
// The label and pattern are the first probed route's; Allow lists the probed methods, except for a method chi
// does not route, which its 405 handler answers without one.
func newMethodNotAllowed(k mnaKey) *Policy {
	p := &Policy{Kind: KindMethodNotAllowed, Access: AuthNone, Tx: TxNone}
	bucket := RateNone
	for i, q := range k.probed {
		if q == nil {
			continue
		}
		if p.Pattern == "" {
			p.Pattern, p.Label = q.Pattern, q.Label
		}
		if q.Access == AuthRequired {
			p.Access = AuthRequired
		}
		if bucketRank(q.RateLimit) > bucketRank(bucket) {
			bucket = q.RateLimit
		}
		if !k.unknown {
			p.Allowed = append(p.Allowed, probeMethods[i])
		}
	}
	if k.preflight && bucket != RateAuth {
		bucket = RateGeneral
	}
	p.RateLimit = bucket
	return p
}

// bucketRank orders the buckets from the loosest to the strictest.
func bucketRank(bucket string) int {
	switch bucket {
	case RateNone:
		return 0
	case RateUpload, RateAvatar:
		return 2
	case RateAnalysis:
		return 3
	case RateChat:
		return 4
	case RateChannelTest:
		return 5
	case RateAuth:
		return 6
	}
	return 1 // general, and "" (general)
}

// SamplePath returns a path that pattern matches: each {param} and the trailing catch-all become "x". The boot
// self-test resolves it to check that the route resolves to its own policy.
func SamplePath(pattern string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case c == '{':
			if depth == 0 {
				b.WriteByte('x')
			}
			depth++
		case c == '}' && depth > 0:
			depth--
		case depth > 0:
		case c == '*' && i == len(pattern)-1:
			b.WriteByte('x')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
