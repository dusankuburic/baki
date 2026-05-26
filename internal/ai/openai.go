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

var openAIBaseURL = "https://api.openai.com/v1/chat/completions"

type OpenAIProvider struct {
	apiKey string
	client *http.Client
}

func NewOpenAIProvider(apiKey string) *OpenAIProvider {
	return &OpenAIProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (o *OpenAIProvider) ID() string          { return "openai" }
func (o *OpenAIProvider) Name() string        { return "OpenAI" }
func (o *OpenAIProvider) ContextLimit() int    { return 128000 }
func (o *OpenAIProvider) DefaultModel() string { return "gpt-4o" }
func (o *OpenAIProvider) FreeModel() string    { return "" }

func (o *OpenAIProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 2.5, OutputCostPerM: 10.0}
}

func (o *OpenAIProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (o *OpenAIProvider) Models() []ModelInfo {
	return []ModelInfo{
		{
			ID: "gpt-4o", DisplayName: "GPT-4o",
			ContextLimit: 128000,
			Pricing:      Pricing{InputCostPerM: 2.5, OutputCostPerM: 10.0},
		},
		{
			ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini",
			ContextLimit: 128000,
			Pricing:      Pricing{InputCostPerM: 0.15, OutputCostPerM: 0.6},
		},
		{
			ID: "gpt-4-turbo", DisplayName: "GPT-4 Turbo",
			ContextLimit: 128000,
			Pricing:      Pricing{InputCostPerM: 10.0, OutputCostPerM: 30.0},
		},
	}
}

type openAIRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	Messages    []openAIMessage `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type openAIErrorResp struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func toOpenAIMessages(systemPrompt string, msgs []Message) []openAIMessage {
	var out []openAIMessage
	if systemPrompt != "" {
		out = append(out, openAIMessage{Role: "system", Content: systemPrompt})
	}
	for _, m := range msgs {
		out = append(out, openAIMessage{Role: m.Role, Content: m.Content})
	}
	return out
}

func (o *OpenAIProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
	}
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", openAIBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai API request: %w", err)
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
				return nil, fmt.Errorf("openai server error: %s", apiErr.Error.Message)
			}
			return nil, fmt.Errorf("openai API: %s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("openai API error (status %d)", resp.StatusCode)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("openai returned no choices")
	}

	return &Response{
		Content:      parsed.Choices[0].Message.Content,
		TokensIn:     parsed.Usage.PromptTokens,
		TokensOut:    parsed.Usage.CompletionTokens,
		FinishReason: parsed.Choices[0].FinishReason,
	}, nil
}

func (o *OpenAIProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := openAIRequest{
		Model:       req.Model,
		MaxTokens:   orDefault(req.MaxTokens, 4096),
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
		Stream:      true,
	}
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", openAIBaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}

	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var apiErr openAIErrorResp
		if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
			return fmt.Errorf("openai stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return fmt.Errorf("openai stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
