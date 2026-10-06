package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// BenchmarkBuildHandler measures one request through the whole production stack (BuildHandler): otelhttp,
// recovery, timeout, compression, access log, metrics and rate limiting, then the router's middlewares (security
// headers, CORS, the /api/v1 alias, JWT, the per-user limit, RLS) and the handler, over the in-memory FakeBackend
// so the numbers are the stack's own cost. It is the baseline Phase 1a (route-policy resolution) and Phase 2b
// (events) are compared against:
//
//	go test ./internal/api/ -run '^$' -bench BenchmarkBuildHandler -benchmem -count 8 > new.txt
//	benchstat old.txt new.txt
func BenchmarkBuildHandler(b *testing.B) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil))) // keep the access log's formatting cost, not its output
	b.Cleanup(func() { slog.SetDefault(prev) })

	backend := testutil.NewFakeBackend()
	rt := newTestRouter(backend, true)
	cfg := config.Default()
	cfg.Mode = config.ModeCloud
	for _, rate := range []*float64{
		&cfg.Runtime.RateLimitGeneralRPS, &cfg.Runtime.RateLimitGeneralBurst,
		&cfg.Runtime.RateLimitAuthRPS, &cfg.Runtime.RateLimitAuthBurst,
		&cfg.Runtime.RateLimitExpensiveRPS, &cfg.Runtime.RateLimitExpensiveBurst,
		&cfg.Runtime.RateLimitChatRPS, &cfg.Runtime.RateLimitChatBurst,
		&cfg.Runtime.RateLimitUploadRPS, &cfg.Runtime.RateLimitUploadBurst,
		&cfg.Runtime.RateLimitPerUserRPS, &cfg.Runtime.RateLimitPerUserBurst,
	} {
		*rate = 1e9 // measure the limiter's bookkeeping, never a 429
	}
	handler, limiters := BuildHandler(rt, cfg, nil)
	b.Cleanup(func() {
		for _, l := range limiters {
			l.Stop()
		}
	})

	ctx := context.Background()
	if err := backend.SaveUser(ctx, &storageif.User{ID: "u-bench", Email: "bench@acme.io", Role: auth.RoleMember}); err != nil {
		b.Fatal(err)
	}
	content := benchFlowContent(40 << 10)
	for i := range 50 {
		if err := backend.SaveFlow(ctx, &storageif.FlowDocument{
			ID: fmt.Sprintf("f-%02d", i), Name: fmt.Sprintf("Flow %d", i), OwnerID: "u-bench", Content: content,
		}); err != nil {
			b.Fatal(err)
		}
	}
	pair, err := rt.security.AuthMgr.Issue("u-bench", "bench@acme.io", auth.RoleMember)
	if err != nil {
		b.Fatal(err)
	}
	bearer := "Bearer " + pair.AccessToken

	// Every save must carry the version it overwrites (optimistic concurrency), or all but the first iteration
	// would measure the 409 path. The body is pre-marshalled around the version so building it costs nothing.
	contentJSON, _ := json.Marshal(content)
	saveBody := func() []byte {
		stored, err := backend.LoadFlowHeader(ctx, "f-00")
		if err != nil {
			b.Fatal(err)
		}
		return fmt.Appendf(nil, `{"name":"Flow 0","version":%d,"content":%s}`, stored.Version, contentJSON)
	}

	cases := []struct {
		name, method, target string
		body                 func() []byte
	}{
		{"library_list", http.MethodGet, "/api/library", nil},
		{"library_save", http.MethodPut, "/api/library/f-00", saveBody},
		{"dashboard_home", http.MethodGet, "/api/dashboard/home", nil},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			serve := func() {
				var body io.Reader
				if c.body != nil {
					b.StopTimer()
					body = bytes.NewReader(c.body())
					b.StartTimer()
				}
				req := httptest.NewRequest(c.method, c.target, body)
				req.Header.Set("Authorization", bearer)
				req.Header.Set("Accept-Encoding", "br, gzip")
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					b.Fatalf("%s %s = %d: %s", c.method, c.target, rr.Code, rr.Body.String())
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				serve()
			}
		})
	}
}

// benchFlowContent is a flow document JSON of roughly size bytes.
func benchFlowContent(size int) json.RawMessage {
	var sb strings.Builder
	sb.WriteString(`{"name":"Main","subflows":[{"name":"Main","blocks":[`)
	for i := 0; sb.Len() < size; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":"b%d","name":"Set variable %d","rawType":"Variables.SetVariable","properties":{"Value":"%%Counter%% + %d"}}`, i, i, i)
	}
	sb.WriteString(`]}]}`)
	return json.RawMessage(sb.String())
}
