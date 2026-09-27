package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"pad-core/logger"
)

// Which request field carries the completion-length cap. max_tokens is the
// original OpenAI parameter and what most compatible servers understand;
// max_completion_tokens is its successor, mandatory for reasoning models
// (o-series, GPT-5) natively and on Azure AI Foundry.
const (
	maxTokensParamLegacy     = "max_tokens"
	maxTokensParamCompletion = "max_completion_tokens"
)

type openaiBase struct {
	// apiKey is the static bearer token, used when tokenFn is nil. Providers
	// with a fixed API key (OpenAI, GLM, GitHub Models, …) set this.
	apiKey string
	// tokenFn resolves the bearer token per-call, for providers whose token is
	// dynamic and can expire/require exchange (Copilot's session token, an
	// Entra ID access token). When set, it takes precedence over apiKey.
	// Errors propagate to the caller instead of reaching the network — a
	// stale/invalid token never becomes a wasted HTTP round trip.
	tokenFn       func(ctx context.Context) (string, error)
	client        *http.Client
	baseURL       *string
	providerLabel string
	// embeddingModel is the model name sent to the OpenAI-compatible /embeddings
	// endpoint. Empty means this provider does not expose embeddings and embed()
	// returns a "not supported" error (no /embeddings call is attempted).
	embeddingModel string
	// embeddingModelDefault is the provider's shipped default, preserved so an
	// empty override can restore it (and so setEmbeddingModel knows whether the
	// provider supports embeddings at all — an empty default means "no embeddings").
	embeddingModelDefault string
	// embedDimensions, when > 0, is sent as `dimensions` for embedding models
	// that support Matryoshka truncation (text-embedding-3-*), so the vectors
	// come back at the knowledge base's configured width (PAD_EMBEDDING_DIM)
	// and are always pgvector-indexable. Ignored for other models.
	embedDimensions int
	// maxTokensParam is maxTokensParamLegacy ("" behaves the same) or
	// maxTokensParamCompletion — see the constants above.
	maxTokensParam string
	extraHeaders   func(req *http.Request, model string)
	handle429      func(resp *http.Response, apiErr openAIErrorResp) error
}

// setEmbeddingModel overrides the model name sent to the /embeddings endpoint.
// An empty model resets to the provider's shipped default. The method is
// promoted to every provider that embeds openaiBase, satisfying
// embedModelSetter so ProviderFactory.ForEmbedding can apply a deployer-chosen
// model without changing each provider's constructor.
func (b *openaiBase) setEmbeddingModel(model string) {
	if model == "" {
		b.embeddingModel = b.embeddingModelDefault
		return
	}
	b.embeddingModel = model
}

// setEmbeddingDimensions satisfies embedDimensionSetter (see embedDimensions).
func (b *openaiBase) setEmbeddingDimensions(n int) {
	if n > 0 {
		b.embedDimensions = n
	}
}

// EmbeddingModel satisfies embedModelNamer so the audited wrapper can price
// embedding usage against the model actually sent. Empty means the provider
// does not expose embeddings (embed() refuses those anyway).
func (b *openaiBase) EmbeddingModel() string { return b.embeddingModel }

// resolveToken returns the bearer token for a request: tokenFn when set
// (dynamic token, e.g. Copilot's session exchange), else the static apiKey.
func (b *openaiBase) resolveToken(ctx context.Context) (string, error) {
	if b.tokenFn != nil {
		return b.tokenFn(ctx)
	}
	return b.apiKey, nil
}

// buildRequest maps the neutral Request onto the OpenAI chat-completions body.
// Exactly one of max_tokens / max_completion_tokens is set (maxTokensParam).
func (b *openaiBase) buildRequest(req Request, stream bool) openAIRequest {
	body := openAIRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
		Tools:       toOpenAITools(req.Tools),
	}
	limit := orDefault(req.MaxTokens, 4096)
	if b.maxTokensParam == maxTokensParamCompletion {
		body.MaxCompletionTokens = limit
	} else {
		body.MaxTokens = limit
	}
	if stream {
		body.Stream = true
		body.StreamOptions = usageStreamOptions
	}
	return body
}

// post performs one chat-completions POST with the provider's auth headers.
func (b *openaiBase) post(ctx context.Context, jsonBody []byte, model string, stream bool) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", *b.baseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	token, err := b.resolveToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get %s token: %w", b.providerLabel, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if b.extraHeaders != nil {
		b.extraHeaders(httpReq, model)
	}
	return b.client.Do(httpReq)
}

// send POSTs body to the chat endpoint. A 200 comes back with its body
// UNREAD (streams must not be buffered); any other status comes back with the
// body already read and closed so the caller can classify it.
//
// It makes at most one automatic repair round trip: when a 400 names a
// request parameter the model does not accept — OpenAI's and Azure's
// `unsupported_parameter` / `unsupported_value` errors carry it in `param`,
// older servers only say "Unsupported parameter: 'x'" — the parameter is
// swapped (max_tokens ↔ max_completion_tokens) or dropped (temperature on a
// reasoning model, …) and the request is sent again. A user-named Azure
// deployment does not reveal which model family sits behind the name, so the
// parameters cannot be known up front; learning from the rejection costs one
// extra round trip once and never loops (a second rejection is returned).
func (b *openaiBase) send(ctx context.Context, body openAIRequest, stream bool) (*http.Response, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	repaired := false
	for {
		jsonBody, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request: %w", err)
		}
		resp, err := b.post(ctx, jsonBody, body.Model, stream)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if !repaired {
			if param, ok := unsupportedParam(resp.StatusCode, errBody); ok && repairParam(payload, param) {
				repaired = true
				logger.Debug("AI request parameter rejected by the model — retrying without it",
					"provider", b.providerLabel, "model", body.Model, "param", param)
				continue
			}
		}
		return resp, errBody, nil
	}
}

// unsupportedParamPatterns recognise rejections that only name the parameter
// in prose (no structured `param`): OpenAI's "Unsupported parameter: 'x' is not
// supported with this model", "Unsupported value: 'x' does not support …" and
// the older "Unrecognized request argument supplied: x".
var unsupportedParamPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)unsupported parameter:? '([a-z_]+)'`),
	regexp.MustCompile(`(?i)unsupported value:? '([a-z_]+)'`),
	regexp.MustCompile(`(?i)unrecognized request argument supplied: ([a-z_]+)`),
	regexp.MustCompile(`(?i)'([a-z_]+)' is not supported with this model`),
}

// unsupportedParam reports the request parameter a 400 rejected, if any.
func unsupportedParam(status int, errBody []byte) (string, bool) {
	if status != http.StatusBadRequest {
		return "", false
	}
	var apiErr openAIErrorResp
	if err := json.Unmarshal(errBody, &apiErr); err != nil {
		return "", false
	}
	if (apiErr.Error.Code == "unsupported_parameter" || apiErr.Error.Code == "unsupported_value") && apiErr.Error.Param != "" {
		return strings.ToLower(apiErr.Error.Param), true
	}
	for _, re := range unsupportedParamPatterns {
		if m := re.FindStringSubmatch(apiErr.Error.Message); m != nil {
			return strings.ToLower(m[1]), true
		}
	}
	return "", false
}

// repairableParams lists the ONLY parameters send may drop or swap after a
// rejection. Anything else (model, messages, tools, …) is load-bearing: a
// rejection naming it is a real error and must surface as one.
var repairableParams = map[string]bool{
	maxTokensParamLegacy:     true,
	maxTokensParamCompletion: true,
	"max_output_tokens":      true,
	"temperature":            true,
	"top_p":                  true,
	"presence_penalty":       true,
	"frequency_penalty":      true,
	"stream_options":         true,
	"store":                  true,
	"parallel_tool_calls":    true,
	"reasoning_effort":       true,
	"seed":                   true,
	"stop":                   true,
	"n":                      true,
	"logprobs":               true,
}

// repairParam rewrites payload so the rejected parameter is gone: the two
// completion-length names swap (same value), everything else is deleted.
// Reports whether anything changed.
func repairParam(payload map[string]any, param string) bool {
	if !repairableParams[param] {
		return false
	}
	v, present := payload[param]
	if !present {
		return false
	}
	delete(payload, param)
	switch param {
	case maxTokensParamLegacy:
		payload[maxTokensParamCompletion] = v
	case maxTokensParamCompletion:
		payload[maxTokensParamLegacy] = v
	}
	return true
}

func (b *openaiBase) chat(ctx context.Context, req Request) (*Response, error) {
	resp, errBody, err := b.send(ctx, b.buildRequest(req, false), false)
	if err != nil {
		return nil, fmt.Errorf("%s API request: %w", b.providerLabel, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, b.handleChatError(resp, errBody)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse %s response: %w", b.providerLabel, err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New(b.providerLabel + " returned no choices")
	}
	choice := parsed.Choices[0]
	// A completion the content filter cut short is not an answer (Azure
	// OpenAI / Foundry); say so instead of returning the fragment.
	if choice.FinishReason == "content_filter" {
		return nil, fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered)
	}

	return &Response{
		Content:      choice.Message.Content,
		TokensIn:     parsed.Usage.PromptTokens,
		TokensOut:    parsed.Usage.CompletionTokens,
		FinishReason: choice.FinishReason,
		ToolCalls:    openAIToolCallsToNeutral(choice.Message.ToolCalls),
		ServedModel:  parsed.Model,
	}, nil
}

// isContentFilterError recognises a prompt the provider's content filter
// rejected outright: Azure OpenAI / Foundry answer 400 with error.code
// "content_filter" (inner code ResponsibleAIPolicyViolation) and a message
// about the "content management policy".
func isContentFilterError(status int, apiErr openAIErrorResp, respBody []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	if apiErr.Error.Code == "content_filter" {
		return true
	}
	lower := strings.ToLower(apiErr.Error.Message)
	return strings.Contains(lower, "content management policy") ||
		bytes.Contains(respBody, []byte("ResponsibleAIPolicyViolation"))
}

func (b *openaiBase) handleChatError(resp *http.Response, respBody []byte) error {
	var apiErr openAIErrorResp
	if err := json.Unmarshal(respBody, &apiErr); err == nil {
		if isContentFilterError(resp.StatusCode, apiErr, respBody) {
			return fmt.Errorf("%w: %s", ErrContentFiltered, apiErr.Error.Message)
		}
		switch {
		case resp.StatusCode == 401:
			return ErrApiKeyInvalid
		case resp.StatusCode == 429:
			if b.handle429 != nil {
				if handled := b.handle429(resp, apiErr); handled != nil {
					return handled
				}
			}
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
		return fmt.Errorf("%s API: %s", b.providerLabel, apiErr.Error.Message)
	}
	return fmt.Errorf("%s API error (status %d)", b.providerLabel, resp.StatusCode)
}

func (b *openaiBase) stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	resp, errBody, err := b.send(ctx, b.buildRequest(req, true), true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return b.handleStreamError(resp, errBody)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}

func (b *openaiBase) handleStreamError(resp *http.Response, respBody []byte) error {
	var apiErr openAIErrorResp
	if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Error.Message != "" {
		if isContentFilterError(resp.StatusCode, apiErr, respBody) {
			return fmt.Errorf("%w: %s", ErrContentFiltered, apiErr.Error.Message)
		}
		switch {
		case resp.StatusCode == 401:
			return ErrApiKeyInvalid
		case resp.StatusCode == 429:
			if b.handle429 != nil {
				if handled := b.handle429(resp, apiErr); handled != nil {
					return handled
				}
			}
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
		return fmt.Errorf("%s stream error (status %d): %s", b.providerLabel, resp.StatusCode, apiErr.Error.Message)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
	}
	return fmt.Errorf("%s stream error (status %d)", b.providerLabel, resp.StatusCode)
}

// supportsEmbeddingDimensions reports whether the embedding model accepts the
// `dimensions` parameter (OpenAI's text-embedding-3 family; ada-002 and
// third-party models reject or ignore it).
func supportsEmbeddingDimensions(model string) bool {
	return strings.Contains(strings.ToLower(model), "text-embedding-3")
}

// embed calls the OpenAI-compatible /embeddings endpoint. It is shared by every
// provider whose API mirrors OpenAI's shape (OpenAI, GLM, GitHub Models, Azure
// AI Foundry, …). The embeddings URL is derived from the provider's chat base
// URL by replacing the trailing "/chat/completions" with "/embeddings". Returns
// a "not supported" error when embeddingModel is empty (the provider has no
// embeddings model), so no network call is attempted for providers known to
// lack the endpoint.
func (b *openaiBase) embed(ctx context.Context, text []string) ([][]float32, error) {
	if b.embeddingModel == "" {
		return nil, fmt.Errorf("embeddings not supported by %s provider", b.providerLabel)
	}

	payload := map[string]any{
		"model": b.embeddingModel,
		"input": text,
	}
	if b.embedDimensions > 0 && supportsEmbeddingDimensions(b.embeddingModel) {
		payload["dimensions"] = b.embedDimensions
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	embedURL := strings.Replace(*b.baseURL, "/chat/completions", "/embeddings", 1)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", embedURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	token, err := b.resolveToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get %s token: %w", b.providerLabel, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	if b.extraHeaders != nil {
		b.extraHeaders(httpReq, b.embeddingModel)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Classified like a chat failure so a 401 reads as an invalid key and
		// a 429 is retried by the wrapper chain instead of failing the index.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("%s embeddings: %w", b.providerLabel, b.handleChatError(resp, errBody))
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
