package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-streaming-c20: a Stop (POST /api/chat/cancel) or an approval decision (POST /api/chat/fix-decision) that the
// shared general bucket or the per-user write cap refused was dropped, and the billed stream ran on. Both commands
// act only on the caller's own running stream, so they ride a bucket of their own (route.RateControl, per address)
// and are not charged to the per-user write cap.

// TestRateLimitGroup_StreamControlCommands: the legacy classifier puts both commands on the control bucket, whatever
// the spelling of the path; any other method on the same path is an ordinary request.
func TestRateLimitGroup_StreamControlCommands(t *testing.T) {
	for _, c := range []struct{ method, path, want string }{
		{http.MethodPost, "/api/chat/cancel", route.RateControl},
		{http.MethodPost, "/api/v1/chat/cancel", route.RateControl},
		{http.MethodPost, "/api/chat/./cancel", route.RateControl},
		{http.MethodPost, "/api/chat/fix-decision", route.RateControl},
		{http.MethodPost, "/api/v1/chat/fix-decision", route.RateControl},
		{http.MethodGet, "/api/chat/cancel", rlGroupGeneral},
		{http.MethodPost, "/api/chat/resume", rlGroupGeneral},
		{http.MethodPost, "/api/chat/stream", rlGroupChat},
	} {
		if got := rateLimitGroup(c.method, &url.URL{Path: c.path}); got != c.want {
			t.Errorf("rateLimitGroup(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// TestPerUserKey_StreamControlCommandsAreExempt: the per-user write cap never charges a stream-control command (the
// control bucket bounds them per address); every other write still is.
func TestPerUserKey_StreamControlCommandsAreExempt(t *testing.T) {
	rt := newPerUserTestRouter()
	authed := func(method, target string) *http.Request {
		return req(method, target).WithContext(auth.WithClaims(context.Background(), &auth.Claims{UserID: "user-42"}))
	}
	for _, target := range []string{"/api/chat/cancel", "/api/v1/chat/cancel", "/api/chat/fix-decision"} {
		if got := rt.perUserKey(authed(http.MethodPost, target)); got != "" {
			t.Errorf("POST %s charged to the per-user bucket %q, want exempt", target, got)
		}
	}
	for _, target := range []string{"/api/chat/stream", "/api/chat/resume", "/api/chat/get"} {
		if got := rt.perUserKey(authed(http.MethodPost, target)); got == "" {
			t.Errorf("POST %s is exempt from the per-user bucket, want charged", target)
		}
	}
}

// serveBody is serve with a JSON body.
func serveBody(h http.Handler, method, target, authHeader string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, target, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// TestChatCancel_ReachesItsHandlerWithTheSharedBucketsDrained is the item's done-when: in cloud mode, with the general
// per-address bucket and the per-user write cap drained (0.001 rps, burst 1), the owner's Stop still reaches the
// handler and stops the stream, and an approval decision still reaches its handler, in every route-policy mode.
// Another user's Stop reaches the handler too and is refused there: the exemption stops nothing that is not the
// caller's.
func TestChatCancel_ReachesItsHandlerWithTheSharedBucketsDrained(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("HOME", cfgDir)
	for _, mode := range allRoutePolicyModes {
		t.Run(string(mode), func(t *testing.T) {
			p := &blockingStreamProvider{FakeProvider: &testutil.FakeProvider{}, release: make(chan struct{})}
			t.Cleanup(func() { p.closeOnce.Do(func() { close(p.release) }) })
			provider := "c20-blocking-" + string(mode)
			ai.RegisterProvider(provider, func(string) ai.Provider { return p })

			rt := newModeTestRouter(t, mode)
			svc := moduleOf[*ChatHandler](t, rt).chatSvc
			t.Cleanup(svc.CancelAll)
			h := fullChain(t, rt, func(cfg *config.Config) {
				cfg.Mode = config.ModeCloud
				cfg.Runtime.RateLimitGeneralRPS, cfg.Runtime.RateLimitGeneralBurst = 0.001, 1
				cfg.Runtime.RateLimitPerUserRPS, cfg.Runtime.RateLimitPerUserBurst = 0.001, 1
			})
			owner := jwtBearer(t, rt, "u1", "u1@example.com")
			other := jwtBearer(t, rt, "u2", "u2@example.com")

			streamID := uuid.NewString()
			if _, err := svc.StreamChatMessage(context.Background(), "u1", &models.FlowDocument{ID: "f-c20", Name: "f"}, nil,
				models.ChatRequest{Provider: provider, UserMessage: "q", ClientStreamID: streamID}); err != nil {
				t.Fatalf("StreamChatMessage: %v", err)
			}

			// Drain both shared buckets: the first write takes the one token of each, the next is refused.
			serveBody(h, http.MethodPost, "/api/chat/suggested-prompts", owner, map[string]any{})
			if rr := serveBody(h, http.MethodPost, "/api/chat/suggested-prompts", owner, map[string]any{}); rr.Code != http.StatusTooManyRequests {
				t.Fatalf("second write = %d, want 429 (the shared buckets hold one)", rr.Code)
			}

			// Another user's Stop reaches the handler and is refused there; the stream runs on.
			if rr := serveBody(h, http.MethodPost, "/api/chat/cancel", other, map[string]string{"id": streamID}); rr.Code != http.StatusForbidden {
				t.Fatalf("another user's Stop = %d %s, want 403 from the handler", rr.Code, rr.Body.String())
			}
			if res, err := svc.ResumeStream(context.Background(), streamID, 0); err != nil || res.Done || res.Error != "" {
				t.Fatalf("after another user's Stop the stream reads %+v (%v), want it running", res, err)
			}

			// The owner's Stop, twice (a retry after a lost answer): both reach the handler.
			for i := 0; i < 2; i++ {
				if rr := serveBody(h, http.MethodPost, "/api/chat/cancel", owner, map[string]string{"id": streamID}); rr.Code != http.StatusOK {
					t.Fatalf("the owner's Stop #%d = %d %s, want 200", i+1, rr.Code, rr.Body.String())
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				res, err := svc.ResumeStream(context.Background(), streamID, 0)
				if err == nil && res.ErrorCode == "STREAM_STOPPED" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the stream did not stop: %+v (%v)", res, err)
				}
				time.Sleep(10 * time.Millisecond)
			}

			// An approval decision reaches its handler too (no such proposal: 404 from the handler, not a 429).
			if rr := serveBody(h, http.MethodPost, "/api/chat/fix-decision", owner,
				map[string]any{"streamId": streamID, "proposalId": "p-1", "approved": false}); rr.Code != http.StatusNotFound {
				t.Errorf("an approval decision = %d %s, want 404 from the handler", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestChatCancel_ControlBucketStillLimitsPerAddress: the commands are off the shared buckets, not unlimited: an
// address that floods them (with or without credentials, so a bogus token cannot buy unthrottled lookups) is
// refused by the control bucket before authentication, in every route-policy mode.
func TestChatCancel_ControlBucketStillLimitsPerAddress(t *testing.T) {
	for _, mode := range allRoutePolicyModes {
		t.Run(string(mode), func(t *testing.T) {
			h := fullChain(t, newModeTestRouter(t, mode), func(cfg *config.Config) {
				cfg.Mode = config.ModeCloud
				cfg.Runtime.RateLimitControlRPS, cfg.Runtime.RateLimitControlBurst = 0.001, 1
			})
			if rr := serveBody(h, http.MethodPost, "/api/chat/cancel", "", map[string]string{"id": "s"}); rr.Code == http.StatusTooManyRequests {
				t.Fatal("the first anonymous Stop was refused by the limiter")
			}
			if rr := serveBody(h, http.MethodPost, "/api/chat/cancel", "", map[string]string{"id": "s"}); rr.Code != http.StatusTooManyRequests {
				t.Errorf("the second anonymous Stop = %d, want 429 (the control bucket holds one)", rr.Code)
			}
			if rr := serveBody(h, http.MethodPost, "/api/chat/fix-decision", "", map[string]string{"streamId": "s", "proposalId": "p"}); rr.Code != http.StatusTooManyRequests {
				t.Errorf("an anonymous decision after the Stops = %d, want 429 (the commands share the control bucket)", rr.Code)
			}
		})
	}
}
