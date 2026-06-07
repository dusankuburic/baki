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

var xaiBaseURL = providerURL("XAI_API_URL", "https://api.x.ai/v1/chat/completions")

type XAIProvider struct {
	apiKey string
	client *http.Client
}

func NewXAIProvider(apiKey string) *XAIProvider {
	return &XAIProvider{
		apiKey: apiKey,
		client: sharedHTTPClient,
	}
}

func (x *XAIProvider) ID() string           { return "xai" }
func (x *XAIProvider) Name() string         { return "xAI (Grok)" }
func (x *XAIProvider) ContextLimit() int    { return 131072 }
func (x *XAIProvider) DefaultModel() string { return "grok-3-mini" }
func (x *XAIProvider) FreeModel() string    { return "" }

func (x *XAIProvider) PricePerMillionTokens() Pricing {
	// grok-3-mini list pricing (USD per 1M tokens).
	return Pricing{InputCostPerM: 0.3, OutputCostPerM: 0.5}
}

func (x *XAIProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by xAI provider")
}

func (x *XAIProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (x *XAIProvider) Models() []ModelInfo {
	return []ModelInfo{
		{
			ID: "grok-3", DisplayName: "Grok 3",
			ContextLimit: 131072,
			Pricing:      Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0},
		},
		{
			ID: "grok-3-mini", DisplayName: "Grok 3 Mini",
			ContextLimit: 131072,
			Pricing:      Pricing{InputCostPerM: 0.3, OutputCostPerM: 0.5},
		},
		{
			ID: "grok-2", DisplayName: "Grok 2",
			ContextLimit: 131072,
			Pricing:      Pricing{InputCostPerM: 2.0, OutputCostPerM: 10.0},
		},
	}
}

func (x *XAIProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", xaiBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+x.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := x.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("xai API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode != http.StatusOK {
		var apiErr openAIErrorResp
		if err := json.Unmarshal(respBody, &apiErr); err == nil {
			switch {
			case resp.StatusCode == 401:
				return nil, ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return nil, rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return nil, fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return nil, fmt.Errorf("xai API: %s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("xai API error (status %d)", resp.StatusCode)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse xai response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("xai returned no choices")
	}

	return &Response{
		Content:      parsed.Choices[0].Message.Content,
		TokensIn:     parsed.Usage.PromptTokens,
		TokensOut:    parsed.Usage.CompletionTokens,
		FinishReason: parsed.Choices[0].FinishReason,
	}, nil
}

func (x *XAIProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := openAIRequest{
		Model:         req.Model,
		MaxTokens:     orDefault(req.MaxTokens, 4096),
		Temperature:   req.Temperature,
		Messages:      toOpenAIMessages(req.SystemPrompt, req.Messages),
		Stream:        true,
		StreamOptions: usageStreamOptions,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", xaiBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}

	httpReq.Header.Set("Authorization", "Bearer "+x.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := x.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var apiErr openAIErrorResp
		if err := json.Unmarshal(errBody, &apiErr); err == nil && apiErr.Error.Message != "" {
			switch {
			case resp.StatusCode == 401:
				return ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return fmt.Errorf("xai stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
		}
		return fmt.Errorf("xai stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
