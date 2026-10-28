package ai

// R-provider-25: an OpenAI-family 403 is the provider refusing the account (region, project permission, billing),
// not a wrong key; a 429 that says the quota or billing is exhausted is the balance error, not a rate limit to
// wait out.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func openAIAt(t *testing.T, status int, body string) (*OpenAIProvider, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewOpenAIProvider("sk-test")
	url := srv.URL + "/chat/completions"
	p.baseURL = &url
	p.client, p.longWaitClient = srv.Client(), nil
	return p, &hits
}

func TestOpenAI_403IsAnAccessRefusal(t *testing.T) {
	p, _ := openAIAt(t, http.StatusForbidden, `{"error":{"message":"Country, region, or territory not supported","code":"unsupported_country_region_territory"}}`)
	_, err := p.Chat(context.Background(), compatChatReq())
	if !errors.Is(err, ErrProviderAccessDenied) || !errors.Is(err, ErrProviderPermissionDenied) {
		t.Fatalf("403: err = %v, want an access refusal (ErrProviderAccessDenied, ErrProviderPermissionDenied)", err)
	}
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Error("an access refusal must keep the auth failure's retry and breaker rules (ErrApiKeyInvalid)")
	}
	if !strings.Contains(err.Error(), "Country, region, or territory not supported") {
		t.Errorf("403: err = %q, want the vendor's message", err)
	}
	err = p.Stream(context.Background(), compatChatReq(), func(Chunk) {})
	if !errors.Is(err, ErrProviderAccessDenied) {
		t.Errorf("403 on Stream: err = %v, want ErrProviderAccessDenied", err)
	}
}

func TestOpenAI_429QuotaIsBalance(t *testing.T) {
	p, hits := openAIAt(t, http.StatusTooManyRequests, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`)
	rp := NewRetryingProviderWithConfig(p, 3, time.Millisecond)
	_, err := rp.Chat(context.Background(), compatChatReq())
	if !errors.Is(err, ErrInsufficientBalance) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("quota 429: err = %v, want ErrInsufficientBalance and not ErrRateLimited", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("quota 429: %d attempts, want 1", n)
	}

	for _, body := range []string{
		`{"error":{"message":"Rate limit reached for gpt-4o on tokens per min","code":"rate_limit_exceeded"}}`,
		// The lower tiers' per-minute limit links the billing page: still a rate limit.
		`{"error":{"message":"Rate limit reached for gpt-4o in organization org-x on requests per min (RPM): Limit 3. Please add a payment method to your account to increase your rate limit. Visit https://platform.openai.com/account/billing to add a payment method.","type":"requests","code":"rate_limit_exceeded"}}`,
	} {
		p, _ = openAIAt(t, http.StatusTooManyRequests, body)
		if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrRateLimited) {
			t.Errorf("a rate-limit 429 (%.40s…): err = %v, want ErrRateLimited", body, err)
		}
	}
}

func TestGLM_BalanceCodeUnchanged(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"余额不足或无可用资源包,请充值。","code":"1113"}}`))
	}))
	defer srv.Close()
	p := NewGLMProvider("k")
	url := srv.URL + "/chat/completions"
	p.baseURL = &url
	p.client, p.longWaitClient = srv.Client(), nil
	if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrInsufficientBalance) {
		t.Errorf("GLM 1113: err = %v, want ErrInsufficientBalance", err)
	}
}
