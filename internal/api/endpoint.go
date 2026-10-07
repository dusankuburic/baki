package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Typed endpoints (docs/architecture/api-conventions.md). JSON and Query build a route's handler from a function
// that takes a typed request and returns a typed response. The function runs on the request's context, so it
// sees the request's actor, RLS transaction and reqinfo. Its errors go through render.Error: an *apperr.Error
// keeps its kind and code, and a service sentinel maps as it always has. New endpoints are built this way, and
// hand-written handlers move over when they are touched (internal/archtest pins the rest).

// Validator is a request that checks itself once decoded. A plain error from Validate is a 400 BAD_REQUEST with
// its message; an *apperr.Error is written as it is.
type Validator interface {
	Validate() error
}

// FlowRequest is a request that names the flow it acts on. Its endpoint must authorize that flow with
// AuthorizeFlow, and only such an endpoint may use AuthorizeFlow.
type FlowRequest interface {
	FlowRef() string
}

// EndpointOption configures an endpoint built by JSON or Query.
type EndpointOption func(*endpointSpec)

// Status sets the status of a successful response: 200 by default, 201 for a create, 204 for a response without
// a body.
func Status(code int) EndpointOption {
	return func(e *endpointSpec) { e.status = code }
}

// PageLimits sets the page sizes of a request that embeds PageRequest: defaultLimit when it asks for none, at most
// maxLimit. Without this option they are DefaultPageLimit and MaxPageLimit.
func PageLimits(defaultLimit, maxLimit int) EndpointOption {
	return func(e *endpointSpec) { e.pageDefault, e.pageMax, e.pageLimitsSet = defaultLimit, maxLimit, true }
}

// AuthorizeFlow authorizes the flow the request names (FlowRequest) before the endpoint runs. It calls
// AuthzService.AuthorizeFlow for the request's actor and action, so a missing flow is a 404 and a refusal is a 403,
// including a scoped token that lacks the action's scope. The endpoint reads the stored header the check loaded
// with AuthorizedFlow. Without accounts (local mode) there is one local user, whom the flow checks have always let
// through: nothing is checked and AuthorizedFlow returns nil.
func AuthorizeFlow(sec *SecurityConfig, authz *service.AuthzService, action service.Action) EndpointOption {
	return func(e *endpointSpec) { e.flow = &endpointFlowCheck{sec: sec, authz: authz, action: action} }
}

// AuthorizedFlow returns the stored header of the flow AuthorizeFlow authorized for the request ctx belongs to. It
// is nil without AuthorizeFlow, and in local mode.
func AuthorizedFlow(ctx context.Context) *storageif.FlowDocument {
	h, _ := ctx.Value(authorizedFlowKey{}).(*storageif.FlowDocument)
	return h
}

type authorizedFlowKey struct{}

type endpointFlowCheck struct {
	sec    *SecurityConfig
	authz  *service.AuthzService
	action service.Action
}

// authorize checks the request's actor against the flow and returns ctx carrying the flow's header.
func (c *endpointFlowCheck) authorize(ctx context.Context, flowID string) (context.Context, error) {
	if !c.sec.JWTEnabled {
		return ctx, nil
	}
	h, err := c.authz.AuthorizeFlow(ctx, auth.ActorFromContext(ctx), c.action, flowID)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, authorizedFlowKey{}, h), nil
}

// JSON builds the handler of an endpoint that takes a JSON body. The body is decoded strictly into Req: an unknown
// field, an empty body or data after the value is a 400 BAD_REQUEST, and so is a body over the router's size
// limit, as decodeBody has always answered it. Fields tagged path:"name" are then filled from the route's
// parameters and fields tagged query:"name" from the query string. Validate, AuthorizeFlow and fn run in that
// order, and fn's Resp is written as JSON with the endpoint's Status. Every error goes through render.Error.
//
// JSON panics when Req and the options do not fit, for instance a FlowRequest without AuthorizeFlow. Routes are
// mounted at boot, so such an endpoint never serves.
func JSON[Req, Resp any](fn func(ctx context.Context, req Req) (Resp, error), opts ...EndpointOption) http.HandlerFunc {
	e := newEndpointSpec[Req](opts)
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if err := decodeStrictJSON(r.Body, &req); err != nil {
			render.Error(w, err, 0)
			return
		}
		serveEndpoint(e, w, r, &req, fn)
	}
}

// Query builds the handler of an endpoint without a body (a GET, a DELETE). Req's fields tagged query:"name" are
// filled from the query string and those tagged path:"name" from the route's parameters; the rest is as for JSON.
// Req must be a struct.
func Query[Req, Resp any](fn func(ctx context.Context, req Req) (Resp, error), opts ...EndpointOption) http.HandlerFunc {
	if t := reflect.TypeFor[Req](); t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("api: Query needs a struct request, not %s", t))
	}
	e := newEndpointSpec[Req](opts)
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		serveEndpoint(e, w, r, &req, fn)
	}
}

// endpointSpec is what JSON and Query settle when they build the handler: the options and Req's URL parameters.
type endpointSpec struct {
	status        int
	pageDefault   int
	pageMax       int
	pageLimitsSet bool
	// paged: Req embeds PageRequest.
	paged  bool
	flow   *endpointFlowCheck
	params []endpointParam
}

var pageRequestType = reflect.TypeFor[PageRequest]()

// newEndpointSpec applies opts and checks that they fit Req, panicking when they do not.
func newEndpointSpec[Req any](opts []EndpointOption) *endpointSpec {
	e := &endpointSpec{status: http.StatusOK, pageDefault: DefaultPageLimit, pageMax: MaxPageLimit}
	for _, o := range opts {
		o(e)
	}
	t := reflect.TypeFor[Req]()
	fail := func(problem string) {
		panic(fmt.Sprintf("api: endpoint taking %s: %s", t, problem))
	}
	if t.Kind() == reflect.Pointer {
		fail("the request must be a value type")
	}
	if t.Kind() == reflect.Struct {
		e.params = endpointParams(t, nil)
	}
	if e.status < 200 || e.status > 299 {
		fail(fmt.Sprintf("success status %d", e.status))
	}

	_, namesFlow := any(new(Req)).(FlowRequest)
	switch {
	case namesFlow && e.flow == nil:
		fail("the request names a flow (FlowRef) that the endpoint does not authorize (AuthorizeFlow)")
	case e.flow != nil && !namesFlow:
		fail("AuthorizeFlow needs a request that names its flow (FlowRef)")
	case e.flow != nil && (e.flow.sec == nil || e.flow.authz == nil):
		fail("AuthorizeFlow needs the SecurityConfig and the AuthzService")
	}

	_, e.paged = any(new(Req)).(pageRequester)
	if e.paged && t != pageRequestType {
		if f, ok := t.FieldByName("PageRequest"); !ok || f.Type != pageRequestType {
			fail("embed PageRequest by value")
		}
	}
	switch {
	case e.pageLimitsSet && !e.paged:
		fail("PageLimits needs a request that embeds PageRequest")
	case e.pageDefault < 1 || e.pageMax < e.pageDefault:
		fail(fmt.Sprintf("page limits %d (default) and %d (maximum)", e.pageDefault, e.pageMax))
	}
	return e
}

// serveEndpoint takes a decoded request through the rest of the endpoint (URL parameters, page bounds, Validate,
// AuthorizeFlow, fn) and writes fn's answer.
func serveEndpoint[Req, Resp any](e *endpointSpec, w http.ResponseWriter, r *http.Request, req *Req, fn func(context.Context, Req) (Resp, error)) {
	if err := e.decodeParams(r, req); err != nil {
		render.Error(w, err, 0)
		return
	}
	if e.paged {
		any(req).(pageRequester).pageRequest().bound(e.pageDefault, e.pageMax)
	}
	if v, ok := any(req).(Validator); ok {
		if err := v.Validate(); err != nil {
			if _, typed := apperr.As(err); !typed {
				err = apperr.Invalid("", err.Error()).Wrap(err)
			}
			render.Error(w, err, 0)
			return
		}
	}
	ctx := r.Context()
	if e.flow != nil {
		var err error
		if ctx, err = e.flow.authorize(ctx, any(req).(FlowRequest).FlowRef()); err != nil {
			render.Error(w, err, 0)
			return
		}
	}
	resp, err := fn(ctx, *req)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	if e.status == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	render.JSONStatus(w, e.status, resp)
}

// decodeStrictJSON decodes body into dst strictly: an unknown field, an empty body or data after the value is
// refused. A decoder error keeps the decoder's message, as decodeBody's has.
func decodeStrictJSON(body io.Reader, dst any) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return apperr.Invalid("", "the request body is empty")
		}
		return apperr.Invalid("", err.Error()).Wrap(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return apperr.Invalid("", "unexpected data after the JSON value")
	}
	return nil
}

// endpointParam is a request field filled from the URL: from the query string (query:"name") or from a route
// parameter (path:"name").
type endpointParam struct {
	index  []int
	name   string
	inPath bool
}

// endpointParams lists the URL parameters of struct type t, walking embedded structs. A parameter is an exported
// string, bool or int field, or a slice of one of those; endpointParams panics on a tagged field of any other kind.
func endpointParams(t reflect.Type, index []int) []endpointParam {
	var out []endpointParam
	for i := range t.NumField() {
		f := t.Field(i)
		at := append(slices.Clone(index), i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, endpointParams(f.Type, at)...)
			continue
		}
		// Through a pointer the parameters would land in a struct nobody allocated: they would never be read.
		if f.Anonymous && f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct &&
			len(endpointParams(f.Type.Elem(), nil)) > 0 {
			panic(fmt.Sprintf("api: %s: embed %s by value: it holds URL parameters", t, f.Type.Elem()))
		}
		query, path := f.Tag.Get("query"), f.Tag.Get("path")
		if query == "" && path == "" {
			continue
		}
		if (query != "" && path != "") || !f.IsExported() || !isEndpointParamType(f.Type) {
			panic(fmt.Sprintf("api: %s.%s: a URL parameter is an exported string, bool or int field, or a slice of one, tagged query or path", t, f.Name))
		}
		out = append(out, endpointParam{index: at, name: query + path, inPath: path != ""})
	}
	return out
}

func isEndpointParamType(t reflect.Type) bool {
	if t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// decodeParams fills req's URL parameters. A value that does not parse is a 400 naming the parameter; an empty or
// absent one leaves the field as it is.
func (e *endpointSpec) decodeParams(r *http.Request, req any) error {
	if len(e.params) == 0 {
		return nil
	}
	v := reflect.ValueOf(req).Elem()
	query := r.URL.Query()
	for _, p := range e.params {
		raw := query[p.name]
		if p.inPath {
			// JSON and Query never see the route's pattern, so a path tag naming none of its parameters is caught
			// here, loudly: left alone the field would keep what the body put in it.
			if rctx := chi.RouteContext(r.Context()); rctx == nil || !slices.Contains(rctx.URLParams.Keys, p.name) {
				return apperr.Internal("", fmt.Sprintf("api: path parameter %q is not one of the route's", p.name))
			}
			raw = []string{chi.URLParam(r, p.name)}
		}
		if err := setEndpointParam(v.FieldByIndex(p.index), raw); err != nil {
			return apperr.Invalid("", fmt.Sprintf("invalid %q parameter: %v", p.name, err))
		}
	}
	return nil
}

// setEndpointParam sets f from a parameter's values: the first value for a single field; for a slice, every
// comma-separated element of every value (?tags=a,b and ?tags=a&tags=b alike).
func setEndpointParam(f reflect.Value, raw []string) error {
	if f.Kind() != reflect.Slice {
		if len(raw) == 0 || raw[0] == "" {
			return nil
		}
		return setEndpointScalar(f, raw[0])
	}
	var parts []string
	for _, s := range raw {
		for part := range strings.SplitSeq(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				parts = append(parts, part)
			}
		}
	}
	if len(parts) == 0 {
		return nil
	}
	list := reflect.MakeSlice(f.Type(), len(parts), len(parts))
	for i, part := range parts {
		if err := setEndpointScalar(list.Index(i), part); err != nil {
			return err
		}
	}
	f.Set(list)
	return nil
}

func setEndpointScalar(f reflect.Value, s string) error {
	switch f.Kind() {
	case reflect.String:
		f.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return errors.New("use true or false")
		}
		f.SetBool(b)
	default:
		n, err := strconv.ParseInt(s, 10, f.Type().Bits())
		if err != nil {
			return errors.New("must be an integer")
		}
		f.SetInt(n)
	}
	return nil
}
