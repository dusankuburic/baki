package api

import (
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
)

var updateRateLimitGroups = flag.Bool("update-ratelimit-groups", false, "rewrite testdata/ratelimit_groups.golden")

// rateLimitGroups is the frozen set of limiter groups, in the order the golden lists them. A group's name is its
// metric label and the middle segment of its Redis key; a rolling deploy runs two releases against the same keys, so
// renaming one splits its buckets (the limit doubles) and orphans its series.
var rateLimitGroups = []string{
	route.RateGeneral, route.RateAuth, route.RateAnalysis, route.RateChat, route.RateUpload, route.RateChannelTest,
	route.RatePerUser, route.RateAvatar, route.RateControl, route.RateEmbed,
}

// rateLimitExceeded reads rate_limit_exceeded_total by its group label.
func rateLimitExceeded(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != "rate_limit_exceeded_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "group" {
					out[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

// TestRateLimitGroups_Golden records, for every limiter group, the metric label its refusals carry and the shape of
// its Redis bucket key (ADR 0029); the per-user group has two shapes. The label is observed by refusing a request,
// the key shapes come from the functions that build them. Refresh only for an intended change (and know that it
// splits buckets across a rolling deploy):
// go test ./internal/api/ -run TestRateLimitGroups_Golden -update-ratelimit-groups
func TestRateLimitGroups_Golden(t *testing.T) {
	rt := newPerUserTestRouter()
	var b strings.Builder
	for _, g := range rateLimitGroups {
		// A limiter of this group with one token and no refill: the second request is refused.
		rl := middleware.NewRateLimiter(0, 1, nil).SetGroup(g)
		ok := rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		before := rateLimitExceeded(t)
		for i := 0; i < 2; i++ {
			ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}
		after := rateLimitExceeded(t)
		label := "(none)"
		for l, n := range after {
			if n > before[l] {
				label = l
			}
		}

		key := middleware.BucketKey(g, "<addr>")
		if g == route.RatePerUser {
			user := rt.perUserKey(httptest.NewRequest(http.MethodPost, "/api/x", nil).
				WithContext(auth.WithClaims(context.Background(), &auth.Claims{UserID: "user-42"})))
			user = strings.Replace(user, hashRateLimitKey("user-42"), "<sha256(userID)>", 1)
			anon := rt.perUserKey(httptest.NewRequest(http.MethodPost, "/api/auth/login", nil))
			anon = strings.Replace(anon, middleware.RateLimitAddr(middleware.ClientIP(httptest.NewRequest(http.MethodPost, "/", nil), nil)), "<addr>", 1)
			key = user + " ; " + anon
		}
		fmt.Fprintf(&b, "%s | %s | %s\n", g, label, key)
	}
	compareGolden(t, filepath.Join("testdata", "ratelimit_groups.golden"), b.String(), *updateRateLimitGroups, "-update-ratelimit-groups")
}

// TestRateLimitGroups_ListMatchesTheRouteConstants: a new Rate* constant in the route package must be added to the
// frozen list above (and so to the golden), or be RateNone, which is no limiter.
func TestRateLimitGroups_ListMatchesTheRouteConstants(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("route", "route.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "Rate") || len(vs.Values) <= i {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, _ := strconv.Unquote(lit.Value)
			declared[v] = true
		}
		return true
	})
	delete(declared, route.RateNone)
	listed := map[string]bool{}
	for _, g := range rateLimitGroups {
		listed[g] = true
	}
	for g := range declared {
		if !listed[g] {
			t.Errorf("route declares the rate-limit group %q, which the frozen list (and the golden) does not name", g)
		}
	}
	for g := range listed {
		if !declared[g] {
			t.Errorf("the frozen list names %q, which the route package no longer declares", g)
		}
	}
}

// TestRateLimitGroups_NoGroupLiteralsInTheChain: the limiters and their dispatch map are built from the route
// constants, never from string literals that could drift from the metric labels and the Redis keys.
func TestRateLimitGroups_NoGroupLiteralsInTheChain(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "middleware_chain.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	frozen := map[string]bool{}
	for _, g := range rateLimitGroups {
		frozen[g] = true
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, _ := strconv.Unquote(lit.Value); frozen[v] {
				t.Errorf("middleware_chain.go spells the group %q as a literal at %v: use the route.Rate* constant", v, lit.Pos())
			}
		}
		return true
	})
}
