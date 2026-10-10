// Package route declares, beside each HTTP route, every cross-cutting decision the middleware makes for a request
// to it: who may call it, the scopes a scoped API token needs, the rate-limit bucket, the per-request RLS
// transaction, the deadline class, the capabilities it needs and its metrics label.
//
// Modules register routes through a Builder. Build walks the finished chi tree, so each policy is keyed by the
// pattern chi itself reports, and it refuses an endpoint registered around the Builder: a route without a
// declared policy does not boot (deny by default). The router resolves each request to its Policy once, at the
// outermost layer, and the middleware reads that Policy instead of keeping path tables of its own
// (docs/architecture/backend-refactoring-proposal.md, Phase 1a).
package route

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
)

// Access says who may call a route.
type Access uint8

const (
	// AuthRequired routes need a session or an API token; jwtAuth refuses anyone else. The default.
	AuthRequired Access = iota
	// AuthNone routes run without authentication: sign-in and recovery, health probes, the share viewer.
	AuthNone
	// AuthSelf routes check the caller themselves (a WebSocket ticket, the metrics token, the CI webhook's HMAC,
	// the Swagger UI's bearer check). jwtAuth leaves them alone, as it does AuthNone routes.
	AuthSelf
)

func (a Access) String() string {
	switch a {
	case AuthNone:
		return "none"
	case AuthSelf:
		return "self"
	}
	return "jwt"
}

// Tx says whether a request runs inside the per-request RLS transaction.
type Tx uint8

const (
	// TxRequest: an authenticated request runs inside the per-request RLS transaction, lazily begun for reads and
	// begun before the handler for writes. The default.
	TxRequest Tx = iota
	// TxNone: no request transaction. Routes that stream, burn CPU or dial out for seconds opt out so they do not
	// pin a pooled connection meanwhile; Go-layer authorization and explicit owner predicates still protect them.
	TxNone
)

func (t Tx) String() string {
	if t == TxNone {
		return "none"
	}
	return "tx"
}

// Deadline is a request's deadline class.
type Deadline uint8

const (
	// DeadlineDefault is the configured request timeout.
	DeadlineDefault Deadline = iota
	// DeadlineNone: WebSocket and SSE sessions, which the client ends.
	DeadlineNone
	// DeadlineStream: model streams (middleware.StreamRequestTimeout).
	DeadlineStream
	// DeadlineBulk: the knowledge bulk routes (middleware.BulkRequestTimeout).
	DeadlineBulk
)

func (d Deadline) String() string {
	switch d {
	case DeadlineNone:
		return "none"
	case DeadlineStream:
		return "stream"
	case DeadlineBulk:
		return "bulk"
	}
	return "default"
}

// Rate-limit buckets. The names double as the limiters' metric labels, so they must not change.
const (
	RateGeneral  = "general"
	RateAuth     = "auth"
	RateAnalysis = "analysis"
	RateChat     = "chat"
	RateUpload   = "upload"
	// RateChannelTest: the notification channel test, which makes the server POST to an admin-chosen URL.
	RateChannelTest = "channel_test"
	// RateNone: not rate limited (content-hashed, immutable assets). An empty RateLimit means RateGeneral, so a
	// policy built by hand fails toward the limiter.
	RateNone = "none"
)

// Kind tells a route's policy from the policies of requests that match no route.
type Kind uint8

const (
	// KindEndpoint: a registered route.
	KindEndpoint Kind = iota
	// KindMethodNotAllowed: the path matches a route, the method none of its registrations (a 405, and every
	// OPTIONS preflight, which corsHeaders answers).
	KindMethodNotAllowed
	// KindNoRoute: nothing matches (a 404, and the static files and SPA shell the NotFound handler serves).
	KindNoRoute
)

func (k Kind) String() string {
	switch k {
	case KindMethodNotAllowed:
		return "method-not-allowed"
	case KindNoRoute:
		return "no-route"
	}
	return "endpoint"
}

// Policy is everything the middleware decides for one route. Policies are built at startup and shared by every
// request; never modify one after Build.
type Policy struct {
	Kind Kind
	// Method is the registered method; "" for KindMethodNotAllowed and KindNoRoute.
	Method string
	// Pattern is the chi pattern as RoutePattern reports it, e.g. /api/library/{id}; "" for KindNoRoute.
	Pattern string

	Access Access
	// Scopes are the scopes a scoped API token needs, all of them. Build fills in the method's default (read for
	// GET and HEAD, write otherwise) when an AuthRequired route declares none and does not refuse tokens.
	Scopes []string
	// TokensDenied: scoped API tokens may not call the route whatever their scopes (credential and identity
	// management). Sessions and unscoped tokens are unaffected.
	TokensDenied bool
	// RateLimit is one of the Rate* buckets.
	RateLimit string
	Tx        Tx
	Deadline  Deadline
	// Needs are the capabilities the deployment must have; without them the route answers 503
	// SERVICE_UNAVAILABLE before its handler runs.
	Needs []capability.Capability
	// Idempotent: a request with an Idempotency-Key header runs once per user and key, and its retries get the
	// first response back (ADR 0007).
	Idempotent bool
	// Label is the metrics route label: the pattern for a route, a bounded catch-all otherwise.
	Label string
	// Allowed lists the methods registered on the path of a KindMethodNotAllowed policy (the Allow header).
	Allowed []string
}

// AllowsToken reports whether a scoped API token holding scopes may call the route.
func (p *Policy) AllowsToken(scopes []string) bool {
	if p.TokensDenied {
		return false
	}
	for _, s := range p.Scopes {
		if !auth.HasScope(scopes, s) {
			return false
		}
	}
	return true
}

// Option adjusts a route's policy. Options given to Route and Group apply to every route inside, before the
// route's own.
type Option func(*Policy)

// Public marks a route that runs without authentication.
func Public() Option { return func(p *Policy) { p.Access = AuthNone } }

// SelfAuthenticated marks a route that authenticates the caller itself; jwtAuth leaves it alone.
func SelfAuthenticated() Option { return func(p *Policy) { p.Access = AuthSelf } }

// Scopes sets the scopes a scoped API token needs, all of them, replacing any inherited ones.
func Scopes(scopes ...string) Option {
	return func(p *Policy) {
		p.Scopes = append([]string(nil), scopes...)
		p.TokensDenied = false
	}
}

// DenyTokens refuses scoped API tokens whatever their scopes.
func DenyTokens() Option {
	return func(p *Policy) {
		p.Scopes = nil
		p.TokensDenied = true
	}
}

// RateLimit charges the route to a rate-limit bucket.
func RateLimit(bucket string) Option { return func(p *Policy) { p.RateLimit = bucket } }

// NoTx runs the route outside the per-request RLS transaction.
func NoTx() Option { return func(p *Policy) { p.Tx = TxNone } }

// LongLived gives the route no deadline (WebSocket, SSE).
func LongLived() Option { return func(p *Policy) { p.Deadline = DeadlineNone } }

// Streaming gives the route the model-stream deadline.
func Streaming() Option { return func(p *Policy) { p.Deadline = DeadlineStream } }

// Bulk gives the route the bulk-work deadline.
func Bulk() Option { return func(p *Policy) { p.Deadline = DeadlineBulk } }

// Needs adds capabilities the deployment must have for the route to run.
func Needs(caps ...capability.Capability) Option {
	return func(p *Policy) { p.Needs = append(append([]capability.Capability(nil), p.Needs...), caps...) }
}

// Idempotent lets clients retry the route with an Idempotency-Key header: a retry gets the first response back
// instead of creating a second record. The route must be authenticated and run in the request transaction, or the
// router refuses to start.
func Idempotent() Option { return func(p *Policy) { p.Idempotent = true } }

// Handler is a registered endpoint: the route's handler with its policy attached. Build finds every endpoint's
// policy through it, and it is where a request's resolved policy is checked against the route chi dispatched to.
type Handler struct {
	Policy *Policy
	Next   http.Handler
	reg    *registry
}

// ServeHTTP runs the guard set on the Builder (SetGuard), then the route's handler unless the guard answered.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.reg != nil {
		if g := h.reg.guard.Load(); g != nil && !(*g)(w, r, h.Policy) {
			return
		}
	}
	h.Next.ServeHTTP(w, r)
}

// Guard checks a request against the policy of the endpoint chi dispatched it to, before the handler runs. It
// returns false when it has answered the request itself.
type Guard func(w http.ResponseWriter, r *http.Request, p *Policy) bool

// anyMethods are the methods Handle registers: chi's own set for Handle.
var anyMethods = []string{
	http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodOptions,
	http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace,
}

// Builder registers routes with their policies on a chi router. Modules receive one in Mount.
type Builder struct {
	mux      chi.Router
	defaults []Option
	mws      []func(http.Handler) http.Handler
	reg      *registry
}

type registry struct {
	root  *chi.Mux
	subs  map[subKey]*chi.Mux
	guard atomic.Pointer[Guard]
}

type subKey struct {
	parent chi.Router
	prefix string
}

// NewBuilder returns the root Builder for mux. Register the router's own middleware (Use) on mux first: chi
// refuses middleware added after routes.
func NewBuilder(mux *chi.Mux) *Builder {
	return &Builder{mux: mux, reg: &registry{root: mux, subs: map[subKey]*chi.Mux{}}}
}

// Get registers h for GET.
func (b *Builder) Get(pattern string, h http.HandlerFunc, opts ...Option) {
	b.Method(http.MethodGet, pattern, h, opts...)
}

// Post registers h for POST.
func (b *Builder) Post(pattern string, h http.HandlerFunc, opts ...Option) {
	b.Method(http.MethodPost, pattern, h, opts...)
}

// Put registers h for PUT.
func (b *Builder) Put(pattern string, h http.HandlerFunc, opts ...Option) {
	b.Method(http.MethodPut, pattern, h, opts...)
}

// Patch registers h for PATCH.
func (b *Builder) Patch(pattern string, h http.HandlerFunc, opts ...Option) {
	b.Method(http.MethodPatch, pattern, h, opts...)
}

// Delete registers h for DELETE.
func (b *Builder) Delete(pattern string, h http.HandlerFunc, opts ...Option) {
	b.Method(http.MethodDelete, pattern, h, opts...)
}

// Handle registers h for every method chi knows (chi's Handle), each with a policy of its own.
func (b *Builder) Handle(pattern string, h http.Handler, opts ...Option) {
	for _, m := range anyMethods {
		b.Method(m, pattern, h, opts...)
	}
}

// Method registers h for one method.
func (b *Builder) Method(method, pattern string, h http.Handler, opts ...Option) {
	p := &Policy{Kind: KindEndpoint, Method: method, RateLimit: RateGeneral}
	for _, o := range b.defaults {
		o(p)
	}
	for _, o := range opts {
		o(p)
	}
	if p.Access != AuthRequired {
		// Nothing to scope: jwtAuth and the RLS middleware leave these routes alone.
		p.Scopes, p.TokensDenied, p.Tx = nil, false, TxNone
	} else if len(p.Scopes) == 0 && !p.TokensDenied {
		p.Scopes = []string{defaultScope(method)}
	}
	next := h
	for _, mw := range slices.Backward(b.mws) {
		next = mw(next)
	}
	b.mux.Method(method, pattern, &Handler{Policy: p, Next: next, reg: b.reg})
}

// SetGuard installs g on every endpoint registered through this Builder or any Builder derived from it, including
// those already registered; it runs at request time, before the route's handler.
func (b *Builder) SetGuard(g func(w http.ResponseWriter, r *http.Request, p *Policy) bool) {
	guard := Guard(g)
	b.reg.guard.Store(&guard)
}

func defaultScope(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return auth.ScopeRead
	}
	return auth.ScopeWrite
}

// Route mounts a subrouter at prefix and calls fn with a Builder for it, as chi's Route does. Another Route call
// with the same prefix on the same Builder level adds to the same subrouter, so modules can share a prefix such
// as /api/admin. opts apply to every route registered inside, before the route's own.
func (b *Builder) Route(prefix string, fn func(*Builder), opts ...Option) {
	key := subKey{b.mux, prefix}
	sub, ok := b.reg.subs[key]
	if !ok {
		sub = chi.NewRouter()
		b.mux.Mount(prefix, sub)
		b.reg.subs[key] = sub
	}
	fn(b.child(sub, opts, nil))
}

// Group calls fn with a Builder whose routes share opts, without a path prefix.
func (b *Builder) Group(fn func(*Builder), opts ...Option) {
	fn(b.child(b.mux, opts, nil))
}

// With returns a Builder whose routes run mw, in order, right before their handler: after the router's
// middleware and after the policy check on the endpoint.
func (b *Builder) With(mw ...func(http.Handler) http.Handler) *Builder {
	return b.child(b.mux, nil, mw)
}

func (b *Builder) child(mux chi.Router, opts []Option, mws []func(http.Handler) http.Handler) *Builder {
	return &Builder{
		mux:      mux,
		defaults: append(append([]Option(nil), b.defaults...), opts...),
		mws:      append(append([]func(http.Handler) http.Handler(nil), b.mws...), mws...),
		reg:      b.reg,
	}
}

// Build finishes registration and returns the policy table. It walks the router, so every policy is keyed by the
// pattern chi reports for it, and fails when an endpoint was registered around the Builder (deny by default) or
// when two registrations share a method and pattern.
func (b *Builder) Build() (*Table, error) {
	t := &Table{byMethod: map[string]map[string]*Policy{}}
	err := chi.Walk(b.reg.root, func(method, pattern string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
		e, ok := h.(*Handler)
		if !ok {
			return fmt.Errorf("route: %s %s has no policy: register it through route.Builder", method, pattern)
		}
		p := e.Policy
		norm := NormalizePattern(pattern)
		if p.Method != method || (p.Pattern != "" && p.Pattern != norm) {
			return fmt.Errorf("route: one endpoint registered as %s %s and %s %s", p.Method, p.Pattern, method, norm)
		}
		p.Pattern = norm
		if p.Label == "" {
			p.Label = norm
		}
		byPattern := t.byMethod[method]
		if byPattern == nil {
			byPattern = map[string]*Policy{}
			t.byMethod[method] = byPattern
		}
		if _, dup := byPattern[norm]; dup {
			return fmt.Errorf("route: %s %s registered twice", method, norm)
		}
		byPattern[norm] = p
		t.all = append(t.all, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(t.all, func(i, j int) bool {
		if t.all[i].Pattern != t.all[j].Pattern {
			return t.all[i].Pattern < t.all[j].Pattern
		}
		return t.all[i].Method < t.all[j].Method
	})
	return t, nil
}

// NormalizePattern returns pattern as chi's RoutePattern reports it at dispatch: a subrouter's own "/" route
// loses its trailing slash (/api/library/ → /api/library), and chi's Find, which stops at a mount's stub for the
// subrouter root, reports the same spelling.
func NormalizePattern(pattern string) string {
	for strings.Contains(pattern, "/*/") {
		pattern = strings.ReplaceAll(pattern, "/*/", "/")
	}
	if pattern != "/" {
		pattern = strings.TrimSuffix(pattern, "//")
		pattern = strings.TrimSuffix(pattern, "/")
	}
	return pattern
}

// Table is the policies of one router by method and pattern.
type Table struct {
	byMethod map[string]map[string]*Policy // keyed apart: the resolver looks up every request, without a key to build
	all      []*Policy
}

// Lookup returns the policy registered for method and pattern (as chi's Find or RoutePattern report it), or nil.
func (t *Table) Lookup(method, pattern string) *Policy {
	return t.byMethod[method][NormalizePattern(pattern)]
}

// Policies returns every route's policy, sorted by pattern, then method.
func (t *Table) Policies() []*Policy { return t.all }
