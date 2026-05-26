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

var githubModelsBaseURL = "https://models.inference.ai.azure.com/chat/completions"

type GitHubModelsProvider struct {
	token  string
	client *http.Client
}

func NewGitHubModelsProvider(token string) *GitHubModelsProvider {
	return &GitHubModelsProvider{
		token:  token,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (g *GitHubModelsProvider) ID() string          { return "github-models" }
func (g *GitHubModelsProvider) Name() string        { return "GitHub Models" }
func (g *GitHubModelsProvider) ContextLimit() int    { return 8192 }
func (g *GitHubModelsProvider) DefaultModel() string { return "gpt-4o" }
func (g *GitHubModelsProvider) FreeModel() string    { return "" }

func (g *GitHubModelsProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 0, OutputCostPerM: 0}
}

func (g *GitHubModelsProvider) EstimateTokens(text string) int {
	return EstimateTokens(text)
}

func (g *GitHubModelsProvider) Models() []ModelInfo {
	// GitHub Models free tier enforces an 8 192-token request limit for most
	// models regardless of the model's native context window. These limits
	// are used to cap the context budget so the API never returns a 413.
	const ghFreeLimit = 8192
	return []ModelInfo{
		{
			ID: "gpt-4o", DisplayName: "GPT-4o",
			ContextLimit: ghFreeLimit,
			Pricing:      Pricing{InputCostPerM: 2.5, OutputCostPerM: 10.0},
		},
		{
			ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini",
			ContextLimit: ghFreeLimit,
			Pricing:      Pricing{InputCostPerM: 0.15, OutputCostPerM: 0.6},
		},
		{
			ID: "Meta-Llama-3.3-70B-Instruct", DisplayName: "Llama 3.3 70B",
			ContextLimit: ghFreeLimit,
			Pricing:      Pricing{InputCostPerM: 0.7, OutputCostPerM: 0.9},
		},
		{
			ID: "Mistral-large-2411", DisplayName: "Mistral Large",
			ContextLimit: ghFreeLimit,
			Pricing:      Pricing{InputCostPerM: 2.0, OutputCostPerM: 6.0},
		},
		{
			ID: "Phi-4", DisplayName: "Phi-4",
			ContextLimit: ghFreeLimit,
			Pricing:      Pricing{InputCostPerM: 0.07, OutputCostPerM: 0.14},
		},
		{
			ID: "DeepSeek-V3-0324", DisplayName: "DeepSeek V3",
			ContextLimit: ghFreeLimit,
			Pricing:      Pricing{InputCostPerM: 0.49, OutputCostPerM: 0.94},
		},
		{
			ID: "ai21-jamba-1.5-large", DisplayName: "Jamba 1.5 Large",
			ContextLimit: ghFreeLimit,
			Pricing:      Pricing{InputCostPerM: 2.0, OutputCostPerM: 8.0},
		},
	}
}

func (g *GitHubModelsProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
	}
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", githubModelsBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+g.token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-ms-model-mesh-model-id", req.Model)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("github models API request: %w", err)
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
				return nil, fmt.Errorf("github models server error: %s", apiErr.Error.Message)
			}
			return nil, fmt.Errorf("github models API: %s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("github models API error (status %d)", resp.StatusCode)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse github models response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("github models returned no choices")
	}

	return &Response{
		Content:      parsed.Choices[0].Message.Content,
		TokensIn:     parsed.Usage.PromptTokens,
		TokensOut:    parsed.Usage.CompletionTokens,
		FinishReason: parsed.Choices[0].FinishReason,
	}, nil
}

func (g *GitHubModelsProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
		Stream:      true,
	}
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", githubModelsBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}

	httpReq.Header.Set("Authorization", "Bearer "+g.token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-ms-model-mesh-model-id", req.Model)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var apiErr openAIErrorResp
		if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
			return fmt.Errorf("github models stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return fmt.Errorf("github models stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
