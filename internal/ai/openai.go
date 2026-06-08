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

var openAIBaseURL = providerURL("OPENAI_API_URL", "https://api.openai.com/v1/chat/completions")

type OpenAIProvider struct {
	apiKey string
	client *http.Client
}

func NewOpenAIProvider(apiKey string) *OpenAIProvider {
	return &OpenAIProvider{
		apiKey: apiKey,
		client: sharedHTTPClient,
	}
}

func (o *OpenAIProvider) SupportsTools() bool  { return true }

func (o *OpenAIProvider) ID() string           { return "openai" }
func (o *OpenAIProvider) Name() string         { return "OpenAI" }
func (o *OpenAIProvider) ContextLimit() int    { return 128000 }
func (o *OpenAIProvider) DefaultModel() string { return "gpt-4o" }
func (o *OpenAIProvider) FreeModel() string    { return "" }

func (o *OpenAIProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 2.5, OutputCostPerM: 10.0}
}

func (o *OpenAIProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (o *OpenAIProvider) Models() []ModelInfo { return catalogModels("openai") }

type openAIRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   float64         `json:"temperature,omitempty"`
	Messages      []openAIMessage `json:"messages"`
	Tools         []openAITool    `json:"tools,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

type openAITool struct {
	Type     string             `json:"type"` // always "function"
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// openAIToolCall is a function call in an assistant message / response. Arguments
// is a JSON-encoded string per the OpenAI schema.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// usageStreamOptions asks the provider to include token-usage accounting on a
// streaming response (the standard OpenAI `stream_options` field). Without it,
// OpenAI-compatible streams carry no usage, so the audited provider records a
// $0 cost and the daily budget can never trip. Supported by OpenAI, xAI, GLM
// and GitHub Models; intentionally NOT sent to Copilot (which bills at $0 here
// and whose endpoint may reject the field).
var usageStreamOptions = &streamOptions{IncludeUsage: true}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
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
		om := openAIMessage{Role: m.Role, Content: m.Content}
		if m.Role == "tool" {
			om.ToolCallID = m.ToolCallID
		}
		for _, tc := range m.ToolCalls {
			oc := openAIToolCall{ID: tc.ID, Type: "function"}
			oc.Function.Name = tc.Name
			oc.Function.Arguments = string(tc.Input)
			if oc.Function.Arguments == "" {
				oc.Function.Arguments = "{}"
			}
			om.ToolCalls = append(om.ToolCalls, oc)
		}
		out = append(out, om)
	}
	return out
}

func toOpenAITools(tools []ToolDefinition) []openAITool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openAITool, len(tools))
	for i, t := range tools {
		out[i] = openAITool{
			Type: "function",
			Function: openAIToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		}
	}
	return out
}

// openAIToolCallsToNeutral converts response tool_calls to provider-neutral
// ToolCalls (arguments string → raw JSON).
func openAIToolCallsToNeutral(calls []openAIToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		args := c.Function.Arguments
		if args == "" {
			args = "{}"
		}
		out = append(out, ToolCall{ID: c.ID, Name: c.Function.Name, Input: json.RawMessage(args)})
	}
	return out
}

func (o *OpenAIProvider) Chat(ctx context.Context, req Request) (*Response, error) {
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

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
		ToolCalls:    openAIToolCallsToNeutral(parsed.Choices[0].Message.ToolCalls),
	}, nil
}

func (o *OpenAIProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	reqBody := map[string]interface{}{
		"model": "text-embedding-3-small",
		"input": text,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := strings.Replace(openAIBaseURL, "/chat/completions", "/embeddings", 1)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai embeddings error (status %d)", resp.StatusCode)
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

func (o *OpenAIProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
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
			switch {
			case resp.StatusCode == 401:
				return ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return fmt.Errorf("openai stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
		}
		return fmt.Errorf("openai stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
