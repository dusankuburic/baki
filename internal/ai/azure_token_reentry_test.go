package ai

// R-provider-c5: the Responses reroute's token retry runs inside the turn's own, so one turn replays with a fresh
// token once: at most one chat-completions request and two /responses requests, one token invalidation.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func entraRerouteProvider(t *testing.T, responses func(w http.ResponseWriter, r *http.Request)) (*AzureFoundryProvider, *azureTestServer, *fakeCred) {
	t.Helper()
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			responses(w, r)
			return
		}
		if _, has := body["tools"]; has && body["reasoning_effort"] != "none" {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": gpt56ToolsRejection, "type": "invalid_request_error", "param": "reasoning_effort", "code": nil}})
			return
		}
		writeJSON(w, 500, map[string]any{"error": map[string]any{"message": "unexpected"}})
	})
	ResetAzureFoundryCaches()
	cfg := AzureFoundryConfig{Endpoint: srv.URL, Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra,
		TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}}
	p := newAzureFoundryProvider(cfg, nil, srv.Client())
	cred := &fakeCred{}
	p.tokens = newEntraTokenSource(cred)
	p.tokenFn = p.entraToken
	return p, srv, cred
}

func countPaths(reqs []azureSeenRequest) (chat, responses int) {
	for _, r := range reqs {
		switch {
		case strings.HasSuffix(r.Path, "/chat/completions"):
			chat++
		case strings.HasSuffix(r.Path, "/responses"):
			responses++
		}
	}
	return chat, responses
}

func TestAzureFoundry_RerouteTokenRetryIsNotNested(t *testing.T) {
	p, srv, cred := entraRerouteProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "token rejected", "code": "401"}})
	})
	err := p.Stream(context.Background(), toolsRequest(0), func(Chunk) {})
	if !errors.Is(err, ErrEntraTokenRejected) {
		t.Fatalf("err = %v, want ErrEntraTokenRejected", err)
	}
	chat, responses := countPaths(srv.requests())
	if chat > 1 || responses > 2 {
		t.Errorf("requests: %d chat-completions, %d /responses; want at most 1 and 2", chat, responses)
	}
	if n := atomic.LoadInt32(&cred.calls); n > 2 {
		t.Errorf("%d tokens fetched, want at most 2 (one invalidation per turn)", n)
	}
}

func TestAzureFoundry_RerouteFreshTokenCompletes(t *testing.T) {
	p, srv, _ := entraRerouteProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "token rejected", "code": "401"}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(gpt56ResponsesStream))
	})
	if err := p.Stream(context.Background(), toolsRequest(0), func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chat, responses := countPaths(srv.requests())
	if chat != 1 || responses != 2 {
		t.Errorf("requests: %d chat-completions, %d /responses; want exactly 1 and 2", chat, responses)
	}
}
