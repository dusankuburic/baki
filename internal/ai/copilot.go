package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

var copilotBaseURL = providerURL("COPILOT_API_URL", "https://api.githubcopilot.com/chat/completions")

const (
	copilotIntegrationID = "vscode-chat"
	copilotEditorVersion = "vscode/1.95.0"
	copilotPluginVersion = "copilot-chat/0.22.0"
)

func setCopilotHeaders(h http.Header) {
	h.Set("Copilot-Integration-Id", copilotIntegrationID)
	h.Set("Editor-Version", copilotEditorVersion)
	h.Set("Editor-Plugin-Version", copilotPluginVersion)
}

type CopilotProvider struct {
	tokenFn func(ctx context.Context) (string, error)
	client  *http.Client
}

// NewCopilotProvider creates a provider that uses a static token (manual PAT).
func NewCopilotProvider(token string) *CopilotProvider {
	return &CopilotProvider{
		tokenFn: func(_ context.Context) (string, error) { return token, nil },
		client:  sharedHTTPClient,
	}
}

// NewCopilotProviderWithAuth creates a provider that resolves a fresh session token via CopilotAuth.
func NewCopilotProviderWithAuth(auth *CopilotAuth, githubToken string) *CopilotProvider {
	return &CopilotProvider{
		tokenFn: func(ctx context.Context) (string, error) {
			return auth.GetSessionToken(ctx, githubToken)
		},
		client: sharedHTTPClient,
	}
}

func (p *CopilotProvider) SupportsTools() bool   { return false }

func (p *CopilotProvider) ID() string           { return "copilot" }
func (p *CopilotProvider) Name() string         { return "GitHub Copilot" }
func (p *CopilotProvider) ContextLimit() int    { return 128000 }
func (p *CopilotProvider) DefaultModel() string { return "gpt-4o" }
func (p *CopilotProvider) FreeModel() string    { return "" }

func (c *CopilotProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 0.0, OutputCostPerM: 0.0}
}

func (c *CopilotProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by Copilot provider")
}

func (c *CopilotProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (p *CopilotProvider) Models() []ModelInfo { return catalogModels("copilot") }

func (p *CopilotProvider) addHeaders(ctx context.Context, req *http.Request) error {
	token, err := p.tokenFn(ctx)
	if err != nil {
		return fmt.Errorf("get copilot token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	setCopilotHeaders(req.Header)
	return nil
}

func (p *CopilotProvider) Chat(ctx context.Context, req Request) (*Response, error) {
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

	httpReq, err := http.NewRequestWithContext(ctx, "POST", copilotBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := p.addHeaders(ctx, httpReq); err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("copilot API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr openAIErrorResp
		if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Error.Message != "" {
			switch {
			case resp.StatusCode == 401:
				return nil, ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return nil, rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return nil, fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return nil, fmt.Errorf("copilot API: %s", apiErr.Error.Message)
		}
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
		}
		return nil, fmt.Errorf("copilot API error (status %d)", resp.StatusCode)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse copilot response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("copilot returned no choices")
	}

	return &Response{
		Content:      parsed.Choices[0].Message.Content,
		TokensIn:     parsed.Usage.PromptTokens,
		TokensOut:    parsed.Usage.CompletionTokens,
		FinishReason: parsed.Choices[0].FinishReason,
	}, nil
}

func (p *CopilotProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
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

	httpReq, err := http.NewRequestWithContext(ctx, "POST", copilotBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	if err := p.addHeaders(ctx, httpReq); err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
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
			return fmt.Errorf("copilot stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
		}
		return fmt.Errorf("copilot stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
