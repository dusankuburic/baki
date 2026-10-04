package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUnauthorized_HookPrecedence (D-11, ai side): openaiBase.handle401
// classifies a 401/403 ahead of unauthorizedError on BOTH the chat and the
// stream path, receives the raw body (API Management's envelope decodes into
// openAIErrorResp with an empty message), and a nil result — or no hook —
// falls through to the shared invalid-key classification.
func TestUnauthorized_HookPrecedence(t *testing.T) {
	sentinel := errors.New("hook classified it")
	const apimBody = `{"statusCode":401,"message":"Unauthorized. Access token is missing, invalid, audience is incorrect (https://cognitiveservices.azure.com), or have expired."}`

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(apimBody))
		}))
		t.Cleanup(srv.Close)
		p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

		var sawBody []byte
		var sawErr openAIErrorResp
		p.handle401 = func(apiErr openAIErrorResp, body []byte) error {
			sawErr, sawBody = apiErr, body
			return sentinel
		}
		if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, sentinel) {
			t.Errorf("status %d chat: want the hook's error, got %v", status, err)
		}
		if err := p.Stream(context.Background(), compatChatReq(), func(Chunk) {}); !errors.Is(err, sentinel) {
			t.Errorf("status %d stream: want the hook's error, got %v", status, err)
		}
		if !strings.Contains(string(sawBody), `"statusCode":401`) || sawErr.Error.Message != "" {
			t.Errorf("hook must see the raw body and the (empty) envelope: body=%q apiErr=%+v", sawBody, sawErr)
		}

		// A nil result falls through to unauthorizedError.
		p.handle401 = func(openAIErrorResp, []byte) error { return nil }
		if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, sentinel) {
			t.Errorf("status %d nil hook result: want ErrApiKeyInvalid, got %v", status, err)
		}
		// No hook: the classification every other provider gets.
		p.handle401 = nil
		if err := p.Stream(context.Background(), compatChatReq(), func(Chunk) {}); !errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrEntraTokenRejected) {
			t.Errorf("status %d no hook: want bare ErrApiKeyInvalid, got %v", status, err)
		}
	}
}

// TestEntraUnauthorized_ReadsBothEnvelopes (D-11): the Entra hook keeps
// Azure's one-line explanation from either envelope, is both
// ErrEntraTokenRejected and ErrApiKeyInvalid (never PermissionDenied), and
// leaves PermissionDenied to its own path.
func TestEntraUnauthorized_ReadsBothEnvelopes(t *testing.T) {
	check := func(t *testing.T, err error, wantText string) {
		t.Helper()
		if !errors.Is(err, ErrEntraTokenRejected) || !errors.Is(err, ErrApiKeyInvalid) {
			t.Errorf("want ErrEntraTokenRejected and ErrApiKeyInvalid, got %v", err)
		}
		if errors.Is(err, ErrProviderPermissionDenied) {
			t.Errorf("a rejected token is not a permission problem: %v", err)
		}
		if !strings.Contains(err.Error(), wantText) || strings.ContainsAny(err.Error(), "\r\n") {
			t.Errorf("message = %q, want one line containing %q", err.Error(), wantText)
		}
	}

	// API Management's envelope: no error.message at all.
	apim := []byte(`{"statusCode":401,"message":"Unauthorized. Access token is missing, invalid,\n audience is incorrect (https://cognitiveservices.azure.com), or have expired."}`)
	var empty openAIErrorResp
	check(t, entraUnauthorized(empty, apim), "audience is incorrect")

	// The OpenAI envelope, with a code that is not PermissionDenied.
	var envelope openAIErrorResp
	envelope.Error.Code = "401"
	envelope.Error.Message = "Access denied\n\tdue to invalid token"
	check(t, entraUnauthorized(envelope, []byte(`{"error":{"code":"401"}}`)), "Access denied due to invalid token")

	// Nothing readable at all: still classified, with the bare sentinel text.
	if err := entraUnauthorized(empty, nil); err == nil || err.Error() != ErrEntraTokenRejected.Error() {
		t.Errorf("empty body: got %v", err)
	}

	// PermissionDenied keeps unauthorizedError's classification.
	var denied openAIErrorResp
	denied.Error.Code = "PermissionDenied"
	denied.Error.Message = "principal lacks Microsoft.CognitiveServices/accounts/OpenAI/deployments/chat/completions/action"
	if err := entraUnauthorized(denied, nil); err != nil {
		t.Errorf("PermissionDenied must fall through (nil), got %v", err)
	}

	// The text is bounded.
	var long openAIErrorResp
	long.Error.Message = strings.Repeat("x", 1000)
	if msg := azureAuthMessage(long, nil); len(msg) > azureAuthMessageMax+len("…") {
		t.Errorf("message not bounded: %d bytes", len(msg))
	}

	// Wired as the hook, end to end on the stream path with the APIM body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(apim)
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
	p.handle401 = entraUnauthorized
	check(t, p.Stream(context.Background(), compatChatReq(), func(Chunk) {}), "audience is incorrect")
}
