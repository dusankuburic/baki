package ai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

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

// ErrThoughtSignatureMissing is Gemini refusing a replayed function call whose thoughtSignature is missing — the
// history lost the model's reasoning record (it came from another provider, or was cut). Deterministic: never
// retried, never trips the breaker; the copy is the chat's message as is.
var ErrThoughtSignatureMissing = errors.New("gemini refused the conversation: a tool call in its history is missing the model's thought signature (the history came from another model, or was shortened) — start a new conversation to keep using tools with this model")

// geminiMissingSignatureFinish is the finishReason Google documents for a request whose history lacks a thought
// signature ("Request has at least one thought signature missing"). Modelled on the documentation, not a capture.
const geminiMissingSignatureFinish = "MISSING_THOUGHT_SIGNATURE"

// geminiMissingSignature reports Gemini's 400 for a replayed function call without its thoughtSignature. The shape
// (INVALID_ARGUMENT, "Function call is missing a thought_signature in functionCall parts…") is modelled on Google's
// thought-signature documentation, not a live capture: matched on the message naming the signature.
func geminiMissingSignature(status int, apiErr geminiAPIError) bool {
	if status != http.StatusBadRequest && apiErr.Status != "INVALID_ARGUMENT" {
		return false
	}
	m := strings.ToLower(apiErr.Message)
	return strings.Contains(m, "thought_signature") || strings.Contains(m, "thought signature")
}

// geminiReplayRecord is a Gemini turn's replay record (Chunk/Response.ProviderItems, then
// Message.ProviderItems): the thought signatures the API attached to the turn's parts and the call ids it issued,
// keyed by ToolCall.ID. toGeminiContents puts them back on the matching parts when the record is Gemini's own.
type geminiReplayRecord struct {
	Calls []geminiReplayCall `json:"calls,omitempty"`
	// TextSignature is the signature on the turn's text (a text answer's last part, often an empty one).
	TextSignature string `json:"textSignature,omitempty"`
}

type geminiReplayCall struct {
	ID        string `json:"id"`
	Signature string `json:"signature,omitempty"`
	// ProviderID: ID is the functionCall.id the API issued (echoed back), not one baki minted.
	ProviderID bool `json:"providerId,omitempty"`
}

// geminiTurn accumulates a model turn's function calls and replay record from its parts, streamed or whole.
type geminiTurn struct {
	calls  []ToolCall
	record geminiReplayRecord
}

// part takes one response part and returns its answer text.
func (t *geminiTurn) part(p geminiPart) string {
	if p.FunctionCall == nil {
		if p.ThoughtSignature != "" {
			t.record.TextSignature = p.ThoughtSignature
		}
		return p.Text
	}
	// A nil Args map marshals to the literal `null`; send `{}` like the OpenAI provider.
	args := []byte("{}")
	if p.FunctionCall.Args != nil {
		args, _ = json.Marshal(p.FunctionCall.Args)
	}
	// The API's own id when it issued one (Gemini 3), else a process-unique one (see geminiCallSeq).
	id, fromProvider := p.FunctionCall.ID, true
	if id == "" {
		id, fromProvider = nextGeminiCallID(), false
	}
	t.calls = append(t.calls, ToolCall{ID: id, Name: p.FunctionCall.Name, Input: json.RawMessage(args)})
	if p.ThoughtSignature != "" || fromProvider {
		t.record.Calls = append(t.record.Calls, geminiReplayCall{ID: id, Signature: p.ThoughtSignature, ProviderID: fromProvider})
	}
	return p.Text
}

// items is the turn's replay record, nil when it holds nothing to replay.
func (t *geminiTurn) items() json.RawMessage {
	if len(t.record.Calls) == 0 && t.record.TextSignature == "" {
		return nil
	}
	raw, err := json.Marshal(t.record)
	if err != nil {
		return nil
	}
	return raw
}

// geminiReplayFor is m's Gemini replay record: only one Gemini wrote (ProviderItemsFrom) and that decodes as one;
// a record from another provider, or of another shape, puts no Gemini signature on the history.
func geminiReplayFor(m Message) (geminiReplayRecord, map[string]geminiReplayCall, bool) {
	var rec geminiReplayRecord
	if m.ProviderItemsFrom != geminiSpec.ID || len(m.ProviderItems) == 0 || json.Unmarshal(m.ProviderItems, &rec) != nil {
		return geminiReplayRecord{}, nil, false
	}
	byID := make(map[string]geminiReplayCall, len(rec.Calls))
	for _, c := range rec.Calls {
		byID[c.ID] = c
	}
	return rec, byID, true
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
		logger.FromContext(ctx).Warn("configured embedding width is above the gemini model's native width — embedding at the native width; the knowledge base will not index these vectors",
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

	resp, err := doProviderRequest(g.client, httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logUpstreamStatus(ctx, geminiSpec.ID, embedModel, resp, false)
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("gemini embeddings: %w", classifyGeminiError(resp, errBody))
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
	// ThoughtSignature is a thinking model's encrypted reasoning record, attached to the part it belongs to (the
	// first functionCall of a turn, or a text answer's last part). It goes back on the same part when the turn
	// is replayed (geminiReplayRecord): Gemini 3 refuses a replayed function call without it.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type geminiFunctionCall struct {
	// ID is the call's id when the API issued one (Gemini 3); echoed on the call and its functionResponse.
	ID   string                 `json:"id,omitempty"`
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type geminiFunctionResponse struct {
	// ID answers the functionCall.id the API issued; empty when the call had none.
	ID       string      `json:"id,omitempty"`
	Name     string      `json:"name"`
	Response interface{} `json:"response"`
}

type geminiGenConfig struct {
	// Temperature is a pointer so an explicit 0 (Request.TemperatureSet —
	// deterministic sampling) is sent; nil leaves the model's default.
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	// ThinkingConfig carries the user's reasoning depth (geminiThinking); nil keeps the model's default.
	ThinkingConfig *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// geminiThinkingConfig is Gemini's thinking lever: a token budget on the 2.5 family, a level on Gemini 3.
// Thoughts are never requested back (includeThoughts stays false): the answer is what the turn shows.
type geminiThinkingConfig struct {
	ThinkingBudget *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel  string `json:"thinkingLevel,omitempty"`
}

// The thinking budgets a reasoning effort maps to on the 2.5 family.
var geminiThinkingBudgets = map[string]int{"low": 1024, "medium": 8192, "high": 24576}

// geminiAnswerReserve is the part of a turn's output cap a thinking budget leaves for the answer: Gemini counts
// thoughts against maxOutputTokens, and a budget filling the cap ends MAX_TOKENS with no answer.
const geminiAnswerReserve = 4096

// geminiMinThinkingBudget is the smallest budget the 2.5 family accepts (2.5 Pro: 128).
const geminiMinThinkingBudget = 128

// geminiThinking maps a request's reasoning effort to the model's thinking lever under its output cap maxOut;
// "" (the provider default) and a model the lever does not suit send none:
//   - Gemini 3 takes a level ("low" or "high"; Pro has no other).
//   - The 2.5 family takes a budget, cut to leave geminiAnswerReserve of the cap for the answer. Flash-Lite does
//     not think by default, so "low" leaves it off.
//   - Image models and the 2.0 "-thinking" previews take neither.
func geminiThinking(model, effort string, maxOut int) *geminiThinkingConfig {
	if effort == "" {
		return nil
	}
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "image"):
		return nil
	case strings.Contains(m, "gemini-3"):
		level := "high"
		if effort == "low" {
			level = "low"
		}
		return &geminiThinkingConfig{ThinkingLevel: level}
	case strings.Contains(m, "gemini-2.5"):
		budget, ok := geminiThinkingBudgets[effort]
		if !ok || (effort == "low" && strings.Contains(m, "flash-lite")) {
			return nil
		}
		if maxOut > 0 {
			budget = min(budget, max(maxOut-geminiAnswerReserve, geminiMinThinkingBudget))
		}
		return &geminiThinkingConfig{ThinkingBudget: &budget}
	}
	return nil
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
		// CachedContentTokenCount is the cached slice OF promptTokenCount.
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

// geminiErrorResp is Google's error envelope; Details carries the RetryInfo of a RESOURCE_EXHAUSTED answer.
type geminiErrorResp struct {
	Error geminiAPIError `json:"error"`
}

type geminiAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
	Details []struct {
		Type       string `json:"@type"`
		RetryDelay string `json:"retryDelay"`
		// Violations is a QuotaFailure detail's list: which quota ran out.
		Violations []struct {
			QuotaID string `json:"quotaId"`
		} `json:"violations"`
	} `json:"details"`
}

// classifyGeminiError maps a non-200 answer of any Gemini call (Chat, Stream, embedBatch) to the shared
// sentinels, status first and body second: a gateway 429 or 502 with an empty or HTML body still counts for
// retry and the breaker, as in the OpenAI-compatible family (R-provider-10).
func classifyGeminiError(resp *http.Response, body []byte) error {
	var parsed geminiErrorResp
	_ = json.Unmarshal(body, &parsed)
	return classifyGeminiAPIError(resp.StatusCode, parsed.Error, parseRetryAfter(resp))
}

// classifyGeminiAPIError is classifyGeminiError for an error already decoded: status is the HTTP status, or the
// error's own code when it arrived as an event of a 200 stream. retryAfter is the header's hint (0: none).
func classifyGeminiAPIError(status int, apiErr geminiAPIError, retryAfter time.Duration) error {
	msg, code := apiErr.Message, apiErr.Status
	switch {
	case (status == http.StatusForbidden || code == "PERMISSION_DENIED") && !strings.Contains(msg, "API key"):
		// The key was read and the project refused (the API disabled, an unregistered caller): vendor-neutral
		// copy that keeps Google's reason, never the Azure role copy.
		return NewAccessDeniedError(cmp.Or(vendorMessage(msg), "gemini answered 403"))
	case status == http.StatusUnauthorized || code == "UNAUTHENTICATED" ||
		((status == http.StatusBadRequest || status == http.StatusForbidden || code == "PERMISSION_DENIED") && strings.Contains(msg, "API key")):
		// A 400 or 403 naming the key: invalid, leaked or restricted — another key is the way out.
		if msg != "" {
			return fmt.Errorf("%w: %s", ErrApiKeyInvalid, msg)
		}
		return ErrApiKeyInvalid
	case (status == http.StatusTooManyRequests || code == "RESOURCE_EXHAUSTED") && geminiDailyQuota(apiErr):
		// A daily quota (the free tier's, a project's) does not come back in a moment: the balance error, not a
		// rate limit to retry and wait out.
		return fmt.Errorf("%w: %s", ErrInsufficientBalance, cmp.Or(vendorMessage(msg), "gemini daily quota exhausted"))
	case status == http.StatusTooManyRequests || code == "RESOURCE_EXHAUSTED":
		if retryAfter == 0 {
			retryAfter = geminiRetryDelay(apiErr)
		}
		return &RateLimitError{RetryAfter: retryAfter}
	case status >= 500 || code == "UNAVAILABLE" || code == "INTERNAL" || code == "DEADLINE_EXCEEDED":
		if msg != "" {
			return fmt.Errorf("%w: %s", ErrProviderDown, msg)
		}
		return fmt.Errorf("%w (status %d)", ErrProviderDown, status)
	}
	if geminiMissingSignature(status, apiErr) {
		return fmt.Errorf("%w (gemini: %s)", ErrThoughtSignatureMissing, oneLine(vendorMessage(msg), 200))
	}
	if err := detectContextLimitError(status, msg); err != nil {
		return err
	}
	if err := detectToolsUnsupportedError(status, msg); err != nil {
		return err
	}
	if msg != "" {
		return fmt.Errorf("gemini API error (status %d): %s", status, msg)
	}
	return fmt.Errorf("gemini API error (status %d)", status)
}

// geminiDailyQuota reports a QuotaFailure naming a per-day quota (GenerateRequestsPerDayPerProjectPerModel-FreeTier).
func geminiDailyQuota(apiErr geminiAPIError) bool {
	for _, d := range apiErr.Details {
		for _, v := range d.Violations {
			if strings.Contains(v.QuotaID, "PerDay") {
				return true
			}
		}
	}
	return false
}

// geminiRetryDelay reads the RetryInfo detail's delay ("7s"), 0 without one.
func geminiRetryDelay(apiErr geminiAPIError) time.Duration {
	for _, d := range apiErr.Details {
		if strings.HasSuffix(d.Type, "google.rpc.RetryInfo") {
			if v, err := time.ParseDuration(d.RetryDelay); err == nil && v > 0 {
				return v
			}
		}
	}
	return 0
}

// toGeminiContents converts neutral messages to Gemini's wire format. The tool results of one assistant turn
// are coalesced into one user content (like toClaudeMessages): Gemini matches a turn's functionResponse parts
// against the preceding model content's functionCall parts and rejects a split with a 400.
func toGeminiContents(msgs []Message) []geminiContent {
	callNames := map[string]string{}
	// providerIDs are the call ids the API issued, read from Gemini's own replay records: echoed on the
	// functionCall and its functionResponse (a minted id never is).
	providerIDs := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			callNames[tc.ID] = tc.Name
		}
		if _, byID, ok := geminiReplayFor(m); ok {
			for id, c := range byID {
				if c.ProviderID {
					providerIDs[id] = true
				}
			}
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
				resp := &geminiFunctionResponse{Name: name, Response: map[string]string{"result": msgs[i].Content}}
				if providerIDs[msgs[i].ToolCallID] {
					resp.ID = msgs[i].ToolCallID
				}
				parts = append(parts, geminiPart{FunctionResponse: resp})
				i++
			}
			i-- // outer loop will increment
			out = append(out, geminiContent{Role: "user", Parts: parts})

		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			// Gemini's own record puts each signature back on its part (R-provider-32); none for another provider's.
			rec, byID, _ := geminiReplayFor(m)
			parts := make([]geminiPart, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content, ThoughtSignature: rec.TextSignature})
			}
			for _, tc := range m.ToolCalls {
				var args map[string]interface{}
				if len(tc.Input) > 0 {
					_ = json.Unmarshal(tc.Input, &args) // best-effort; args stays nil on bad input
				}
				call := &geminiFunctionCall{Name: tc.Name, Args: args}
				part := geminiPart{FunctionCall: call}
				if c, ok := byID[tc.ID]; ok {
					part.ThoughtSignature = c.Signature
					if c.ProviderID {
						call.ID = tc.ID
					}
				}
				parts = append(parts, part)
			}
			out = append(out, geminiContent{
				Role:  "model",
				Parts: parts,
			})

		default:
			role := "user"
			part := geminiPart{Text: m.Content}
			if m.Role == "assistant" {
				role = "model"
				if rec, _, ok := geminiReplayFor(m); ok && m.Content != "" {
					part.ThoughtSignature = rec.TextSignature
				}
			}
			out = append(out, geminiContent{
				Role:  role,
				Parts: []geminiPart{part},
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
			ThinkingConfig:  geminiThinking(req.Model, req.ReasoningEffort, orDefault(req.MaxTokens, 4096)),
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

	resp, err := doProviderRequest(g.client, httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode != http.StatusOK {
		logUpstreamStatus(ctx, geminiSpec.ID, req.Model, resp, false)
		return nil, classifyGeminiError(resp, respBody)
	}

	var parsed geminiResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse gemini response: %w", err)
	}
	if len(parsed.Candidates) == 0 {
		return nil, errors.New("gemini returned no content")
	}

	if parsed.Candidates[0].FinishReason == geminiMissingSignatureFinish {
		return nil, fmt.Errorf("%w (finishReason %s)", ErrThoughtSignatureMissing, geminiMissingSignatureFinish)
	}

	var turn geminiTurn
	var text string
	for _, p := range parsed.Candidates[0].Content.Parts {
		text += turn.part(p)
	}
	toolCalls := turn.calls

	if text == "" && len(toolCalls) == 0 {
		if parsed.Candidates[0].FinishReason == "MAX_TOKENS" {
			// A thinking model spent the whole budget thinking: the output limit, as on the stream path.
			return nil, fmt.Errorf("%w (finishReason MAX_TOKENS)", ErrOutputLimit)
		}
		return nil, errors.New("gemini returned no content")
	}

	tokensIn, tokensOut, cacheRead := 0, 0, 0
	if u := parsed.UsageMetadata; u != nil {
		tokensIn = u.PromptTokenCount
		tokensOut = u.CandidatesTokenCount + u.ThoughtsTokenCount
		cacheRead = min(u.CachedContentTokenCount, u.PromptTokenCount)
	}

	return &Response{
		Content:         text,
		TokensIn:        tokensIn,
		TokensOut:       tokensOut,
		CacheReadTokens: cacheRead,
		FinishReason:    parsed.Candidates[0].FinishReason,
		ToolCalls:       toolCalls,
		ProviderItems:   turn.items(),
	}, nil
}

func (g *GeminiProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := geminiRequest{
		Contents: toGeminiContents(req.Messages),
		GenerationConfig: &geminiGenConfig{
			Temperature:     geminiTemperature(req),
			MaxOutputTokens: orDefault(req.MaxTokens, 4096),
			ThinkingConfig:  geminiThinking(req.Model, req.ReasoningEffort, orDefault(req.MaxTokens, 4096)),
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

	resp, err := doProviderRequest(g.client, httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logUpstreamStatus(ctx, geminiSpec.ID, req.Model, resp, false)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return classifyGeminiError(resp, body)
	}

	return parseGeminiSSE(resp.Body, onChunk)
}
