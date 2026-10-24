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
	"sync/atomic"

	"pad-core/logger"
)

// geminiCallSeq mints synthesized tool-call IDs ("call_N"): Gemini's protocol
// returns none. It must be process-wide — a tool loop replays history through
// toGeminiContents, whose callNames map is keyed by ID, and a per-request
// counter makes iteration 2's call_0 overwrite iteration 1's entry.
var geminiCallSeq atomic.Uint64

func nextGeminiCallID() string {
	return fmt.Sprintf("call_%d", geminiCallSeq.Add(1))
}

type GeminiProvider struct {
	apiKey string
	client *http.Client
	// embeddingModel overrides the embed model; "" falls back to
	// gemini-embedding-001. Satisfies embedModelSetter for ForEmbedding.
	embeddingModel string
	// embedDimensions is the knowledge base's vector width (PAD_EMBEDDING_DIM), sent as outputDimensionality
	// when the model can shorten to it; 0 leaves the model's native width.
	embedDimensions int
}

func NewGeminiProvider(apiKey string) *GeminiProvider {
	return &GeminiProvider{
		apiKey: apiKey,
		client: fixedHostClient(),
	}
}

// setEmbeddingModel satisfies embedModelSetter (promoted by the factory).
func (g *GeminiProvider) setEmbeddingModel(model string) { g.embeddingModel = model }

// setEmbeddingDimensions satisfies embedDimensionSetter: without it the factory's width never reached Gemini
// and a corpus embedded at the native 3072 was never vector-indexed at any other PAD_EMBEDDING_DIM.
func (g *GeminiProvider) setEmbeddingDimensions(n int) {
	if n > 0 {
		g.embedDimensions = n
	}
}

// Native widths of Gemini's embedding models. outputDimensionality can only shorten a vector (Matryoshka
// truncation); a wider ask is a 400, so the native width is kept instead.
const (
	geminiEmbeddingNativeWidth = 3072 // gemini-embedding-001 and the gemini-embedding-* previews
	textEmbedding004Width      = 768  // text-embedding-004/005
)

// geminiEmbeddingDimensionsFor returns the outputDimensionality to send for model at width n, or 0 for none:
// never above the model's native width. A model whose width is unknown gets the configured width as is —
// every current Gemini embedding model accepts the parameter, and the operator chose both.
func geminiEmbeddingDimensionsFor(model string, n int) int {
	if n <= 0 {
		return 0
	}
	m := strings.ToLower(model)
	var native int
	switch {
	case strings.HasPrefix(m, "gemini-embedding"):
		native = geminiEmbeddingNativeWidth
	case strings.HasPrefix(m, "text-embedding-00"):
		native = textEmbedding004Width
	default:
		return n
	}
	if n > native {
		return 0
	}
	return n
}

// geminiWidthWarned makes the above-native warning one line per process rather than one per batch (the
// factory builds a fresh provider per request); a deployment has one width and one Gemini embedding model.
var geminiWidthWarned atomic.Bool

func resetGeminiWidthWarning() { geminiWidthWarned.Store(false) }

// EmbeddingModel satisfies embedModelNamer so the audited wrapper can price
// embedding usage against the model actually sent (empty ⇒ shipped default).
func (g *GeminiProvider) EmbeddingModel() string {
	if g.embeddingModel != "" {
		return g.embeddingModel
	}
	return geminiEmbedModelDefault
}

const geminiEmbedModelDefault = "gemini-embedding-001"

func (g *GeminiProvider) SupportsTools() bool { return true }

func (g *GeminiProvider) ID() string           { return geminiSpec.ID }
func (g *GeminiProvider) Name() string         { return geminiSpec.Name }
func (g *GeminiProvider) ContextLimit() int    { return 1048576 }
func (g *GeminiProvider) DefaultModel() string { return geminiSpec.DefaultModel }
func (g *GeminiProvider) FreeModel() string    { return "" }

// geminiEmbedBatchSize caps one batchEmbedContents request: Gemini rejects
// oversized batches (~100 documented) and callers embed up to
// maxKnowledgeChunks texts, so Embed splits and concatenates in order.
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

func (g *GeminiProvider) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	// Gemini's embeddings API differs from OpenAI's shape: one
	// batchEmbedContents POST carries an array of requests, each wrapping the
	// text in content.parts[].text; RETRIEVAL_DOCUMENT is the recommended
	// taskType for stored/searched documents, RETRIEVAL_QUERY for the query
	// searched with.
	embedModel := geminiEmbedModelDefault
	if g.embeddingModel != "" {
		embedModel = g.embeddingModel
	}
	fullModel := "models/" + embedModel
	taskType := "RETRIEVAL_DOCUMENT"
	if IsQueryEmbedding(ctx) {
		taskType = "RETRIEVAL_QUERY"
	}
	dims := geminiEmbeddingDimensionsFor(embedModel, g.embedDimensions)
	if g.embedDimensions > 0 && dims == 0 && geminiWidthWarned.CompareAndSwap(false, true) {
		logger.Warn("configured embedding width is above the gemini model's native width — embedding at the native width; the knowledge base will not index these vectors",
			"model", embedModel, "width", g.embedDimensions)
	}

	type embedRequest struct {
		Model    string        `json:"model"`
		Content  geminiContent `json:"content"`
		TaskType string        `json:"taskType"`
		// OutputDimensionality shortens the vector to the knowledge base's width; omitted at the native width.
		OutputDimensionality int `json:"outputDimensionality,omitempty"`
	}
	reqs := make([]embedRequest, len(texts))
	for i, t := range texts {
		reqs[i] = embedRequest{
			Model:                fullModel,
			Content:              geminiContent{Parts: []geminiPart{{Text: t}}},
			TaskType:             taskType,
			OutputDimensionality: dims,
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBodyBytes)).Decode(&parsed); err != nil {
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
	return catalogModels(geminiSpec.Catalog), nil
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
	// Temperature is a pointer so an explicit 0 (Request.TemperatureSet —
	// deterministic sampling) is sent; nil leaves the model's default.
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
}

// geminiTemperature is the generationConfig temperature for req: sent when the
// caller chose it (TemperatureSet, 0 included) or it is non-zero; an unchosen
// zero keeps the legacy provider default (omitted).
func geminiTemperature(req Request) *float64 {
	if !req.TemperatureSet && req.Temperature == 0 {
		return nil
	}
	t := req.Temperature
	return &t
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
		// ThoughtsTokenCount bills separately from candidatesTokenCount
		// (thinking tokens); include it in the output count.
		ThoughtsTokenCount int `json:"thoughtsTokenCount"`
		TotalTokenCount    int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type geminiErrorResp struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// toGeminiContents converts neutral messages to Gemini's wire format. The tool results of one assistant turn
// are coalesced into one user content (like toClaudeMessages): Gemini matches a turn's functionResponse parts
// against the preceding model content's functionCall parts and rejects a split with a 400.
func toGeminiContents(msgs []Message) []geminiContent {
	callNames := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			callNames[tc.ID] = tc.Name
		}
	}

	var out []geminiContent
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch {
		case m.Role == "tool":
			// Gather this and any immediately-following tool results.
			var parts []geminiPart
			for i < len(msgs) && msgs[i].Role == "tool" {
				// A result whose call fell out of the history still needs a name: the call id is stable and
				// never empty, unlike a missing map entry.
				name := callNames[msgs[i].ToolCallID]
				if name == "" {
					name = msgs[i].ToolCallID
				}
				parts = append(parts, geminiPart{
					FunctionResponse: &geminiFunctionResponse{
						Name:     name,
						Response: map[string]string{"result": msgs[i].Content},
					},
				})
				i++
			}
			i-- // outer loop will increment
			out = append(out, geminiContent{Role: "user", Parts: parts})

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
			Temperature:     geminiTemperature(req),
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
			// Mirror the OpenAI-compat/Claude paths: an oversized prompt must
			// surface as ErrContextLimit so the chat service maps it to the
			// friendly "conversation too long" UX instead of a raw 400.
			if err := detectContextLimitError(resp.StatusCode, apiErr.Error.Message); err != nil {
				return nil, err
			}
			if err := detectToolsUnsupportedError(resp.StatusCode, apiErr.Error.Message); err != nil {
				return nil, err
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
	for _, p := range parts {
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
				ID:    nextGeminiCallID(),
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
		tokensOut = parsed.UsageMetadata.CandidatesTokenCount + parsed.UsageMetadata.ThoughtsTokenCount
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
			Temperature:     geminiTemperature(req),
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
			if err := detectContextLimitError(resp.StatusCode, apiErr.Error.Message); err != nil {
				return err
			}
			if err := detectToolsUnsupportedError(resp.StatusCode, apiErr.Error.Message); err != nil {
				return err
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
