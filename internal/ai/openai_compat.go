package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type openaiBase struct {
	// apiKey is the static bearer token, used when tokenFn is nil. Providers
	// with a fixed API key (OpenAI, GLM, GitHub Models, …) set this.
	apiKey string
	// tokenFn resolves the bearer token per-call, for providers whose token is
	// dynamic and can expire/require exchange (Copilot's session token). When
	// set, it takes precedence over apiKey. Errors propagate to the caller
	// instead of reaching the network — a stale/invalid token never becomes a
	// wasted HTTP round trip.
	tokenFn       func(ctx context.Context) (string, error)
	client        *http.Client
	baseURL       *string
	providerLabel string
	// embeddingModel is the model name sent to the OpenAI-compatible /embeddings
	// endpoint. Empty means this provider does not expose embeddings and embed()
	// returns a "not supported" error (no /embeddings call is attempted).
	embeddingModel string
	extraHeaders   func(req *http.Request, model string)
	handle429      func(resp *http.Response, apiErr openAIErrorResp) error
}

// resolveToken returns the bearer token for a request: tokenFn when set
// (dynamic token, e.g. Copilot's session exchange), else the static apiKey.
func (b *openaiBase) resolveToken(ctx context.Context) (string, error) {
	if b.tokenFn != nil {
		return b.tokenFn(ctx)
	}
	return b.apiKey, nil
}

func (b *openaiBase) chat(ctx context.Context, req Request) (*Response, error) {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
		Tools:       toOpenAITools(req.Tools),
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", *b.baseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	token, err := b.resolveToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get %s token: %w", b.providerLabel, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	if b.extraHeaders != nil {
		b.extraHeaders(httpReq, req.Model)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s API request: %w", b.providerLabel, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, b.handleChatError(resp, respBody)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse %s response: %w", b.providerLabel, err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New(b.providerLabel + " returned no choices")
	}

	return &Response{
		Content:      parsed.Choices[0].Message.Content,
		TokensIn:     parsed.Usage.PromptTokens,
		TokensOut:    parsed.Usage.CompletionTokens,
		FinishReason: parsed.Choices[0].FinishReason,
		ToolCalls:    openAIToolCallsToNeutral(parsed.Choices[0].Message.ToolCalls),
	}, nil
}

func (b *openaiBase) handleChatError(resp *http.Response, respBody []byte) error {
	var apiErr openAIErrorResp
	if err := json.Unmarshal(respBody, &apiErr); err == nil {
		switch {
		case resp.StatusCode == 401:
			return ErrApiKeyInvalid
		case resp.StatusCode == 429:
			if b.handle429 != nil {
				if handled := b.handle429(resp, apiErr); handled != nil {
					return handled
				}
			}
			return rateLimitErr(resp)
		case resp.StatusCode >= 500:
			return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
		}
		if err := detectContextLimitError(resp.StatusCode, apiErr.Error.Message); err != nil {
			return err
		}
		return fmt.Errorf("%s API: %s", b.providerLabel, apiErr.Error.Message)
	}
	return fmt.Errorf("%s API error (status %d)", b.providerLabel, resp.StatusCode)
}

func (b *openaiBase) stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := openAIRequest{
		Model:         req.Model,
		MaxTokens:     orDefault(req.MaxTokens, 4096),
		Temperature:   req.Temperature,
		Messages:      toOpenAIMessages(req.SystemPrompt, req.Messages),
		Tools:         toOpenAITools(req.Tools),
		Stream:        true,
		StreamOptions: usageStreamOptions,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", *b.baseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}

	token, err := b.resolveToken(ctx)
	if err != nil {
		return fmt.Errorf("get %s token: %w", b.providerLabel, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if b.extraHeaders != nil {
		b.extraHeaders(httpReq, req.Model)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return b.handleStreamError(resp, errBody)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}

func (b *openaiBase) handleStreamError(resp *http.Response, respBody []byte) error {
	var apiErr openAIErrorResp
	if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Error.Message != "" {
		switch {
		case resp.StatusCode == 401:
			return ErrApiKeyInvalid
		case resp.StatusCode == 429:
			if b.handle429 != nil {
				if handled := b.handle429(resp, apiErr); handled != nil {
					return handled
				}
			}
			return rateLimitErr(resp)
		case resp.StatusCode >= 500:
			return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
		}
		if err := detectContextLimitError(resp.StatusCode, apiErr.Error.Message); err != nil {
			return err
		}
		return fmt.Errorf("%s stream error (status %d): %s", b.providerLabel, resp.StatusCode, apiErr.Error.Message)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
	}
	return fmt.Errorf("%s stream error (status %d)", b.providerLabel, resp.StatusCode)
}

// embed calls the OpenAI-compatible /embeddings endpoint. It is shared by every
// provider whose API mirrors OpenAI's shape (OpenAI, GLM, GitHub Models, …).
// The embeddings URL is derived from the provider's chat base URL by replacing
// the trailing "/chat/completions" with "/embeddings". Returns a "not supported"
// error when embeddingModel is empty (the provider has no embeddings model), so
// no network call is attempted for providers known to lack the endpoint.
func (b *openaiBase) embed(ctx context.Context, text []string) ([][]float32, error) {
	if b.embeddingModel == "" {
		return nil, fmt.Errorf("embeddings not supported by %s provider", b.providerLabel)
	}

	reqBody, err := json.Marshal(map[string]any{
		"model": b.embeddingModel,
		"input": text,
	})
	if err != nil {
		return nil, err
	}

	embedURL := strings.Replace(*b.baseURL, "/chat/completions", "/embeddings", 1)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", embedURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	token, err := b.resolveToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get %s token: %w", b.providerLabel, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	if b.extraHeaders != nil {
		b.extraHeaders(httpReq, b.embeddingModel)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s embeddings error (status %d)", b.providerLabel, resp.StatusCode)
	}

	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	res := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		res[i] = d.Embedding
	}
	return res, nil
}
