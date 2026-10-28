package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The stop and the approval decision on a deployment with several replicas (ADR 0033): what the HTTP layer answers
// when the stream is not on the replica that got the request.

func commandHandler(t *testing.T, c *redisreplica.Cluster) *ChatHandler {
	t.Helper()
	svc := service.NewChatService(nil, t.TempDir(), nil, nil, nil, nil, nil, "", service.ChatOptions{Resume: c.Replicas[1].Client, Instance: "replica-2"})
	return NewChatHandler(svc, nil, &SecurityConfig{JWTEnabled: true}, nil, nil)
}

func callCommand(h *ChatHandler, handler func(http.ResponseWriter, *http.Request), user, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req = req.WithContext(auth.WithClaims(context.Background(), &auth.Claims{UserID: user}))
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

type errBody struct {
	Code    string         `json:"code"`
	Details map[string]any `json:"details"`
}

func wantUnreachable(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rr.Code, rr.Body.String())
	}
	var b errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil || b.Code != service.CodeReplicaUnreachable {
		t.Fatalf("body = %s, want code %s", rr.Body.String(), service.CodeReplicaUnreachable)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
	if v, _ := b.Details["retryAfterSeconds"].(float64); v <= 0 {
		t.Errorf("details = %v, want retryAfterSeconds", b.Details)
	}
}

const remoteSnapshot = `{"v":1,"kind":"chat.resume","replica":"replica-1","owner":"u1","text":"partial"}`

// Redis down: whether the stream exists cannot be read, which is "retry", not "no such stream".
func TestChatCommands_RedisDownIsRetryableNotARefusal(t *testing.T) {
	c := redisreplica.New(t, 2)
	h := commandHandler(t, c)
	c.Kill()
	wantUnreachable(t, callCommand(h, h.handleCancelStream, "u1", `{"id":"s-remote"}`))
	wantUnreachable(t, callCommand(h, h.handleFixDecision, "u1", `{"streamId":"s-remote","proposalId":"p1","approved":true}`))
}

// The stream's replica is not listening: the command is refused at once, the stop is not reported as done.
func TestChatCommands_OwnerGoneIsRetryable(t *testing.T) {
	c := redisreplica.New(t, 2)
	h := commandHandler(t, c)
	c.Redis.Set("chat:resume:s-remote", remoteSnapshot)
	wantUnreachable(t, callCommand(h, h.handleCancelStream, "u1", `{"id":"s-remote"}`))
}

// Another user's stream, and a stream nobody has, are both refused as unknown, as they always were.
func TestChatCommands_NotTheCallersStreamIsRefused(t *testing.T) {
	c := redisreplica.New(t, 2)
	h := commandHandler(t, c)
	c.Redis.Set("chat:resume:s-remote", remoteSnapshot)
	for _, id := range []string{"s-remote", "nobody-has-this"} {
		rr := callCommand(h, h.handleCancelStream, "u2", `{"id":"`+id+`"}`)
		var b errBody
		_ = json.Unmarshal(rr.Body.Bytes(), &b)
		if rr.Code != http.StatusForbidden || b.Code != "STREAM_NOT_FOUND" {
			t.Errorf("cancel of %s by u2: %d %s, want 403 STREAM_NOT_FOUND", id, rr.Code, rr.Body.String())
		}
	}
}

// No Redis: the handlers answer exactly as they did.
func TestChatCommands_WithoutRedisUnchanged(t *testing.T) {
	svc := service.NewChatService(nil, t.TempDir(), nil, nil, nil, nil, nil, "", service.ChatOptions{})
	h := NewChatHandler(svc, nil, &SecurityConfig{JWTEnabled: true}, nil, nil)
	rr := callCommand(h, h.handleCancelStream, "u1", `{"id":"unknown"}`)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "STREAM_NOT_FOUND") {
		t.Errorf("cancel of an unknown stream: %d %s, want 403 STREAM_NOT_FOUND", rr.Code, rr.Body.String())
	}
	h.common.JWTEnabled = false
	if rr := callCommand(h, h.handleCancelStream, "", `{"id":"unknown"}`); rr.Code != http.StatusOK {
		t.Errorf("cancel without accounts: %d %s, want 200 as before", rr.Code, rr.Body.String())
	}
}
