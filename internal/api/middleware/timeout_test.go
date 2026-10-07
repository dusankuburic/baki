package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/metrics"
)

func TestRequestTimeout_StreamingPathsBypass(t *testing.T) {
	streamingPaths := []string{"/ws", "/api/events", "/api/chat/stream"}
	for _, p := range streamingPaths {
		t.Run(p, func(t *testing.T) {
			var ctxCancelled bool
			handler := RequestTimeout(50*time.Millisecond, config.RoutePolicyLegacy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
					ctxCancelled = true
				case <-time.After(100 * time.Millisecond):
				}
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, p, nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if ctxCancelled {
				t.Error("streaming path should bypass timeout, context was cancelled")
			}
			if rr.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rr.Code)
			}
		})
	}
}

func TestRequestTimeout_NormalPathGetsDeadline(t *testing.T) {
	handler := RequestTimeout(20*time.Millisecond, config.RoutePolicyLegacy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Value(0).(time.Time)
		_ = deadline
		_ = ok
		// The context should have a deadline set
		_, hasDeadline := r.Context().Deadline()
		if !hasDeadline {
			t.Error("expected context to have a deadline")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestRequestTimeout_ContextCancelsAfterDuration(t *testing.T) {
	handler := RequestTimeout(10*time.Millisecond, config.RoutePolicyLegacy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			t.Error("context cancelled before handler finished")
		case <-time.After(5 * time.Millisecond):
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
}

func TestRequestTimeout_ContextAlreadyCancelledPropagates(t *testing.T) {
	handler := RequestTimeout(time.Hour, config.RoutePolicyLegacy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() == nil {
			t.Error("expected context to be already cancelled")
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/something", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
}

// TestRequestTimeout_BulkKnowledgePathsGetLongDeadline: the knowledge-base
// bulk routes (upload, re-index) run a sequence of embedding calls and get
// the bounded-long BulkRequestTimeout; the org's other knowledge routes
// keep the ordinary per-request deadline.
func TestRequestTimeout_BulkKnowledgePathsGetLongDeadline(t *testing.T) {
	cases := []struct {
		path string
		bulk bool
	}{
		{"/api/orgs/o/knowledge/reindex", true},
		{"/api/orgs/o/knowledge/upload", true},
		{"/api/orgs/o/knowledge", false},
		{"/api/orgs/o/knowledge/doc-1", false},
		{"/api/orgs/o/knowledge/upload/extra", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			var got time.Time
			var has bool
			h := RequestTimeout(30*time.Second, config.RoutePolicyLegacy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, has = r.Context().Deadline()
				w.WriteHeader(http.StatusOK)
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, c.path, nil))
			if !has {
				t.Fatal("no deadline: every knowledge route is bounded")
			}
			remaining := time.Until(got)
			if c.bulk {
				if remaining < time.Minute || remaining > BulkRequestTimeout+time.Minute {
					t.Errorf("bulk deadline %v from now; want ≈ %v", remaining.Round(time.Second), BulkRequestTimeout)
				}
			} else if remaining > 30*time.Second+time.Second {
				t.Errorf("ordinary deadline %v from now; want ≤ 30s", remaining.Round(time.Second))
			}
			if isBulkPath(c.path) != c.bulk {
				t.Errorf("isBulkPath(%q) = %v, want %v", c.path, !c.bulk, c.bulk)
			}
		})
	}
}

// TestRequestTimeout_ClassifiesTheDispatchedPath: the deadline class follows
// the path the router actually dispatches on (path.Clean, then the /api/v1
// alias strip), not the raw request path. On the raw string GET
// /api/v1/events — the SSE bus — got the 30 s default and the EventSource
// reconnected every 30 s, and a non-canonical spelling of a stream or bulk
// route got the short deadline although the router served it by the same
// long-running handler.
func TestRequestTimeout_ClassifiesTheDispatchedPath(t *testing.T) {
	const def = 30 * time.Second
	const none time.Duration = 0 // no deadline at all
	cases := []struct {
		path string
		want time.Duration
	}{
		// The /api/v1 alias and non-canonical spellings.
		{"/api/v1/events", none},
		{"/api/events/", none},
		{"/api//events", none},
		{"/api/v1/chat/stream/", StreamRequestTimeout},
		{"/api/chat/stream/", StreamRequestTimeout},
		{"/api/v1/orgs/x/knowledge/upload", BulkRequestTimeout},
		{"/api/v1/orgs/x/knowledge/./upload", BulkRequestTimeout},
		{"/api/orgs/x/knowledge/reindex/", BulkRequestTimeout},
		// Canonical paths are classified exactly as before.
		{"/ws", none},
		{"/api/events", none},
		{"/api/chat/stream", StreamRequestTimeout},
		{"/api/v1/chat/stream", StreamRequestTimeout},
		{"/api/orgs/x/knowledge/upload", BulkRequestTimeout},
		{"/api/orgs/x/knowledge/reindex", BulkRequestTimeout},
		{"/api/something", def},
		{"/api/v1/something", def},
		{"/api/v1", def},
		// "/api/v1" is an alias only as a whole segment.
		{"/api/v1foo/events", def},
		// Percent-escaped paths: chi routes on RawPath whenever it is set,
		// and nothing cleans that — the first two reach versions/{vn} with
		// the whole tail as vn, the pprof one /debug/pprof/*. Classified on
		// the cleaned decoded path they all read /api/events or /ws and ran
		// with no deadline at all; cleaning the escaped form instead would
		// still collapse the literal dot segments of the last two.
		{"/api/library/abc/versions/1%2F..%2F..%2F..%2F..%2F..%2Fapi%2Fevents", def},
		{"/api/library/abc/versions/1%2F..%2F..%2F..%2F..%2F..%2Fws", def},
		{"/api/library/%41/../../events", def},
		{"/debug/pprof/%41/../../../ws", def},
		// … and the raw form still carries a route's class.
		{"/api/orgs/o%2Dx/knowledge/upload", BulkRequestTimeout},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			var got time.Time
			var has bool
			h := RequestTimeout(def, config.RoutePolicyLegacy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, has = r.Context().Deadline()
				w.WriteHeader(http.StatusOK)
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.path, nil))
			if c.want == none {
				if has {
					t.Fatalf("deadline %v from now; a long-lived session must have none", time.Until(got).Round(time.Second))
				}
				return
			}
			if !has {
				t.Fatalf("no deadline; want ≈ %v", c.want)
			}
			if remaining := time.Until(got); remaining > c.want || remaining < c.want-10*time.Second {
				t.Errorf("deadline %v from now; want ≈ %v", remaining.Round(time.Second), c.want)
			}
		})
	}
}

func TestIsStreamingPath(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		// Bounded streams (*/stream) get the generous outer deadline …
		{"/api/chat/stream", true},
		{"/api/analysis/stream", true},
		{"/stream", true},
		// … open-ended sessions are long-lived, not streaming (no deadline).
		{"/ws", false},
		{"/api/events", false},
		{"/api/something", false},
		{"/", false},
		{"/api/flows/123", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isStreamingPath(tt.path); got != tt.expected {
				t.Errorf("isStreamingPath(%q) = %v, want %v", tt.path, got, tt.expected)
			}
		})
	}
	for _, p := range []string{"/ws", "/api/events"} {
		if !isLongLivedPath(p) {
			t.Errorf("isLongLivedPath(%q) = false; WebSocket/SSE sessions must stay unbounded", p)
		}
	}
	if isLongLivedPath("/api/chat/stream") {
		t.Error("a chat stream is bounded, not long-lived")
	}
}

// TestRequestTimeout_DeclarativeTakesThePolicyDeadline: in declarative mode the deadline class is the resolved
// policy's, whatever the path says; a request no resolver has seen gets the default, bounded deadline.
func TestRequestTimeout_DeclarativeTakesThePolicyDeadline(t *testing.T) {
	const def = 30 * time.Second
	cases := []struct {
		path   string
		policy *route.Policy
		want   time.Duration // 0: no deadline
	}{
		{"/api/chat/stream", &route.Policy{Deadline: route.DeadlineDefault}, def},
		{"/api/anything", &route.Policy{Deadline: route.DeadlineNone}, 0},
		{"/api/anything", &route.Policy{Deadline: route.DeadlineStream}, StreamRequestTimeout},
		{"/api/anything", &route.Policy{Deadline: route.DeadlineBulk}, BulkRequestTimeout},
		{"/ws", nil, def},
	}
	for _, c := range cases {
		var got time.Time
		var has bool
		h := RequestTimeout(def, config.RoutePolicyDeclarative)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, has = r.Context().Deadline()
		}))
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		if c.policy != nil {
			r = withPolicy(r, c.policy)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if c.want == 0 {
			if has {
				t.Errorf("%s %+v: deadline set, want none", c.path, c.policy)
			}
			continue
		}
		if remaining := time.Until(got); !has || remaining > c.want || remaining < c.want-10*time.Second {
			t.Errorf("%s %+v: deadline %v from now (set %v), want ≈ %v", c.path, c.policy, remaining.Round(time.Second), has, c.want)
		}
	}
}

// TestRequestTimeout_ShadowAppliesLegacyAndCountsDisagreement: shadow mode keeps the legacy deadline and counts a
// route whose declared class differs; 405 and no-route policies differ by design and are not counted.
func TestRequestTimeout_ShadowAppliesLegacyAndCountsDisagreement(t *testing.T) {
	endpoint := &route.Policy{Kind: route.KindEndpoint, Method: http.MethodPost, Pattern: "/api/shadow-deadline/stream", Label: "/api/shadow-deadline/stream", Deadline: route.DeadlineDefault}
	notAllowed := &route.Policy{Kind: route.KindMethodNotAllowed, Label: "/api/shadow-deadline-405/stream", Deadline: route.DeadlineDefault}
	var has bool
	var got time.Time
	h := RequestTimeout(30*time.Second, config.RoutePolicyShadow)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, has = r.Context().Deadline()
	}))
	h.ServeHTTP(httptest.NewRecorder(), withPolicy(httptest.NewRequest(http.MethodPost, "/api/shadow-deadline/stream", nil), endpoint))
	if remaining := time.Until(got); !has || remaining < 5*time.Minute {
		t.Errorf("shadow mode applied %v (set %v), want the legacy stream deadline", remaining.Round(time.Second), has)
	}
	h.ServeHTTP(httptest.NewRecorder(), withPolicy(httptest.NewRequest(http.MethodGet, "/api/shadow-deadline-405/stream", nil), notAllowed))
	if n := metrics.RoutePolicyMismatchCount(endpoint.Label, DecisionDeadline); n != 1 {
		t.Errorf("endpoint deadline mismatches = %v, want 1", n)
	}
	if n := metrics.RoutePolicyMismatchCount(notAllowed.Label, DecisionDeadline); n != 0 {
		t.Errorf("405 deadline mismatches = %v, want 0 (not compared)", n)
	}
}
