package ai

import (
	"context"
	"encoding/json"
)

// openAIDefaultURL is OpenAI's own chat-completions endpoint; OPENAI_API_URL
// overrides it (compatible servers and proxies).
const openAIDefaultURL = "https://api.openai.com/v1/chat/completions"

var openAIBaseURL = providerURL("OPENAI_API_URL", openAIDefaultURL)

type OpenAIProvider struct {
	openaiBase
}

func NewOpenAIProvider(apiKey string) *OpenAIProvider {
	return &OpenAIProvider{
		openaiBase: openaiBase{
			apiKey:                apiKey,
			client:                sharedHTTPClient,
			baseURL:               &openAIBaseURL,
			providerLabel:         "openai",
			embeddingModel:        "text-embedding-3-small",
			embeddingModelDefault: "text-embedding-3-small",
			maxTokensParam:        openAIMaxTokensParam(openAIBaseURL),
		},
	}
}

// openAIMaxTokensParam picks the completion-cap field for the configured endpoint. OpenAI itself gets
// max_completion_tokens (its reasoning models reject max_tokens); an OPENAI_API_URL override keeps max_tokens
// — compatible servers (Ollama, llama-cpp-python, older vLLM) silently IGNORE the successor, so no 400 would
// ever trigger send()'s repair and generation would run uncapped. A proxy in front of real OpenAI serving a
// reasoning model rejects max_tokens with unsupported_parameter, and send() swaps it once.
func openAIMaxTokensParam(baseURL string) string {
	if baseURL == openAIDefaultURL {
		return maxTokensParamCompletion
	}
	return maxTokensParamLegacy
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
	return o.embed(ctx, text)
}

type openAIRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	// MaxCompletionTokens is the successor of max_tokens: OpenAI's reasoning models and Azure Foundry
	// deployments of them reject max_tokens outright. openaiBase.maxTokensParam picks which is sent.
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	// Temperature is a pointer so an explicit 0 (Request.TemperatureSet — deterministic sampling) goes out
	// on the wire; nil omits the field, which a zero Temperature without TemperatureSet still means.
	Temperature   *float64        `json:"temperature,omitempty"`
	Messages      []openAIMessage `json:"messages"`
	Tools         []openAITool    `json:"tools,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
	// ReasoningEffort is the user-chosen reasoning depth (low|medium|high),
	// omitted for the provider default. The gpt-5.6 tools repair
	// replaces it with "none" in sendWith's generic map.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
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
	// ReasoningContent is response-only (message.reasoning_content — DeepSeek-R1, GLM, Grok mini on
	// Foundry): it becomes Response.Reasoning, never Content, and is omitted from requests.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIResponse struct {
	ID string `json:"id"`
	// Model is the concrete model that served the request (a versioned name, or the model routed behind a
	// router deployment); carried to Response.ServedModel for pricing.
	Model   string `json:"model"`
	Choices []struct {
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// The cached slice of PromptTokens (a breakdown, never an addition).
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// openAIErrorResp is the OpenAI error envelope ({"error":{…}}); decode with decodeOpenAIError, which also
// reads the Azure Model Inference layer's top-level shape into the same fields.
type openAIErrorResp struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		// Param names the offending request field on unsupported_parameter /
		// unsupported_value rejections ("max_tokens", "temperature").
		Param string `json:"param"`
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
		if m.Role == "assistant" {
			// Backstop for conversations stored before inline reasoning was
			// split off the answer: never re-send a <think> block as history.
			om.Content = stripLeadingThink(m.Content)
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
