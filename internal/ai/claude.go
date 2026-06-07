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

var claudeAPIURL = providerURL("CLAUDE_API_URL", "https://api.anthropic.com/v1/messages")
var claudeAPIVersion = "2023-06-01"

type ClaudeProvider struct {
	apiKey string
	client *http.Client
}

func NewClaudeProvider(apiKey string) *ClaudeProvider {
	return &ClaudeProvider{
		apiKey: apiKey,
		client: sharedHTTPClient,
	}
}

func (c *ClaudeProvider) ID() string          { return "claude" }
func (c *ClaudeProvider) Name() string        { return "Claude" }
func (c *ClaudeProvider) ContextLimit() int    { return 200000 }
func (c *ClaudeProvider) DefaultModel() string { return "claude-sonnet-4-5" }
func (c *ClaudeProvider) FreeModel() string    { return "" }

func (c *ClaudeProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}
}

func (c *ClaudeProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by Claude provider")
}

func (c *ClaudeProvider) EstimateTokens(text string) int {
	return EstimateTokensClaude(text)
}

func (c *ClaudeProvider) Models() []ModelInfo {
	return []ModelInfo{
		{
			ID: "claude-opus-4-5", DisplayName: "Claude Opus 4.5",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 15.0, OutputCostPerM: 75.0},
		},
		{
			ID: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0},
		},
		{
			ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5",
			ContextLimit: 200000,
			Pricing:      Pricing{InputCostPerM: 0.8, OutputCostPerM: 4.0},
		},
	}
}

type claudeRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature,omitempty"`
	System      string          `json:"system,omitempty"`
	Messages    []claudeMessage `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Content    []claudeContent  `json:"content"`
	Model      string           `json:"model"`
	StopReason string           `json:"stop_reason"`
	Usage      claudeUsage      `json:"usage"`
}

type claudeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type claudeErrorResp struct {
	Error claudeErrorDetail `json:"error"`
}

type claudeErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func toClaudeMessages(msgs []Message) []claudeMessage {
	out := make([]claudeMessage, len(msgs))
	for i, m := range msgs {
		out[i] = claudeMessage{Role: m.Role, Content: m.Content}
	}
	return out
}

func (c *ClaudeProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := claudeRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		System:      req.SystemPrompt,
		Messages:    toClaudeMessages(req.Messages),
	}
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", claudeAPIURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", claudeAPIVersion)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode != http.StatusOK {
		var apiErr claudeErrorResp
		if err := json.Unmarshal(respBody, &apiErr); err == nil {
			switch {
			case resp.StatusCode == 401:
				return nil, ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return nil, rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return nil, fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return nil, fmt.Errorf("claude API: %s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("claude API error (status %d)", resp.StatusCode)
	}

	var parsed claudeResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse claude response: %w", err)
	}
	if len(parsed.Content) == 0 {
		return nil, errors.New("claude returned no content")
	}

	return &Response{
		Content:      parsed.Content[0].Text,
		TokensIn:     parsed.Usage.InputTokens,
		TokensOut:    parsed.Usage.OutputTokens,
		FinishReason: parsed.StopReason,
	}, nil
}

func (c *ClaudeProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := claudeRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		System:      req.SystemPrompt,
		Messages:    toClaudeMessages(req.Messages),
		Stream:      true,
	}
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", claudeAPIURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}

	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", claudeAPIVersion)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var apiErr claudeErrorResp
		if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
			switch {
			case resp.StatusCode == 401:
				return ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return fmt.Errorf("claude stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
		}
		return fmt.Errorf("claude stream error (status %d)", resp.StatusCode)
	}

	return parseClaudeSSE(resp.Body, onChunk)
}
