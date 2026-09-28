package ai

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Finding #32: a context-length rejection that states the window carries it.
func TestDetectContextLimitError_ParsesTheStatedWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		limit   int
	}{
		{"azure openai", "This model's maximum context length is 16384 tokens. However, your messages resulted in 40123 tokens. Please reduce the length of the messages.", 16384},
		{"vllm", "This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (32000 in the messages, 8000 in the completion). Please reduce the length of the messages or completion.", 32768},
		{"no number", "prompt is too long: the context length was exceeded", 0},
		{"claude wording", "prompt is too long: 250000 tokens > 200000 maximum", 200000},
		{"claude max_tokens wording", "input length and `max_tokens` exceed context limit: 180000 + 32000 > 200000, decrease input length or `max_tokens` and try again", 200000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := detectContextLimitError(400, tc.message)
			if !errors.Is(err, ErrContextLimit) {
				t.Fatalf("want ErrContextLimit, got %v", err)
			}
			var cle *ContextLimitError
			switch {
			case tc.limit == 0 && errors.As(err, &cle):
				t.Errorf("no stated window: got %+v", cle)
			case tc.limit > 0 && (!errors.As(err, &cle) || cle.Limit != tc.limit):
				t.Errorf("want ContextLimitError{%d}, got %#v", tc.limit, err)
			}
			if err.Error() != ErrContextLimit.Error() {
				t.Errorf("message = %q", err.Error())
			}
		})
	}
	if err := detectContextLimitError(500, "maximum context length is 10 tokens"); err != nil {
		t.Errorf("non-400: %v", err)
	}
}

// Finding #32 end to end: a deployment under a custom name (listed at the
// provider-wide 128k window) rejects an oversized request stating its 16k
// window; the next request's audited wrapper — the factory builds a fresh
// one per request — sizes that deployment to the stated window.
func TestAuditedAzure_LearnsTheWindowFromAContextLengthRejection(t *testing.T) {
	const msg = "This model's maximum context length is 16384 tokens. However, your messages resulted in 40123 tokens. Please reduce the length of the messages."
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": "context_length_exceeded"}})
	})
	raw := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) {
		c.ChatDeployment = "phi"
		c.Deployments = []string{"chat-stream"}
	})
	fresh := func() Provider {
		a := newAuditedProvider(raw, nil, "u1", azureFoundryProviderID)
		a.adoptRawHints(raw)
		return a
	}
	ctx := context.Background()
	if got := ContextLimitFor(ctx, fresh(), "phi"); got != raw.ContextLimit() {
		t.Fatalf("before any rejection: window = %d, want the provider default", got)
	}

	req := Request{Model: "phi", Messages: []Message{{Role: "user", Content: "long"}}}
	if _, err := fresh().Chat(ctx, req); !errors.Is(err, ErrContextLimit) {
		t.Fatalf("Chat: want ErrContextLimit, got %v", err)
	}
	next := fresh()
	if got := ContextLimitFor(ctx, next, "phi"); got != 16384 {
		t.Errorf("after the rejection: window = %d, want the stated 16384", got)
	}
	if got := ModelMaxOutputTokens(ctx, next, "phi"); got <= 0 || got > 16384/4 {
		t.Errorf("output cap = %d, want at most a quarter of the learned window", got)
	}
	if got := ContextLimitFor(ctx, next, "chat-stream"); got != raw.ContextLimit() {
		t.Errorf("another deployment inherited the window: %d", got)
	}

	// The streaming path learns the same way.
	stream := Request{Model: "chat-stream", Messages: []Message{{Role: "user", Content: "long"}}}
	if err := fresh().Stream(ctx, stream, func(Chunk) {}); !errors.Is(err, ErrContextLimit) {
		t.Fatalf("Stream: want ErrContextLimit, got %v", err)
	}
	if got := ContextLimitFor(ctx, fresh(), "chat-stream"); got != 16384 {
		t.Errorf("after a streamed rejection: window = %d, want 16384", got)
	}

	// Another user's scope is not affected.
	other := newAuditedProvider(raw, nil, "u2", azureFoundryProviderID)
	other.adoptRawHints(raw)
	if got := ContextLimitFor(ctx, other, "phi"); got != raw.ContextLimit() {
		t.Errorf("another user's window = %d, want the provider default", got)
	}
}

func TestLearnedWindows_OnlyNarrowAndOnlyAliasProviders(t *testing.T) {
	resetLearnedWindows()
	t.Cleanup(resetLearnedWindows)

	recordContextWindow("u1", "openai", "gpt-4o", 1000)
	if _, ok := lookupContextWindow("u1", "openai", "gpt-4o"); ok {
		t.Error("a provider whose ids are model names must not record windows")
	}
	models := []ModelInfo{{ID: "gpt-4o", ContextLimit: 128000}}
	if got := applyLearnedWindows("u1", "openai", models); got[0].ContextLimit != 128000 {
		t.Errorf("non-alias listing changed: %+v", got[0])
	}

	recordContextWindow("u1", azureFoundryProviderID, "router", 32768)
	recordContextWindow("u1", azureFoundryProviderID, "router", 65536)
	if lim, _ := lookupContextWindow("u1", azureFoundryProviderID, "router"); lim != 32768 {
		t.Errorf("learned window = %d, want the smaller observation", lim)
	}
	listed := applyLearnedWindows("u1", azureFoundryProviderID, []ModelInfo{
		{ID: "router", ContextLimit: 16384, MaxOutputTokens: 2048},
		{ID: "other", ContextLimit: 128000},
	})
	if listed[0].ContextLimit != 16384 || listed[0].MaxOutputTokens != 2048 {
		t.Errorf("a learned window must only narrow: %+v", listed[0])
	}
	if listed[1].ContextLimit != 128000 {
		t.Errorf("unrelated entry changed: %+v", listed[1])
	}

	ResetAzureFoundryCaches()
	if _, ok := lookupContextWindow("u1", azureFoundryProviderID, "router"); ok {
		t.Error("ResetAzureFoundryCaches must drop the learned windows")
	}
}
