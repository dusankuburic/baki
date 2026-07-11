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

type GeminiProvider struct {
	apiKey string
	client *http.Client
}

func NewGeminiProvider(apiKey string) *GeminiProvider {
	return &GeminiProvider{
		apiKey: apiKey,
		client: sharedHTTPClient,
	}
}

func (g *GeminiProvider) SupportsTools() bool { return true }

func (g *GeminiProvider) ID() string           { return "gemini" }
func (g *GeminiProvider) Name() string         { return "Gemini" }
func (g *GeminiProvider) ContextLimit() int    { return 1048576 }
func (g *GeminiProvider) DefaultModel() string { return "gemini-2.5-pro" }
func (g *GeminiProvider) FreeModel() string    { return "" }

func (g *GeminiProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 1.25, OutputCostPerM: 10.0}
}

// geminiEmbedBatchSize caps how many texts ride in one batchEmbedContents
// request. Gemini rejects oversized batches (documented ~100/call), and callers
// embed up to maxKnowledgeChunks (500) documents at once, so Embed splits the
// input into sub-batches and concatenates the results in order.
const geminiEmbedBatchSize = 100

func (g *GeminiProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	res := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += geminiEmbedBatchSize {
		end := min(start+geminiEmbedBatchSize, len(texts))
		batch, err := g.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		res = append(res, batch...)
	}
	return res, nil
}

// embedBatch embeds a single sub-batch (≤ geminiEmbedBatchSize texts).
func (g *GeminiProvider) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	// Gemini's embeddings API differs from OpenAI's shape: a single
	// batchEmbedContents POST carries an array of requests, each wrapping the
	// text in a content.parts[].text object. taskType RETRIEVAL_DOCUMENT is the
	// recommended setting for documents that will be stored and searched.
	const embedModel = "gemini-embedding-001"
	fullModel := "models/" + embedModel

	type embedRequest struct {
		Model    string        `json:"model"`
		Content  geminiContent `json:"content"`
		TaskType string        `json:"taskType"`
	}
	reqs := make([]embedRequest, len(texts))
	for i, t := range texts {
		reqs[i] = embedRequest{
			Model:    fullModel,
			Content:  geminiContent{Parts: []geminiPart{{Text: t}}},
			TaskType: "RETRIEVAL_DOCUMENT",
		}
	}

	body, err := json.Marshal(map[string]any{"requests": reqs})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:batchEmbedContents", geminiBaseHost, embedModel)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini embeddings error (status %d)", resp.StatusCode)
	}

	var parsed struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	res := make([][]float32, len(parsed.Embeddings))
	for i, e := range parsed.Embeddings {
		res[i] = e.Values
	}
	return res, nil
}

func (g *GeminiProvider) EstimateTokens(text string) int {
	return EstimateTokensGemini(text)
}

func (g *GeminiProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return catalogModels("gemini"), nil
}

var geminiURL = defaultGeminiURL

var geminiBaseHost = providerURL("GEMINI_API_URL", "https://generativelanguage.googleapis.com")

func defaultGeminiURL(model, _ /*apiKey*/, action string) string {
	base := fmt.Sprintf("%s/v1beta/models/%s:%s", geminiBaseHost, model, action)
	if action == "streamGenerateContent" {
		return base + "?alt=sse"
	}
	return base
}

type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
	Tools             []geminiToolDecl `json:"tools,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type geminiFunctionResponse struct {
	Name     string      `json:"name"`
	Response interface{} `json:"response"`
}

type geminiGenConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

type geminiFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiToolDecl struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
			Role  string       `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type geminiErrorResp struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func toGeminiContents(msgs []Message) []geminiContent {
	callNames := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			callNames[tc.ID] = tc.Name
		}
	}

	var out []geminiContent
	for _, m := range msgs {
		switch {
		case m.Role == "tool":
			name := callNames[m.ToolCallID]
			resp := map[string]string{"result": m.Content}
			out = append(out, geminiContent{
				Role: "user",
				Parts: []geminiPart{{
					FunctionResponse: &geminiFunctionResponse{
						Name:     name,
						Response: resp,
					},
				}},
			})

		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			parts := make([]geminiPart, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var args map[string]interface{}
				if len(tc.Input) > 0 {
					_ = json.Unmarshal(tc.Input, &args) // best-effort; args stays nil on bad input
				}
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: tc.Name,
						Args: args,
					},
				})
			}
			out = append(out, geminiContent{
				Role:  "model",
				Parts: parts,
			})

		default:
			role := "user"
			if m.Role == "assistant" {
				role = "model"
			}
			out = append(out, geminiContent{
				Role:  role,
				Parts: []geminiPart{{Text: m.Content}},
			})
		}
	}
	return out
}

func toGeminiTools(tools []ToolDefinition) []geminiToolDecl {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]geminiFunctionDecl, len(tools))
	for i, t := range tools {
		decls[i] = geminiFunctionDecl{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return []geminiToolDecl{{FunctionDeclarations: decls}}
}

func (g *GeminiProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := geminiRequest{
		Contents: toGeminiContents(req.Messages),
		GenerationConfig: &geminiGenConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: orDefault(req.MaxTokens, 4096),
		},
		Tools: toGeminiTools(req.Tools),
	}
	if req.SystemPrompt != "" {
		body.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: req.SystemPrompt}},
		}
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	url := geminiURL(req.Model, g.apiKey, "generateContent")

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode != http.StatusOK {
		var apiErr geminiErrorResp
		if err := json.Unmarshal(respBody, &apiErr); err == nil {
			switch {
			case resp.StatusCode == 400 && strings.Contains(apiErr.Error.Message, "API key"):
				return nil, ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return nil, rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return nil, fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return nil, fmt.Errorf("gemini API: %s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("gemini API error (status %d)", resp.StatusCode)
	}

	var parsed geminiResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse gemini response: %w", err)
	}
	if len(parsed.Candidates) == 0 {
		return nil, errors.New("gemini returned no content")
	}

	parts := parsed.Candidates[0].Content.Parts
	var text string
	var toolCalls []ToolCall
	for i, p := range parts {
		if p.Text != "" {
			text += p.Text
		}
		if p.FunctionCall != nil {
			// A nil Args map marshals to the literal `null`, which is not a
			// valid tool-arguments object; send `{}` like the OpenAI provider.
			args := []byte("{}")
			if p.FunctionCall.Args != nil {
				args, _ = json.Marshal(p.FunctionCall.Args)
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:    fmt.Sprintf("call_%d", i),
				Name:  p.FunctionCall.Name,
				Input: json.RawMessage(args),
			})
		}
	}

	if text == "" && len(toolCalls) == 0 {
		return nil, errors.New("gemini returned no content")
	}

	tokensIn, tokensOut := 0, 0
	if parsed.UsageMetadata != nil {
		tokensIn = parsed.UsageMetadata.PromptTokenCount
		tokensOut = parsed.UsageMetadata.CandidatesTokenCount
	}

	return &Response{
		Content:      text,
		TokensIn:     tokensIn,
		TokensOut:    tokensOut,
		FinishReason: parsed.Candidates[0].FinishReason,
		ToolCalls:    toolCalls,
	}, nil
}

func (g *GeminiProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := geminiRequest{
		Contents: toGeminiContents(req.Messages),
		GenerationConfig: &geminiGenConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: orDefault(req.MaxTokens, 4096),
		},
		Tools: toGeminiTools(req.Tools),
	}
	if req.SystemPrompt != "" {
		body.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: req.SystemPrompt}},
		}
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	url := geminiURL(req.Model, g.apiKey, "streamGenerateContent")

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var apiErr geminiErrorResp
		if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
			switch {
			case resp.StatusCode == 400 && strings.Contains(apiErr.Error.Message, "API key"):
				return ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return fmt.Errorf("gemini stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
		}
		return fmt.Errorf("gemini stream error (status %d)", resp.StatusCode)
	}

	return parseGeminiSSE(resp.Body, onChunk)
}
