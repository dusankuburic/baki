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

var githubModelsBaseURL = providerURL("GITHUB_MODELS_API_URL", "https://models.inference.ai.azure.com/chat/completions")

type GitHubModelsProvider struct {
	token  string
	client *http.Client
}

func NewGitHubModelsProvider(token string) *GitHubModelsProvider {
	return &GitHubModelsProvider{
		token:  token,
		client: sharedHTTPClient,
	}
}

func (g *GitHubModelsProvider) SupportsTools() bool { return true }

func (g *GitHubModelsProvider) ID() string           { return "github-models" }
func (g *GitHubModelsProvider) Name() string         { return "GitHub Models" }
func (g *GitHubModelsProvider) ContextLimit() int    { return 8192 }
func (g *GitHubModelsProvider) DefaultModel() string { return "gpt-4o" }
func (g *GitHubModelsProvider) FreeModel() string    { return "" }

func (g *GitHubModelsProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 0.0, OutputCostPerM: 0.0}
}

func (g *GitHubModelsProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by GitHub Models provider")
}

func (g *GitHubModelsProvider) EstimateTokens(text string) int {
	return EstimateTokens(text)
}

func (g *GitHubModelsProvider) Models() []ModelInfo { return catalogModels("github-models") }

func (g *GitHubModelsProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
		Tools:       toOpenAITools(req.Tools),
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
				return nil, rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return nil, fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
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
		ToolCalls:    openAIToolCallsToNeutral(parsed.Choices[0].Message.ToolCalls),
	}, nil
}

func (g *GitHubModelsProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := openAIRequest{
		Model:         req.Model,
		MaxTokens:     orDefault(req.MaxTokens, 4096),
		Temperature:   req.Temperature,
		Messages:      toOpenAIMessages(req.SystemPrompt, req.Messages),
		Tools:         toOpenAITools(req.Tools),
		Stream:        true,
		StreamOptions: usageStreamOptions,
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
			switch {
			case resp.StatusCode == 401:
				return ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return fmt.Errorf("github models stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
		}
		return fmt.Errorf("github models stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
