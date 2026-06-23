package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

var openAIBaseURL = providerURL("OPENAI_API_URL", "https://api.openai.com/v1/chat/completions")

type OpenAIProvider struct {
	openaiBase
}

func NewOpenAIProvider(apiKey string) *OpenAIProvider {
	return &OpenAIProvider{
		openaiBase: openaiBase{
			apiKey:        apiKey,
			client:        sharedHTTPClient,
			baseURL:       &openAIBaseURL,
			providerLabel: "openai",
		},
	}
}

func (o *OpenAIProvider) SupportsTools() bool { return true }

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

func (o *OpenAIProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return catalogModels("openai"), nil
}

func (o *OpenAIProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	return o.chat(ctx, req)
}

func (o *OpenAIProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return o.stream(ctx, req, onChunk)
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
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

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
