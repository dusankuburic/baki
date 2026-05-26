package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const copilotBaseURL = "https://api.githubcopilot.com/chat/completions"

type CopilotProvider struct {
	token  string
	client *http.Client
}

func NewCopilotProvider(token string) *CopilotProvider {
	return &CopilotProvider{
		token:  token,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *CopilotProvider) ID() string          { return "copilot" }
func (p *CopilotProvider) Name() string        { return "GitHub Copilot" }
func (p *CopilotProvider) ContextLimit() int   { return 128000 }
func (p *CopilotProvider) DefaultModel() string { return "gpt-4o" }
func (p *CopilotProvider) FreeModel() string   { return "" }

func (p *CopilotProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 0, OutputCostPerM: 0}
}

func (p *CopilotProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (p *CopilotProvider) Models() []ModelInfo {
	return []ModelInfo{
		{
			ID: "gpt-4o", DisplayName: "GPT-4o",
			ContextLimit: 128000,
			Pricing:      Pricing{InputCostPerM: 0, OutputCostPerM: 0},
		},
		{
			ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini",
			ContextLimit: 128000,
			Pricing:      Pricing{InputCostPerM: 0, OutputCostPerM: 0},
		},
		{
			ID: "claude-3.5-sonnet", DisplayName: "Claude 3.5 Sonnet",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 0, OutputCostPerM: 0},
		},
	}
}

func (p *CopilotProvider) addHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	req.Header.Set("Editor-Version", "vscode/1.95.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.22.0")
}

func (p *CopilotProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
	}
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", copilotBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	p.addHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("copilot API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode != http.StatusOK {
		var apiErr openAIErrorResp
		if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Error.Message != "" {
			if resp.StatusCode == 401 {
				return nil, ErrApiKeyInvalid
			}
			if resp.StatusCode == 429 {
				return nil, ErrRateLimited
			}
			return nil, fmt.Errorf("copilot API: %s", apiErr.Error.Message)
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
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", copilotBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	p.addHeaders(httpReq)
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
			return fmt.Errorf("copilot stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return fmt.Errorf("copilot stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
