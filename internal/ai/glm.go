package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var glmBaseURL = "https://api.z.ai/api/paas/v4/chat/completions"

type GLMProvider struct {
	apiKey string
	client *http.Client
}

func NewGLMProvider(apiKey string) *GLMProvider {
	return &GLMProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (g *GLMProvider) ID() string          { return "glm" }
func (g *GLMProvider) Name() string        { return "GLM (z.ai)" }
func (g *GLMProvider) ContextLimit() int   { return 200000 }
func (g *GLMProvider) DefaultModel() string { return "glm-5.1" }
func (g *GLMProvider) FreeModel() string    { return "glm-4.7-flash" }

func (g *GLMProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4}
}

func (g *GLMProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (g *GLMProvider) Models() []ModelInfo {
	return []ModelInfo{
		{
			ID: "glm-5.1", DisplayName: "GLM-5.1",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4},
		},
		{
			ID: "glm-5", DisplayName: "GLM-5",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 1.0, OutputCostPerM: 3.2},
		},
		{
			ID: "glm-5-turbo", DisplayName: "GLM-5 Turbo",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 0.8, OutputCostPerM: 2.4},
		},
		{
			ID: "glm-4.7", DisplayName: "GLM-4.7",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 0.6, OutputCostPerM: 2.2},
		},
		{
			ID: "glm-4.7-flashx", DisplayName: "GLM-4.7 FlashX",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 0.2, OutputCostPerM: 0.6},
		},
		{
			ID: "glm-4.7-flash", DisplayName: "GLM-4.7 Flash (Free)",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 0, OutputCostPerM: 0},
		},
	}
}

func (g *GLMProvider) Chat(ctx context.Context, req Request) (*Response, error) {
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

	httpReq, err := http.NewRequestWithContext(ctx, "POST", glmBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("glm API request: %w", err)
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
				return nil, ErrRateLimited
			case resp.StatusCode >= 500:
				return nil, fmt.Errorf("glm server error: %s", apiErr.Error.Message)
			case apiErr.Error.Code == "1113":
				return nil, ErrInsufficientBalance
			}
			return nil, fmt.Errorf("glm API: %s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("glm API error (status %d)", resp.StatusCode)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse glm response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("glm returned no choices")
	}

	return &Response{
		Content:      parsed.Choices[0].Message.Content,
		TokensIn:     parsed.Usage.PromptTokens,
		TokensOut:    parsed.Usage.CompletionTokens,
		FinishReason: parsed.Choices[0].FinishReason,
	}, nil
}

func (g *GLMProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
		Stream:      true,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", glmBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}

	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var apiErr openAIErrorResp
		if err := json.Unmarshal(errBody, &apiErr); err == nil && apiErr.Error.Message != "" {
			return fmt.Errorf("glm stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return fmt.Errorf("glm stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
