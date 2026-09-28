package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// azureTestServer records every request (method, path, headers, decoded JSON
// body) and answers with the handler's response.
type azureTestServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []azureSeenRequest
}

type azureSeenRequest struct {
	Path    string
	Headers http.Header
	Body    map[string]any
}

func newAzureTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int, body map[string]any)) *azureTestServer {
	t.Helper()
	s := &azureTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, azureSeenRequest{Path: r.URL.Path, Headers: r.Header.Clone(), Body: body})
		n := len(s.reqs)
		s.mu.Unlock()
		handler(w, r, n, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *azureTestServer) requests() []azureSeenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]azureSeenRequest, len(s.reqs))
	copy(out, s.reqs)
	return out
}

func azureKeyConfig(endpoint string) AzureFoundryConfig {
	return AzureFoundryConfig{Endpoint: endpoint, Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "secret-key"}}
}

// newAzureKeyProvider builds an API-key provider against a local server
// (validation is bypassed: httptest hosts are loopback).
func newAzureKeyProvider(t *testing.T, srv *azureTestServer, mutate func(*AzureFoundryConfig)) *AzureFoundryProvider {
	t.Helper()
	ResetAzureFoundryCaches()
	cfg := azureKeyConfig(srv.URL)
	if mutate != nil {
		mutate(&cfg)
	}
	return newAzureFoundryProvider(cfg, nil, srv.Client())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func chatCompletionJSON(text, model string) map[string]any {
	return map[string]any{
		"id":    "chatcmpl-1",
		"model": model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}
}

func TestAzureFoundry_ChatRequestShape(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 200, chatCompletionJSON("hello", "gpt-4o-2024-11-20"))
	})
	p := newAzureKeyProvider(t, srv, nil)

	resp, err := p.Chat(context.Background(), Request{Model: "my-gpt4o", Temperature: 0.3, MaxTokens: 256,
		Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hello" || resp.ServedModel != "gpt-4o-2024-11-20" || resp.TokensIn != 10 || resp.TokensOut != 5 {
		t.Errorf("response = %+v", resp)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	got := reqs[0]
	if got.Path != "/openai/v1/chat/completions" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Headers.Get("api-key") != "secret-key" || got.Headers.Get("Authorization") != "Bearer secret-key" {
		t.Errorf("auth headers = %v", got.Headers)
	}
	if got.Body["model"] != "my-gpt4o" {
		t.Errorf("model = %v, want the deployment name", got.Body["model"])
	}
	if _, has := got.Body["max_tokens"]; has {
		t.Error("max_tokens must not be sent to Azure (reasoning deployments reject it)")
	}
	if got.Body["max_completion_tokens"] != float64(256) {
		t.Errorf("max_completion_tokens = %v", got.Body["max_completion_tokens"])
	}
}

const azureStreamFixture = `data: {"id":"x","model":"gpt-4o-2024-11-20","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search_flow","arguments":"{\"q\":"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`

func TestAzureFoundry_StreamCarriesServedModelAndTools(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(azureStreamFixture))
	})
	p := newAzureKeyProvider(t, srv, nil)

	var text strings.Builder
	var done Chunk
	err := p.Stream(context.Background(), Request{Model: "my-gpt4o", Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolDefinition{{Name: "search_flow", InputSchema: []byte(`{"type":"object"}`)}}}, func(c Chunk) {
		text.WriteString(c.Text)
		if c.Done {
			done = c
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q", text.String())
	}
	if done.ServedModel != "gpt-4o-2024-11-20" || done.TokensIn != 10 || done.TokensOut != 5 || done.FinishReason != "tool_calls" {
		t.Errorf("done chunk = %+v", done)
	}
	if len(done.ToolCalls) != 1 || done.ToolCalls[0].ID != "call_1" || done.ToolCalls[0].Name != "search_flow" || string(done.ToolCalls[0].Input) != `{"q":"x"}` {
		t.Errorf("tool calls = %+v", done.ToolCalls)
	}
	if body := srv.requests()[0].Body; body["stream"] != true || body["tools"] == nil {
		t.Errorf("stream request body = %v", body)
	}
}

func TestAzureFoundry_RepairsUnsupportedTemperature(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, has := body["temperature"]; has {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": "Unsupported value: 'temperature' does not support 0.3 with this model. Only the default (1) value is supported.",
				"type":    "invalid_request_error", "param": "temperature", "code": "unsupported_value"}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "o4-mini-2025-04-16"))
	})
	p := newAzureKeyProvider(t, srv, nil)
	resp, err := p.Chat(context.Background(), Request{Model: "reasoning", Temperature: 0.3, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want exactly one repair round trip", len(reqs))
	}
	if _, has := reqs[1].Body["temperature"]; has {
		t.Error("second request still carries temperature")
	}
}

func TestAzureFoundry_SwapsMaxTokensParam(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, has := body["max_completion_tokens"]; has {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": "Unrecognized request argument supplied: max_completion_tokens", "type": "invalid_request_error"}})
			return
		}
		if body["max_tokens"] != float64(128) {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "max_tokens missing after swap", "code": "bad"}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "legacy"))
	})
	p := newAzureKeyProvider(t, srv, nil)
	if _, err := p.Chat(context.Background(), Request{Model: "legacy", MaxTokens: 128, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("got %d requests, want 2", n)
	}
}

func TestAzureFoundry_DoesNotRepairLoadBearingParams(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"message": "Unsupported parameter: 'messages' is not supported", "param": "messages", "code": "unsupported_parameter"}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	_, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected the 400 to surface")
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("a rejection naming a required field must not be retried; got %d requests", n)
	}
}

func TestAzureFoundry_ContentFilterOnPrompt(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"message":    "The response was filtered due to the prompt triggering Azure OpenAI's content management policy.",
			"type":       nil,
			"code":       "content_filter",
			"innererror": map[string]any{"code": "ResponsibleAIPolicyViolation"}}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	req := Request{Model: "m", Messages: []Message{{Role: "user", Content: "bad"}}}
	if _, err := p.Chat(context.Background(), req); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("Chat: want ErrContentFiltered, got %v", err)
	}
	if err := p.Stream(context.Background(), req, func(Chunk) {}); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("Stream: want ErrContentFiltered, got %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("content filter rejections must not be retried/repaired: %d requests", n)
	}
}

func TestParseOpenAISSE_ContentFilterFinishIsAnError(t *testing.T) {
	body := "data: {\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n" +
		"data: [DONE]\n\n"
	var chunks []Chunk
	err := parseOpenAISSE(strings.NewReader(body), func(c Chunk) { chunks = append(chunks, c) })
	if err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(chunks) != 2 || chunks[0].Text != "partial" || !errors.Is(chunks[1].Error, ErrContentFiltered) {
		t.Errorf("chunks = %+v", chunks)
	}
	for _, c := range chunks {
		if c.Done {
			t.Error("a filtered completion must not end with a clean Done chunk")
		}
	}
}

func TestAzureFoundry_ChatContentFilterFinish(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		resp := chatCompletionJSON("partial", "gpt-4o")
		resp["choices"].([]map[string]any)[0]["finish_reason"] = "content_filter"
		writeJSON(w, 200, resp)
	})
	p := newAzureKeyProvider(t, srv, nil)
	if _, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("want ErrContentFiltered, got %v", err)
	}
}

func TestAzureFoundry_401IsInvalidKey(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "Access denied due to invalid subscription key", "code": "401"}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	if _, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("want ErrApiKeyInvalid, got %v", err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("API-key mode must not retry a 401: %d requests", n)
	}
}

func TestAzureFoundry_EntraRetriesOnceWithFreshToken(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "token rejected", "code": "401"}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "gpt-4o"))
	})
	ResetAzureFoundryCaches()
	cfg := AzureFoundryConfig{Endpoint: srv.URL, Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra,
		TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}}
	p := newAzureFoundryProvider(cfg, nil, srv.Client())
	cred := &fakeCred{}
	p.tokens = newEntraTokenSource(cred)
	p.tokenFn = p.entraToken

	resp, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	reqs := srv.requests()
	if len(reqs) != 2 || reqs[0].Headers.Get("Authorization") != "Bearer tok-1" || reqs[1].Headers.Get("Authorization") != "Bearer tok-2" {
		t.Errorf("requests = %+v", reqs)
	}
	if reqs[0].Headers.Get("api-key") != "" {
		t.Error("entra mode must not send an api-key header")
	}

	// A rejection that survives the fresh token is final (no loop).
	srv2 := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "no role", "code": "401"}})
	})
	cfg.Endpoint = srv2.URL
	p2 := newAzureFoundryProvider(cfg, nil, srv2.Client())
	p2.tokens = newEntraTokenSource(&fakeCred{})
	p2.tokenFn = p2.entraToken
	_, err = p2.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("want ErrApiKeyInvalid, got %v", err)
	}
	if errors.Is(err, ErrProviderPermissionDenied) {
		t.Errorf("a 401 without code PermissionDenied is an invalid credential, got %v", err)
	}
	if n := len(srv2.requests()); n != 2 {
		t.Errorf("want exactly one retry, got %d requests", n)
	}
}

// D-26: with a real credential the "retry with a fresh token" only works if
// Invalidate rebuilds the credential — azidentity's MSAL cache hands back the
// refused token until 5 minutes before expiry (stickyCred mirrors that). With
// no constructor the retry degrades to one request and the original error.
func TestAzureFoundry_EntraRetryRebuildsCredential(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "token rejected", "code": "401"}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "gpt-4o"))
	})
	ResetAzureFoundryCaches()
	cfg := AzureFoundryConfig{Endpoint: srv.URL, Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra,
		TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}}
	p := newAzureFoundryProvider(cfg, nil, srv.Client())
	src := newEntraTokenSource(&stickyCred{token: "tok-1"})
	var rebuilds atomic.Int32
	src.newCred = func() (azcore.TokenCredential, error) {
		rebuilds.Add(1)
		return &stickyCred{token: "tok-2"}, nil
	}
	p.tokens = src
	p.tokenFn = p.entraToken

	resp, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	reqs := srv.requests()
	if len(reqs) != 2 || reqs[0].Headers.Get("Authorization") != "Bearer tok-1" || reqs[1].Headers.Get("Authorization") != "Bearer tok-2" {
		t.Errorf("requests = %+v, want tok-1 then the rebuilt credential's tok-2", reqs)
	}
	if n := rebuilds.Load(); n != 1 {
		t.Errorf("credential rebuilt %d times, want once", n)
	}

	// No constructor: MSAL's cached token would come back, so the retry is
	// not attempted and the rejection is final after one request.
	srv2 := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "token rejected", "code": "401"}})
	})
	cfg.Endpoint = srv2.URL
	p2 := newAzureFoundryProvider(cfg, nil, srv2.Client())
	p2.tokens = newEntraTokenSource(&stickyCred{token: "tok-1"})
	p2.tokenFn = p2.entraToken
	_, err = p2.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("want ErrApiKeyInvalid, got %v", err)
	}
	if n := len(srv2.requests()); n != 1 {
		t.Errorf("without a rebuilt credential the same token must not be replayed: %d requests", n)
	}
}

// D-11: in Entra ID mode a 401/403 from the resource (either envelope,
// including API Management's {"statusCode","message"} shape, which decodes
// to an empty error.message) is a rejected access token carrying Azure's
// one-line explanation — still ErrApiKeyInvalid (one fresh-token replay, no
// retry, no breaker count), never PermissionDenied; the same body from an
// API-key provider stays a bare invalid key.
func TestAzureFoundry_EntraTokenRejectedKeepsAzureMessage(t *testing.T) {
	const apimMsg = "Unauthorized. Access token is missing, invalid, audience is incorrect (https://cognitiveservices.azure.com), or have expired."
	for name, body := range map[string]map[string]any{
		"apim envelope":   {"statusCode": 401, "message": apimMsg},
		"openai envelope": {"error": map[string]any{"code": "401", "message": apimMsg}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
				writeJSON(w, 401, body)
			})
			ResetAzureFoundryCaches()
			cfg := AzureFoundryConfig{Endpoint: srv.URL, Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra,
				TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}}
			p := newAzureFoundryProvider(cfg, nil, srv.Client())
			p.tokens = newEntraTokenSource(&fakeCred{})
			p.tokenFn = p.entraToken
			req := Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}

			check := func(what string, err error) {
				t.Helper()
				if !errors.Is(err, ErrEntraTokenRejected) || !errors.Is(err, ErrApiKeyInvalid) {
					t.Errorf("%s: want ErrEntraTokenRejected and ErrApiKeyInvalid, got %v", what, err)
				}
				if errors.Is(err, ErrProviderPermissionDenied) {
					t.Errorf("%s: a rejected token is not PermissionDenied: %v", what, err)
				}
				if err == nil || !strings.Contains(err.Error(), "audience is incorrect") || strings.ContainsAny(err.Error(), "\r\n") {
					t.Errorf("%s: Azure's explanation must be kept on one line, got %v", what, err)
				}
			}
			check("stream", p.Stream(context.Background(), req, func(Chunk) {}))
			reqs := srv.requests()
			if len(reqs) != 2 || reqs[0].Headers.Get("Authorization") != "Bearer tok-1" || reqs[1].Headers.Get("Authorization") != "Bearer tok-2" {
				t.Errorf("requests = %+v, want exactly one fresh-token replay (tok-1, tok-2)", reqs)
			}
			_, err := p.Chat(context.Background(), req)
			check("chat", err)

			// API-key mode on the same body: an invalid key, nothing more.
			kp := newAzureKeyProvider(t, srv, nil)
			_, err = kp.Chat(context.Background(), req)
			if !errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrEntraTokenRejected) {
				t.Errorf("api-key provider: want a bare ErrApiKeyInvalid, got %v", err)
			}
		})
	}
}

// D-14: the connection test's empty-candidate path asks the probe why an
// Entra ID + project document that names nothing lists nothing — the token
// refusal or the project route's answer — instead of "name a deployment".
func TestProbeAzureFoundryProject_ReportsTokenRefusal(t *testing.T) {
	resetEntraSources()
	t.Cleanup(resetEntraSources)
	const tenant = "11111111-2222-3333-4444-555555555555"
	const client = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	refused := &azidentity.AuthenticationFailedError{RawResponse: &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_client","error_codes":[7000215]}`)),
	}}
	entraSources.mu.Lock()
	entraSources.m[entraSourceKey(tenant, client, "wrong")] = entraSourceEntry{src: newEntraTokenSource(erroringCred{refused}), lastUsed: time.Now()}
	entraSources.mu.Unlock()

	doc := `{"endpoint":"https://my-res.services.ai.azure.com","auth":{"mode":"entra","tenantId":"` + tenant + `","clientId":"` + client + `","clientSecret":"wrong"},` +
		`"projectEndpoint":"https://my-res.services.ai.azure.com/api/projects/p"}`
	err := ProbeAzureFoundryProject(context.Background(), doc)
	if !errors.Is(err, errEntraToken) || !errors.Is(err, ErrEntraCredentialsRejected) {
		t.Fatalf("want the token refusal, got %v", err)
	}
	if err.Error() == "" || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("the message must name the refusal: %q", err)
	}

	// Documents that cannot list for a benign reason probe nothing.
	for name, doc := range map[string]string{
		"unconfigured": "",
		"api key":      `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"c"}`,
		"no project":   `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"entra","tenantId":"` + tenant + `","clientId":"` + client + `","clientSecret":"wrong"}}`,
	} {
		if err := ProbeAzureFoundryProject(context.Background(), doc); err != nil {
			t.Errorf("%s: want nil, got %v", name, err)
		}
	}
	if err := ProbeAzureFoundryProject(context.Background(), "not json"); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("a broken document reports its parse error, got %v", err)
	}

	// The project route's answer (a missing role) is reported too.
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		writeJSON(w, 403, map[string]any{"error": map[string]any{"code": "PermissionDenied", "message": "the principal lacks the Azure AI User role"}})
	})
	p := newAzureAgentProvider(t, srv)
	err = p.probeProject(context.Background())
	if err == nil || !strings.Contains(err.Error(), "project deployments listing failed") || !strings.Contains(err.Error(), "Azure AI User") {
		t.Errorf("want the project route's explanation, got %v", err)
	}
}

// D-15: removing or replacing the document releases its Entra ID token
// source (credential object and cached token) and its cached listing; other
// principals' sources are untouched.
func TestForgetAzureFoundryCredential_DropsSourceAndListing(t *testing.T) {
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	cfg := AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/p",
		Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s1"}}
	other := cfg
	other.Auth.ClientSecret = "s2"
	src, err := entraSourceFor(cfg.Auth.TenantID, cfg.Auth.ClientID, cfg.Auth.ClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	otherSrc, err := entraSourceFor(other.Auth.TenantID, other.Auth.ClientID, other.Auth.ClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	azureModelsCache.mu.Lock()
	azureModelsCache.m[azureModelsCacheKey(cfg)] = azureModelsEntry{models: []ModelInfo{{ID: "chat"}}, expires: time.Now().Add(time.Hour)}
	azureModelsCache.m[azureModelsCacheKey(other)] = azureModelsEntry{models: []ModelInfo{{ID: "chat"}}, expires: time.Now().Add(time.Hour)}
	azureModelsCache.mu.Unlock()

	ForgetAzureFoundryCredential(cfg)

	if lookupEntraSource(src) {
		t.Error("the document's token source must be dropped")
	}
	if !lookupEntraSource(otherSrc) {
		t.Error("another principal's source must survive")
	}
	if _, ok := cachedAzureModels(azureModelsCacheKey(cfg)); ok {
		t.Error("the document's cached listing must be dropped")
	}
	if _, ok := cachedAzureModels(azureModelsCacheKey(other)); !ok {
		t.Error("another document's listing must survive")
	}
	if again, _ := entraSourceFor(cfg.Auth.TenantID, cfg.Auth.ClientID, cfg.Auth.ClientSecret); again == src {
		t.Error("the next provider for the same tuple must build a fresh source")
	}
	ForgetAzureFoundryCredential(AzureFoundryConfig{}) // unconfigured: no-op, no panic
}

// Finding #23: a 401 PermissionDenied — an Entra ID principal the service
// accepted but that lacks the data-plane role — keeps Azure's explanation, is
// ErrProviderPermissionDenied as well as ErrApiKeyInvalid, and is not retried
// with a fresh token (a new token cannot grant a role), for chat and streams.
func TestAzureFoundry_PermissionDeniedKeepsAzureMessage(t *testing.T) {
	const azureMsg = "The principal `aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee` lacks the required data action `Microsoft.CognitiveServices/accounts/OpenAI/deployments/chat/completions/action` to perform `POST /openai/deployments/{deployment-id}/chat/completions` operation."
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "PermissionDenied", "message": azureMsg}})
	})
	ResetAzureFoundryCaches()
	cfg := AzureFoundryConfig{Endpoint: srv.URL, Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra,
		TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}}
	p := newAzureFoundryProvider(cfg, nil, srv.Client())
	cred := &fakeCred{}
	p.tokens = newEntraTokenSource(cred)
	p.tokenFn = p.entraToken
	req := Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}

	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrProviderPermissionDenied) || !errors.Is(err, ErrApiKeyInvalid) {
			t.Errorf("%s: want ErrProviderPermissionDenied and ErrApiKeyInvalid, got %v", what, err)
		}
		if err == nil || !strings.Contains(err.Error(), "lacks the required data action") {
			t.Errorf("%s: Azure's explanation must be kept, got %v", what, err)
		}
	}
	_, err := p.Chat(context.Background(), req)
	check("chat", err)
	check("stream", p.Stream(context.Background(), req, func(Chunk) {}))
	if n := len(srv.requests()); n != 2 {
		t.Errorf("want one request per call (no fresh-token retry), got %d", n)
	}
	if n := atomic.LoadInt32(&cred.calls); n != 1 {
		t.Errorf("token fetches = %d, want 1 (the role is missing, not the token)", n)
	}
}

func TestParseRetryAfter_PrefersMilliseconds(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "2")
	resp.Header.Set("retry-after-ms", "1500")
	if d := parseRetryAfter(resp); d != 1500*time.Millisecond {
		t.Errorf("parseRetryAfter = %v, want 1.5s", d)
	}
	resp.Header.Del("retry-after-ms")
	if d := parseRetryAfter(resp); d != 2*time.Second {
		t.Errorf("parseRetryAfter without ms = %v, want 2s", d)
	}
}

func TestAzureFoundry_429CarriesRetryAfterMs(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("retry-after-ms", "1500")
		writeJSON(w, 429, map[string]any{"error": map[string]any{"message": "Requests to the ChatCompletions_Create Operation have exceeded token rate limit", "code": "429"}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	_, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	var rle *RateLimitError
	if !errors.As(err, &rle) || rle.RetryAfter != 1500*time.Millisecond {
		t.Errorf("want RateLimitError{1.5s}, got %v", err)
	}
}

// azureCatalogueServer answers GET /openai/v1/models the way Azure really
// does — the resource's model CATALOGUE (versioned base and fine-tuned ids
// with capabilities), not its deployments — and fails the test if anything
// requests it: the listing must never be built from it.
func azureCatalogueServer(t *testing.T) *azureTestServer {
	t.Helper()
	return newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		t.Errorf("unexpected request %s %s: the listing must not touch the network", r.Method, r.URL.Path)
		writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "dall-e-3-3.0", "object": "model", "capabilities": map[string]any{"chat_completion": false, "inference": true}, "lifecycle_status": "generally-available"},
			{"id": "gpt-4o-2024-11-20", "object": "model", "capabilities": map[string]any{"chat_completion": true, "inference": true}, "lifecycle_status": "generally-available"},
			{"id": "text-embedding-3-large-1", "object": "model", "capabilities": map[string]any{"embeddings": true}},
		}})
	})
}

// Finding #1: GET /openai/v1/models is the catalogue, so the API-key listing
// is exactly the document's deployments — chat, additional, embedding — with
// no request at all, each sized and priced from the catalogue when its name
// is a catalogued model.
func TestAzureFoundry_ModelsListsConfiguredDeployments(t *testing.T) {
	srv := azureCatalogueServer(t)
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) {
		c.ChatDeployment = "prod-gpt4o"
		c.Deployments = []string{"gpt-4o", "Phi-4", "model-router"}
		c.EmbeddingDeployment = "text-embedding-3-small"
	})
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "prod-gpt4o,gpt-4o,Phi-4,model-router,text-embedding-3-small" {
		t.Fatalf("listing = %v, want the configured deployments in document order", ids)
	}
	custom, gpt4o, phi, router, emb := models[0], models[1], models[2], models[3], models[4]
	if custom.ContextLimit != p.ContextLimit() || custom.Pricing != (Pricing{}) || custom.MaxOutputTokens != 0 || custom.Embedding {
		t.Errorf("custom-named deployment = %+v, want the provider default window and no guessed price", custom)
	}
	if gpt4o.Pricing.InputCostPerM != 2.5 || gpt4o.ContextLimit != 128000 || gpt4o.MaxOutputTokens != 16384 || gpt4o.Embedding {
		t.Errorf("catalogued name = %+v, want the catalogue's price and limits", gpt4o)
	}
	// Finding #32: a small-window model named after itself is sized right
	// before any call succeeded.
	if phi.ContextLimit != 16384 || phi.MaxOutputTokens != 8192 {
		t.Errorf("Phi-4 = %+v, want its 16k window up front", phi)
	}
	if router.ContextLimit != p.ContextLimit() || router.MaxOutputTokens != 0 {
		t.Errorf("model-router = %+v, want the conservative default window", router)
	}
	// Finding #42: the embedding deployment stays listed (pricing) but flagged.
	if !emb.Embedding || emb.Pricing.InputCostPerM != 0.02 {
		t.Errorf("embedding deployment = %+v, want Embedding and its catalogue price", emb)
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("API-key listing made %d requests, want none (never /openai/v1/models)", n)
	}
	if p.DefaultModel() != "prod-gpt4o" {
		t.Errorf("default = %q, want the chat deployment", p.DefaultModel())
	}
}

func TestAzureFoundry_ModelsEmptyWithoutDeploymentsOrProject(t *testing.T) {
	srv := azureCatalogueServer(t)
	p := newAzureKeyProvider(t, srv, nil)
	models, err := p.Models(context.Background())
	if err != nil || len(models) != 0 {
		t.Errorf("nothing named and no project: models=%+v err=%v, want an empty list", models, err)
	}
	if p.DefaultModel() != "" {
		t.Errorf("configured provider without a chat deployment: default = %q, want none (never the catalogue's gpt-4.1)", p.DefaultModel())
	}

	// Only additional deployments: the first is the default.
	q := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.Deployments = []string{"a1", "a2"} })
	if q.DefaultModel() != "a1" {
		t.Errorf("default = %q, want the first additional deployment", q.DefaultModel())
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

func TestAzureFoundry_EmbedSendsDimensionsForTextEmbedding3(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if r.URL.Path != "/openai/v1/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		inputs, _ := body["input"].([]any)
		out := []map[string]any{}
		for range inputs {
			out = append(out, map[string]any{"embedding": []float32{0.1}})
		}
		writeJSON(w, 200, map[string]any{"data": out})
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "text-embedding-3-large" })
	p.setEmbeddingDimensions(1536)
	if _, err := p.Embed(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	body := srv.requests()[0].Body
	if body["model"] != "text-embedding-3-large" || body["dimensions"] != float64(1536) {
		t.Errorf("embeddings body = %v", body)
	}

	// The document's deployment wins over the deployment-wide override; a
	// document without one takes the override.
	p.setEmbeddingModel("other")
	if p.EmbeddingModel() != "text-embedding-3-large" {
		t.Errorf("override replaced the document's deployment: %q", p.EmbeddingModel())
	}
	q := newAzureKeyProvider(t, srv, nil)
	if _, err := q.Embed(context.Background(), []string{"a"}); err == nil {
		t.Error("no embedding deployment: Embed must fail with guidance")
	}
	q.setEmbeddingModel("ada")
	if q.EmbeddingModel() != "ada" {
		t.Errorf("override not applied when the document has no deployment: %q", q.EmbeddingModel())
	}
}

func TestNewAzureFoundryProvider_ConfigStates(t *testing.T) {
	ResetAzureFoundryCaches()
	unconfigured := NewAzureFoundryProvider("")
	models, err := unconfigured.Models(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("unconfigured Models = %v, %v", models, err)
	}
	if unconfigured.DefaultModel() != "gpt-4.1" || unconfigured.ID() != "azure-foundry" || !unconfigured.SupportsTools() {
		t.Errorf("metadata provider = %+v", unconfigured)
	}

	broken := NewAzureFoundryProvider("not json")
	if _, err := broken.Chat(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("broken document Chat: %v", err)
	}
	if err := broken.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {}); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("broken document Stream: %v", err)
	}
	if _, err := broken.Models(context.Background()); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("broken document Models: %v", err)
	}

	disallowed := NewAzureFoundryProvider(`{"endpoint":"https://evil.example","auth":{"mode":"api_key","key":"k"}}`)
	if _, err := disallowed.Chat(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("disallowed host must never be dialled: %v", err)
	}

	valid := NewAzureFoundryProvider(`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"chat"}`)
	if valid.configErr != nil || valid.DefaultModel() != "chat" || *valid.baseURL != "https://my-res.openai.azure.com/openai/v1/chat/completions" {
		t.Errorf("valid provider = err %v default %q base %q", valid.configErr, valid.DefaultModel(), *valid.baseURL)
	}
}

// A full model-listing cache makes room by dropping expired listings first
// and then the soonest-to-expire one — never by wiping the whole map, which
// also discarded every listing the active users were about to read (a fresh
// project listing is two paginated API calls each).
func TestAzureModelsCacheEvictionIsIncremental(t *testing.T) {
	azureModelsCache.mu.Lock()
	prev := azureModelsCache.m
	azureModelsCache.m = map[string]azureModelsEntry{}
	azureModelsCache.mu.Unlock()
	t.Cleanup(func() {
		azureModelsCache.mu.Lock()
		azureModelsCache.m = prev
		azureModelsCache.mu.Unlock()
	})

	now := time.Now()
	entry := func(ttl time.Duration) azureModelsEntry {
		return azureModelsEntry{models: []ModelInfo{{ID: "m"}}, expires: now.Add(ttl)}
	}
	seed := func(t *testing.T, fill func(m map[string]azureModelsEntry)) {
		t.Helper()
		azureModelsCache.mu.Lock()
		azureModelsCache.m = map[string]azureModelsEntry{}
		fill(azureModelsCache.m)
		azureModelsCache.mu.Unlock()
	}

	t.Run("expired listings are pruned first and are enough", func(t *testing.T) {
		seed(t, func(m map[string]azureModelsEntry) {
			for i := 0; i < azureModelsCacheMax-2; i++ {
				m[fmt.Sprintf("live-%03d", i)] = entry(time.Hour)
			}
			m["stale"] = entry(-time.Minute)  // expired
			m["stale2"] = entry(-time.Second) // expired
		})
		azureModelsCache.mu.Lock()
		if len(azureModelsCache.m) != azureModelsCacheMax {
			t.Fatalf("seed size = %d, want %d", len(azureModelsCache.m), azureModelsCacheMax)
		}
		pruneAzureModelsCacheLocked(now)
		size := len(azureModelsCache.m)
		_, staleKept := azureModelsCache.m["stale"]
		_, stale2Kept := azureModelsCache.m["stale2"]
		_, liveKept := azureModelsCache.m["live-000"]
		azureModelsCache.mu.Unlock()
		if staleKept || stale2Kept {
			t.Error("expired listings must be pruned first")
		}
		if !liveKept {
			t.Error("a live listing must survive an expired-first prune")
		}
		if size != azureModelsCacheMax-2 {
			t.Errorf("size after prune = %d, want %d (only the expired dropped)", size, azureModelsCacheMax-2)
		}
	})

	t.Run("with nothing expired, the soonest-to-expire entry goes", func(t *testing.T) {
		seed(t, func(m map[string]azureModelsEntry) {
			for i := 0; i < azureModelsCacheMax-2; i++ {
				m[fmt.Sprintf("k-%03d", i)] = entry(time.Duration(i+1) * time.Minute)
			}
			m["soonest"] = entry(30 * time.Second)
			m["freshest"] = entry(3 * time.Hour)
		})
		azureModelsCache.mu.Lock()
		pruneAzureModelsCacheLocked(now)
		size := len(azureModelsCache.m)
		_, soonestKept := azureModelsCache.m["soonest"]
		_, freshestKept := azureModelsCache.m["freshest"]
		azureModelsCache.mu.Unlock()
		if soonestKept {
			t.Error("the soonest-to-expire entry is the one to drop")
		}
		if !freshestKept {
			t.Error("the freshest entry must survive")
		}
		if size != azureModelsCacheMax-1 {
			t.Errorf("size after prune = %d, want %d (exactly one dropped for one insert)", size, azureModelsCacheMax-1)
		}
	})
}
