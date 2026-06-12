package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type openaiBase struct {
	apiKey        string
	client        *http.Client
	baseURL       *string
	providerLabel string
	extraHeaders  func(req *http.Request, model string)
	handle429     func(resp *http.Response, apiErr openAIErrorResp) error
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

	httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
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

	httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
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
		return fmt.Errorf("%s stream error (status %d): %s", b.providerLabel, resp.StatusCode, apiErr.Error.Message)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
	}
	return fmt.Errorf("%s stream error (status %d)", b.providerLabel, resp.StatusCode)
}
