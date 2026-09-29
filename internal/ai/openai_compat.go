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
	"sort"
	"strings"
	"sync"
	"time"

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
	// and are always pgvector-indexable. Ignored for other models and never
	// above a model's native width (see embeddingDimensionsFor).
	embedDimensions int
	// embedDimensionsFor, when set, replaces embeddingDimensionsFor for
	// providers whose embedding "model" is a user-chosen alias that reveals
	// nothing about the model behind it (Azure AI Foundry deployment names).
	embedDimensionsFor func(model string, n int) int
	// maxTokensParam is maxTokensParamLegacy ("" behaves the same) or
	// maxTokensParamCompletion — see the constants above.
	maxTokensParam string
	extraHeaders   func(req *http.Request, model string)
	handle429      func(resp *http.Response, apiErr openAIErrorResp) error
	// handle401, when set, classifies a 401/403 ahead of unauthorizedError (a
	// nil result falls through to it): Azure AI Foundry in Entra ID mode
	// names the rejected access token instead of an API key the principal
	// does not have (see entraUnauthorized).
	handle401 func(apiErr openAIErrorResp, respBody []byte) error
	// handle403, when set, classifies a 403 ahead of handle401: Azure AI
	// Foundry answers 403 for a request its network rules (firewall,
	// private endpoint) or the principal's role refuse — the credential was
	// accepted, so it is not an invalid key and a fresh token cannot help
	// (see azureForbidden).
	handle403 func(apiErr openAIErrorResp, respBody []byte) error
	// apiKeyHeader, when set, is the header the static apiKey travels in
	// INSTEAD of Authorization: Bearer (Azure AI Foundry's `api-key`); a
	// tokenFn provider always uses the bearer header.
	apiKeyHeader string
}

// setAuth sets the credential header on a request: the bearer token, or the
// static key in apiKeyHeader when the provider names one.
func (b *openaiBase) setAuth(ctx context.Context, req *http.Request) error {
	token, err := b.resolveToken(ctx)
	if err != nil {
		return fmt.Errorf("get %s token: %w", b.providerLabel, err)
	}
	if b.apiKeyHeader != "" && b.tokenFn == nil {
		req.Header.Set(b.apiKeyHeader, token)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// unauthorized classifies a 401/403: a 403 goes to the provider's handle403
// hook first, then the handle401 hook, then the shared unauthorizedError.
// Used by the chat and stream handlers alike (and, through handleChatError,
// by embeddings).
func (b *openaiBase) unauthorized(status int, apiErr openAIErrorResp, respBody []byte) error {
	if status == http.StatusForbidden && b.handle403 != nil {
		if err := b.handle403(apiErr, respBody); err != nil {
			return err
		}
	}
	if b.handle401 != nil {
		if err := b.handle401(apiErr, respBody); err != nil {
			return err
		}
	}
	return unauthorizedError(apiErr)
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
		Model:    req.Model,
		Messages: toOpenAIMessages(req.SystemPrompt, req.Messages),
		Tools:    toOpenAITools(req.Tools),
	}
	// An explicitly chosen temperature goes out even when it is 0; an unset
	// zero keeps meaning "the provider's default" and is omitted.
	if req.TemperatureSet || req.Temperature != 0 {
		t := req.Temperature
		body.Temperature = &t
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
	if err := b.setAuth(ctx, httpReq); err != nil {
		return nil, err
	}
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
// It makes at most one automatic repair round trip PER PARAMETER: when a 400
// names a request parameter the model does not accept — OpenAI's and Azure's
// `unsupported_parameter` / `unsupported_value` errors carry it in `param`,
// older servers only say "Unsupported parameter: 'x'" — the parameter is
// swapped (max_tokens ↔ max_completion_tokens) or dropped (temperature on a
// reasoning model, …) and the request is sent again. A user-named Azure
// deployment does not reveal which model family sits behind the name, so the
// parameters cannot be known up front; learning from the rejection costs one
// extra round trip per parameter. One model can reject several (gpt-5.6:
// temperature, then function tools under its default reasoning — see
// reasoningToolsRejected), so each parameter is repaired at most once and the
// total is capped at maxSendRepairs: it never loops (a rejection of a
// parameter already repaired is returned).
//
// The repairs a 200 confirmed are remembered per endpoint and model
// (learnedRepairs) and pre-applied to the next request, so the rejection
// round trip — a full re-upload of the prompt — is paid once per deployment
// rather than on every turn and every tool-loop iteration. A remembered
// repair the model has stopped agreeing with (a re-pointed deployment) is
// forgotten and the parameter repaired afresh.
//
// The request is encoded once; the generic map a repair rewrites is decoded
// only when a repair is needed (encoding, decoding and re-encoding every
// request cost ~3 ms and 1.5 MB of garbage per turn for nothing).
func (b *openaiBase) send(ctx context.Context, body openAIRequest, stream bool) (*http.Response, []byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	var payload map[string]any // built only when a repair needs it
	ensurePayload := func() bool {
		return payload != nil || json.Unmarshal(jsonBody, &payload) == nil
	}
	remarshal := func() error {
		out, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		jsonBody = out
		return nil
	}
	hasTools := len(body.Tools) > 0
	// repaired guards the loop (each parameter at most once) and is what a
	// 200 memoises; learned marks the entries pre-applied from the memo.
	repaired := map[string]bool{}
	learned := map[string]bool{}
	memoKey := b.repairsKey(body.Model)
	if params := repairsFor(memoKey); len(params) > 0 && ensurePayload() {
		for _, param := range params {
			if param == reasoningEffortParam {
				// A model router picks the model per request: the model that
				// needed reasoning_effort "none" last time says nothing about
				// the next one, which may reject the parameter outright. The
				// router relearns it from the rejection when it has to.
				if hasTools && !isModelRouterAlias(body.Model) {
					payload[reasoningEffortParam] = "none"
					repaired[param], learned[param] = true, true
				}
				continue
			}
			if repairParam(payload, param) {
				repaired[param], learned[param] = true, true
			}
		}
		if len(learned) > 0 {
			if err := remarshal(); err != nil {
				return nil, nil, err
			}
		}
	}
	// memoStale: a pre-applied repair was rejected this round, so the
	// request no longer starts from the raw payload and must not be
	// memoised as a whole; the next request relearns from scratch.
	memoStale := false
	roundTrips := 0
	// toolsResp/toolsBody keep the tools-with-reasoning rejection that
	// triggered the reasoning_effort repair (see below).
	var (
		toolsResp *http.Response
		toolsBody []byte
	)
	for {
		resp, err := b.post(ctx, jsonBody, body.Model, stream)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == http.StatusOK {
			if len(repaired) > 0 && !memoStale {
				rememberRepairs(memoKey, repaired)
			}
			return resp, nil, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		b.logUpstreamRejection(resp, body.Model)
		if roundTrips >= maxSendRepairs {
			return resp, errBody, nil
		}
		// Before the generic repair (and before the caller's
		// detectToolsUnsupportedError, which would drop the tools): the
		// model accepts function tools once reasoning is off.
		if hasTools && !repaired[reasoningEffortParam] &&
			reasoningToolsRejected(resp.StatusCode, errBody) && ensurePayload() {
			payload[reasoningEffortParam] = "none"
			repaired[reasoningEffortParam] = true
			toolsResp, toolsBody = resp, errBody
			logger.Debug("model rejects function tools with reasoning on chat completions — retrying with reasoning_effort=none",
				"provider", b.providerLabel, "model", body.Model)
			if err := remarshal(); err != nil {
				return nil, nil, err
			}
			roundTrips++
			continue
		}
		param, ok := unsupportedParam(resp.StatusCode, errBody)
		if !ok {
			return resp, errBody, nil
		}
		if param == reasoningEffortParam && toolsBody != nil {
			// The model refuses reasoning_effort "none" as well: report the
			// original tools rejection, which the caller classifies as
			// ErrToolsUnsupported so the tool loop degrades to an answer
			// without tools instead of failing the turn.
			return toolsResp, toolsBody, nil
		}
		if learned[param] {
			// The memo's rewrite of this parameter is what the model rejects
			// now: forget the memo and repair the parameter once more, as if
			// freshly rejected, instead of failing the turn for the rest of
			// the memo's lifetime.
			forgetRepairs(memoKey)
			memoStale = true
			delete(learned, param)
			delete(repaired, param)
		}
		if !repaired[param] && ensurePayload() && repairParam(payload, param) {
			repaired[param] = true
			logger.Debug("AI request parameter rejected by the model — retrying without it",
				"provider", b.providerLabel, "model", body.Model, "param", param)
			if err := remarshal(); err != nil {
				return nil, nil, err
			}
			roundTrips++
			continue
		}
		return resp, errBody, nil
	}
}

// maxSendRepairs caps send's repair round trips per request. Each parameter
// is repaired at most once anyway; the cap bounds a server that keeps naming
// new ones.
const maxSendRepairs = 4

// learnedRepairs remembers, per endpoint and model, the parameters send had
// to repair, so later requests go out repaired instead of paying the
// rejection round trip on every turn and tool-loop iteration: the default
// temperature (0.3) is rejected by every reasoning deployment, and gpt-5.6
// with tools additionally needs reasoning_effort "none". Keyed by resource
// URL + deployment (no secret): the behaviour is a property of the model
// behind the deployment, the same for every user of it. Bounded and
// short-lived — a deployment can be re-pointed at another model. Shared with
// the Responses path (postResponses) through repairsFor / rememberRepairs.
var learnedRepairs = struct {
	sync.Mutex
	m map[string]learnedRepair
}{m: map[string]learnedRepair{}}

type learnedRepair struct {
	params []string
	seen   time.Time
}

const (
	learnedRepairsMax = 1024
	learnedRepairsTTL = time.Hour
)

// repairsKey is the learnedRepairs key for one deployment of this endpoint.
func (b *openaiBase) repairsKey(model string) string {
	url := ""
	if b.baseURL != nil {
		url = *b.baseURL
	}
	return url + "\x00" + model
}

// repairsFor returns the remembered repairs for key, sorted; nil when none
// are known or the entry expired.
func repairsFor(key string) []string {
	learnedRepairs.Lock()
	defer learnedRepairs.Unlock()
	e, ok := learnedRepairs.m[key]
	if !ok {
		return nil
	}
	if time.Since(e.seen) > learnedRepairsTTL {
		delete(learnedRepairs.m, key)
		return nil
	}
	return e.params
}

// rememberRepairs stores the repairs a 200 confirmed for key.
func rememberRepairs(key string, repaired map[string]bool) {
	params := make([]string, 0, len(repaired))
	for p := range repaired {
		params = append(params, p)
	}
	sort.Strings(params)
	learnedRepairs.Lock()
	defer learnedRepairs.Unlock()
	if len(learnedRepairs.m) >= learnedRepairsMax {
		// Cheap to repopulate (one rejection per deployment): start over.
		learnedRepairs.m = map[string]learnedRepair{}
	}
	learnedRepairs.m[key] = learnedRepair{params: params, seen: time.Now()}
}

// forgetRepairs drops key's memo (a remembered repair the model now refuses).
func forgetRepairs(key string) {
	learnedRepairs.Lock()
	delete(learnedRepairs.m, key)
	learnedRepairs.Unlock()
}

// resetLearnedRepairs clears the memo and the streaming refusals (tests,
// ResetAzureFoundryCaches).
func resetLearnedRepairs() {
	learnedRepairs.Lock()
	learnedRepairs.m = map[string]learnedRepair{}
	learnedRepairs.Unlock()
	streamRefusals.Lock()
	streamRefusals.m = map[string]time.Time{}
	streamRefusals.Unlock()
}

// reasoningEffortParam is the chat-completions reasoning control.
const reasoningEffortParam = "reasoning_effort"

// reasoningToolsRejection matches gpt-5.6's (and later models') chat
// completions rejection of function tools while reasoning is on: "Function
// tools with reasoning_effort are not supported for gpt-5.6-sol in
// /v1/chat/completions. To use function tools, use /v1/responses or set
// reasoning_effort to 'none'." Its reasoning_effort defaults to medium, so
// every tools request is refused until reasoning_effort is "none".
var reasoningToolsRejection = regexp.MustCompile(`(?i)function tools with reasoning_effort are not supported|set reasoning_effort to '?none'?`)

// parameterRejectionStatus reports a status a parameter rejection arrives
// with: 400 (OpenAI, Azure OpenAI) or 422 (the Azure AI Model Inference
// layer behind Foundry Models sold directly by Azure: "Extra parameters
// ['max_completion_tokens'] are not allowed when extra-parameters is not set
// or set to be 'error'"). Repairs stay limited to repairableParams, so a 422
// from any other provider can at most trigger one extra round trip that
// removes a parameter the model refused.
func parameterRejectionStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
}

// reasoningToolsRejected reports whether a parameter rejection is that one.
func reasoningToolsRejected(status int, errBody []byte) bool {
	if !parameterRejectionStatus(status) {
		return false
	}
	apiErr, ok := decodeOpenAIError(errBody)
	if !ok {
		return false
	}
	return reasoningToolsRejection.MatchString(apiErr.Error.Message)
}

// decodeOpenAIError decodes an error body into the OpenAI envelope. Besides
// {"error":{"code","message","param","type"}} it reads the Azure AI Model
// Inference layer's TOP-LEVEL shape (Foundry Models sold directly by Azure):
//
//	{"status":422,"code":"parameter_not_supported","message":"One of the parameters contain invalid values.","detail":{"loc":["body","response_format"],"input":"json_object"}}
//	{"status":400,"code":"content_filter","message":"The response was filtered","param":"messages","type":null}
//
// whose `error`, when present, may be a plain string (which failed the whole
// strict decode). The top-level fields fill Error.Code / Message / Param /
// Type; the offending parameter comes from `param`, else detail.loc[1] (loc
// is ["body", "<param>", …]); a string `detail` extends the message. The
// top-level shape is only adopted when it carries a `code` or a `detail`:
// API Management's {"statusCode":401,"message":…} envelope keeps an empty
// Error (the Azure auth hooks read its message from the raw body). ok is
// false when the body is not JSON at all.
func decodeOpenAIError(body []byte) (openAIErrorResp, bool) {
	var apiErr openAIErrorResp
	strictOK := json.Unmarshal(body, &apiErr) == nil
	if strictOK && (apiErr.Error.Message != "" || apiErr.Error.Code != "") {
		return apiErr, true
	}
	var top struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Param   string          `json:"param"`
		Type    json.RawMessage `json:"type"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &top) != nil {
		return apiErr, strictOK
	}
	code := rawText(top.Code)
	detail := bytes.TrimSpace(top.Detail)
	if code == "" && (len(detail) == 0 || bytes.Equal(detail, []byte("null"))) {
		return apiErr, strictOK
	}
	var out openAIErrorResp
	out.Error.Code = code
	out.Error.Message = top.Message
	out.Error.Param = top.Param
	out.Error.Type = rawText(top.Type)
	if len(detail) > 0 && detail[0] == '"' {
		var s string
		if json.Unmarshal(detail, &s) == nil && strings.TrimSpace(s) != "" {
			if out.Error.Message == "" {
				out.Error.Message = s
			} else {
				out.Error.Message += ": " + s
			}
		}
	} else if len(detail) > 0 && detail[0] == '{' {
		var d struct {
			Loc []any `json:"loc"`
		}
		if json.Unmarshal(detail, &d) == nil && out.Error.Param == "" && len(d.Loc) > 1 {
			if p, ok := d.Loc[1].(string); ok {
				out.Error.Param = p
			}
		}
	}
	return out, true
}

// unsupportedParamPatterns recognise rejections that only name the parameter
// in prose (no structured `param`): OpenAI's "Unsupported parameter: 'x' is not
// supported with this model", "Unsupported value: 'x' does not support …",
// the older "Unrecognized request argument supplied: x" and the Model
// Inference layer's "Extra parameters ['x'] are not allowed …" (one
// parameter per round trip when it lists several).
var unsupportedParamPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)unsupported parameter:? '([a-z_]+)'`),
	regexp.MustCompile(`(?i)unsupported value:? '([a-z_]+)'`),
	regexp.MustCompile(`(?i)unrecognized request argument supplied: ([a-z_]+)`),
	regexp.MustCompile(`(?i)'([a-z_]+)' is not supported with this model`),
	regexp.MustCompile(`(?i)extra parameters? \[?'([a-z_]+)'`),
}

// unsupportedParam reports the request parameter a rejection named, if any.
func unsupportedParam(status int, errBody []byte) (string, bool) {
	if !parameterRejectionStatus(status) {
		return "", false
	}
	apiErr, ok := decodeOpenAIError(errBody)
	if !ok {
		return "", false
	}
	switch apiErr.Error.Code {
	case "unsupported_parameter", "unsupported_value",
		// The Model Inference layer's code, the parameter in detail.loc.
		"parameter_not_supported":
		if apiErr.Error.Param != "" {
			return strings.ToLower(apiErr.Error.Param), true
		}
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
	// The Responses path's include:["reasoning.encrypted_content"]: an
	// endpoint that refuses it answers without replayable reasoning.
	"include": true,
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
	// OpenAI / Foundry); say so instead of returning the fragment. The usage
	// rides on the error (as the stream path's error chunk carries it), so
	// the tokens the blocked completion spent can still be billed.
	if choice.FinishReason == "content_filter" {
		return nil, &heldUsageError{
			err:         fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered),
			tokensIn:    parsed.Usage.PromptTokens,
			tokensOut:   parsed.Usage.CompletionTokens,
			servedModel: parsed.Model,
		}
	}
	// A leading <think>…</think> block (DeepSeek-R1, …) is reasoning, not
	// the answer — split exactly as the streaming path does. A separate
	// message.reasoning_content is reasoning too, never Content.
	content, reasoning := splitLeadingThink(choice.Message.Content)
	if rc := choice.Message.ReasoningContent; rc != "" {
		if reasoning == "" {
			reasoning = rc
		} else {
			reasoning = rc + "\n" + reasoning
		}
	}

	return &Response{
		Content:      content,
		Reasoning:    reasoning,
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

// unauthorizedError classifies a 401. Azure AI Foundry's "PermissionDenied"
// means the credential WAS accepted — a valid Entra ID token whose principal
// lacks the data-plane role — so it keeps the service's explanation (the
// principal and the missing data action) instead of reading as an invalid key;
// every other 401 is an invalid credential.
func unauthorizedError(apiErr openAIErrorResp) error {
	if strings.EqualFold(apiErr.Error.Code, "PermissionDenied") {
		if msg := strings.Join(strings.Fields(apiErr.Error.Message), " "); msg != "" {
			if len(msg) > 500 {
				msg = strings.ToValidUTF8(msg[:500], "") + "…"
			}
			return &permissionDeniedError{msg: msg}
		}
	}
	return ErrApiKeyInvalid
}

// handleChatError classifies a non-200 chat response. Transient and auth
// conditions are keyed by HTTP STATUS first, with the JSON envelope only
// enriching the message: a gateway/APIM 429 or 502 with an empty or HTML body
// must still count as rate-limited / provider-down for the retry wrapper and
// the circuit breaker — the pre-status "%s API error" fallback was retried by
// neither. A 401/403 without a parsable body keeps the invalid-key mapping
// (Azure Foundry's PermissionDenied still needs the body; see
// unauthorizedError).
func (b *openaiBase) handleChatError(resp *http.Response, respBody []byte) error {
	apiErr, parsed := decodeOpenAIError(respBody)
	if parsed && isContentFilterError(resp.StatusCode, apiErr, respBody) {
		return fmt.Errorf("%w: %s", ErrContentFiltered, apiErr.Error.Message)
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return b.unauthorized(resp.StatusCode, apiErr, respBody)
	case resp.StatusCode == 429:
		if b.handle429 != nil {
			if handled := b.handle429(resp, apiErr); handled != nil {
				return handled
			}
		}
		return rateLimitErr(resp)
	case resp.StatusCode >= 500:
		if parsed && apiErr.Error.Message != "" {
			return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
		}
		return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
	}
	if parsed {
		if err := detectContextLimitError(resp.StatusCode, apiErr.Error.Message); err != nil {
			return err
		}
		if err := detectToolsUnsupportedError(resp.StatusCode, apiErr.Error.Message); err != nil {
			return err
		}
		return fmt.Errorf("%s API: %s", b.providerLabel, apiErr.Error.Message)
	}
	return fmt.Errorf("%s API error (status %d%s)", b.providerLabel, resp.StatusCode, requestIDSuffix(resp))
}

// upstreamRequestID is the gateway's correlation id for a response — Azure
// OpenAI / Foundry's apim-request-id (also x-ms-request-id), OpenAI's
// x-request-id — the one handle support can search for. Empty when absent
// (httptest servers send none). It carries no secret.
func upstreamRequestID(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	for _, h := range []string{"apim-request-id", "x-ms-request-id", "x-request-id"} {
		if v := resp.Header.Get(h); v != "" {
			return v
		}
	}
	return ""
}

// requestIDSuffix formats the request id for the residual (unclassified)
// error texts, so a user-visible failure can be correlated with Azure's side.
func requestIDSuffix(resp *http.Response) string {
	if rid := upstreamRequestID(resp); rid != "" {
		return ", request id " + rid
	}
	return ""
}

// logUpstreamRejection records a non-200's status and the gateway's request
// id, when it sent one: operators investigating a tenant's 429/5xx/
// DeploymentNotFound behaviour had the provider label and status but no id to
// hand to Azure support. A parameter rejection may still be repaired by
// send, so it logs at debug.
func (b *openaiBase) logUpstreamRejection(resp *http.Response, model string) {
	rid := upstreamRequestID(resp)
	if rid == "" {
		return
	}
	if parameterRejectionStatus(resp.StatusCode) {
		logger.Debug("AI upstream rejection", "provider", b.providerLabel, "model", model, "status", resp.StatusCode, "requestId", rid)
		return
	}
	logger.Warn("AI upstream error", "provider", b.providerLabel, "model", model, "status", resp.StatusCode, "requestId", rid)
}

func (b *openaiBase) stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	key := b.repairsKey(req.Model)
	if streamRefused(key) {
		return b.streamViaChat(ctx, req, onChunk)
	}
	resp, errBody, err := b.send(ctx, b.buildRequest(req, true), true)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close() // send already read and closed it; harmless
		if param, ok := unsupportedParam(resp.StatusCode, errBody); ok && param == "stream" {
			// The model does not stream at all (o1 on Azure: "Unsupported
			// value: 'stream' does not support true with this model"): answer
			// the turn non-streaming and remember the deployment, so later
			// turns skip the rejection round trip.
			rememberStreamRefusal(key)
			logger.Debug("model rejects streaming — answering non-streaming",
				"provider", b.providerLabel, "model", req.Model)
			return b.streamViaChat(ctx, req, onChunk)
		}
		return b.handleStreamError(resp, errBody)
	}
	defer drainAndClose(resp.Body)
	return parseOpenAISSE(resp.Body, onChunk)
}

// streamViaChat serves a Stream call with one non-streaming request and
// replays the reply as chunks: the answer (and split-off reasoning) as one
// text chunk, then the Done chunk with tool calls, usage, finish reason and
// the served model. A content-filter stop becomes an error chunk carrying
// its usage, as the streaming parser emits it; every other failure is
// returned before any chunk, like a stream's HTTP error.
func (b *openaiBase) streamViaChat(ctx context.Context, req Request, onChunk func(Chunk)) error {
	resp, err := b.chat(ctx, req)
	if err != nil {
		var held *heldUsageError
		if errors.As(err, &held) {
			onChunk(Chunk{Error: held.err, TokensIn: held.tokensIn, TokensOut: held.tokensOut, ServedModel: held.servedModel})
			return nil
		}
		return err
	}
	if resp.Content != "" || resp.Reasoning != "" {
		onChunk(Chunk{Text: resp.Content, Reasoning: resp.Reasoning, ServedModel: resp.ServedModel})
	}
	onChunk(Chunk{
		Done:         true,
		TokensIn:     resp.TokensIn,
		TokensOut:    resp.TokensOut,
		ToolCalls:    resp.ToolCalls,
		FinishReason: resp.FinishReason,
		ServedModel:  resp.ServedModel,
	})
	return nil
}

// streamRefusals remembers, per endpoint and deployment (repairsKey), the
// models that refused `stream: true`, for learnedRepairsTTL: a separate memo
// because send's learnedRepairs entry is overwritten by every repaired 200.
var streamRefusals = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

func streamRefused(key string) bool {
	streamRefusals.Lock()
	defer streamRefusals.Unlock()
	seen, ok := streamRefusals.m[key]
	if !ok {
		return false
	}
	if time.Since(seen) > learnedRepairsTTL {
		delete(streamRefusals.m, key)
		return false
	}
	return true
}

func rememberStreamRefusal(key string) {
	streamRefusals.Lock()
	defer streamRefusals.Unlock()
	if len(streamRefusals.m) >= learnedRepairsMax {
		streamRefusals.m = map[string]time.Time{}
	}
	streamRefusals.m[key] = time.Now()
}

// Bounds for drainAndClose: the chunked terminator after the terminal event
// is a handful of bytes, and a server that keeps the stream open must not
// hold the caller.
const (
	drainMaxBytes = 64 << 10
	drainMaxWait  = 200 * time.Millisecond
)

// drainAndClose reads what is left of a finished stream — the chunked
// terminator after [DONE] / response.completed — before closing it, so
// Go's HTTP/1.1 transport returns the connection to the pool instead of
// discarding a body closed early (the next request then paid a new TCP + TLS
// handshake). Bounded in bytes and time; Close unblocks the drain goroutine
// when the server stalls, and a body that already failed drains at once.
func drainAndClose(body io.ReadCloser) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(body, drainMaxBytes))
		close(done)
	}()
	t := time.NewTimer(drainMaxWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
	_ = body.Close()
}

// handleStreamError classifies a non-200 stream response, status first for
// exactly the same reason as handleChatError: an empty- or HTML-body 429/5xx
// from a gateway must still be retried and counted by the breaker. Unlike the
// chat handler, the message-dependent branches (content filter, context/tools
// detection, the prose fallback) also require a non-empty error.message, as
// they always have.
func (b *openaiBase) handleStreamError(resp *http.Response, respBody []byte) error {
	apiErr, parsed := decodeOpenAIError(respBody)
	msg := apiErr.Error.Message
	if parsed && msg != "" && isContentFilterError(resp.StatusCode, apiErr, respBody) {
		return fmt.Errorf("%w: %s", ErrContentFiltered, msg)
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return b.unauthorized(resp.StatusCode, apiErr, respBody)
	case resp.StatusCode == 429:
		if b.handle429 != nil {
			if handled := b.handle429(resp, apiErr); handled != nil {
				return handled
			}
		}
		return rateLimitErr(resp)
	case resp.StatusCode >= 500:
		if parsed && msg != "" {
			return fmt.Errorf("%w: %s", ErrProviderDown, msg)
		}
		return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
	}
	if parsed && msg != "" {
		if err := detectContextLimitError(resp.StatusCode, msg); err != nil {
			return err
		}
		if err := detectToolsUnsupportedError(resp.StatusCode, msg); err != nil {
			return err
		}
		return fmt.Errorf("%s stream error (status %d): %s", b.providerLabel, resp.StatusCode, msg)
	}
	return fmt.Errorf("%s stream error (status %d%s)", b.providerLabel, resp.StatusCode, requestIDSuffix(resp))
}

// Native (maximum) vector widths of OpenAI's text-embedding-3 models.
// Matryoshka truncation can only SHORTEN a vector: asking for more than the
// native width is a 400.
const (
	textEmbedding3SmallWidth = 1536
	textEmbedding3LargeWidth = 3072
)

// embeddingDimensionsFor returns the `dimensions` value to send for model at
// the configured width n, or 0 to send none. Only OpenAI's text-embedding-3
// family accepts the parameter (ada-002 and third-party models reject or
// ignore it), and never above the model's native width: a width the model
// cannot produce keeps its native vectors instead of failing the request
// (such chunks are then ranked Go-side, outside the pgvector index). A name in
// the family that does not reveal the size ("text-embedding-3-prod") keeps n.
func embeddingDimensionsFor(model string, n int) int {
	m := strings.ToLower(model)
	if n <= 0 || !strings.Contains(m, "text-embedding-3") {
		return 0
	}
	switch {
	case strings.Contains(m, "text-embedding-3-small") && n > textEmbedding3SmallWidth,
		strings.Contains(m, "text-embedding-3-large") && n > textEmbedding3LargeWidth:
		return 0
	}
	return n
}

// embeddingDimensions is the `dimensions` value embed sends (0 = none).
func (b *openaiBase) embeddingDimensions() int {
	if b.embedDimensionsFor != nil {
		return b.embedDimensionsFor(b.embeddingModel, b.embedDimensions)
	}
	return embeddingDimensionsFor(b.embeddingModel, b.embedDimensions)
}

// embedDimsRefusals remembers embedding endpoints (URL + model) that refused
// `dimensions`, so the repair round trip is paid once per process instead of
// on every Embed (the factory builds a fresh provider per request — a 400
// on every RAG query inside the 800 ms guidelines budget) and once per split
// sub-batch. Keyed by endpoint + model, not by credential: the refusal is a
// property of the model behind the deployment, and a per-user key would
// relearn it per member. Same bounds and TTL as the served-model memo: a
// deployment can be re-pointed at a text-embedding-3 model (until the TTL
// expires it keeps its native vectors, not pgvector-indexed).
var embedDimsRefusals = struct {
	mu sync.Mutex
	m  map[string]time.Time
}{m: map[string]time.Time{}}

func (b *openaiBase) embedDimsKey() string {
	url := ""
	if b.baseURL != nil {
		url = *b.baseURL
	}
	return url + "\x00" + b.embeddingModel
}

// dimensionsRefused reports whether this endpoint + model refused
// `dimensions` within the TTL.
func (b *openaiBase) dimensionsRefused() bool {
	key := b.embedDimsKey()
	embedDimsRefusals.mu.Lock()
	defer embedDimsRefusals.mu.Unlock()
	seen, ok := embedDimsRefusals.m[key]
	if !ok {
		return false
	}
	if time.Since(seen) > servedMemoTTL {
		delete(embedDimsRefusals.m, key)
		return false
	}
	return true
}

// rememberDimensionsRefusal records that this endpoint + model refused
// `dimensions`.
func (b *openaiBase) rememberDimensionsRefusal() {
	embedDimsRefusals.mu.Lock()
	defer embedDimsRefusals.mu.Unlock()
	if len(embedDimsRefusals.m) >= servedMemoMax {
		for k := range embedDimsRefusals.m {
			delete(embedDimsRefusals.m, k)
			break
		}
	}
	embedDimsRefusals.m[b.embedDimsKey()] = time.Now()
}

// resetEmbedDimsRefusals clears the memo (tests, ResetAzureFoundryCaches).
func resetEmbedDimsRefusals() {
	embedDimsRefusals.mu.Lock()
	embedDimsRefusals.m = map[string]time.Time{}
	embedDimsRefusals.mu.Unlock()
}

// rejectsEmbeddingDimensions reports whether an /embeddings failure is the
// model refusing the `dimensions` parameter — OpenAI's and Azure's "This
// model does not support specifying dimensions.", a structured
// `param: "dimensions"`, or a width it cannot produce.
func rejectsEmbeddingDimensions(status int, errBody []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	apiErr, ok := decodeOpenAIError(errBody)
	if !ok {
		return bytes.Contains(bytes.ToLower(errBody), []byte("dimensions"))
	}
	return strings.EqualFold(apiErr.Error.Param, "dimensions") ||
		strings.Contains(strings.ToLower(apiErr.Error.Message), "dimensions")
}

// embed calls the OpenAI-compatible /embeddings endpoint. It is shared by every
// provider whose API mirrors OpenAI's shape (OpenAI, GLM, GitHub Models, Azure
// AI Foundry, …). The embeddings URL is derived from the provider's chat base
// URL by replacing the trailing "/chat/completions" with "/embeddings". Returns
// a "not supported" error when embeddingModel is empty (the provider has no
// embeddings model), so no network call is attempted for providers known to
// lack the endpoint.
//
// Like send(), it makes at most one repair round trip: a 400 refusing the
// `dimensions` parameter (a model outside the text-embedding-3 family behind
// an Azure deployment name, or a width above its native size) is retried once
// without it, so the model returns its native vectors. A 400 saying the batch
// carried more inputs than the resource accepts is split and retried instead
// of failing the index (see embedBatch).
func (b *openaiBase) embed(ctx context.Context, text []string) ([][]float32, error) {
	if b.embeddingModel == "" {
		return nil, fmt.Errorf("embeddings not supported by %s provider", b.providerLabel)
	}
	return b.embedBatch(ctx, text, &embedRequestBudget{remaining: 4*len(text) + 8})
}

// embedBatchLimitRejection matches a 400 saying the request carried more
// INPUTS than the resource accepts — a count rejection, which splitting can
// fix. Some Azure OpenAI resources cap the number of array items per
// /embeddings request (16 on the classic route) and the wording varies ("Too
// many input items in a single request", "batch size", "maximum number of
// input items"), so it matches the RAW body, defensively. Match it through
// embedBatchLimitRejected: per-input SIZE rejections ("too many input tokens",
// "input size exceeds the maximum") must NOT split — no split can shrink a
// single oversized input, and matching them ran a doomed 2n-1-request cascade
// before surfacing the same error.
var embedBatchLimitRejection = regexp.MustCompile(`(?i)too many (inputs?|items?|values?)|batch (size|count|limit)|maximum (number of )?(input items|inputs|items|values)`)

// embedBatchLimitRejected reports whether a 400 body is a batch-COUNT
// rejection a split can fix. RE2 has no negative lookahead, so the count
// pattern cannot exclude the " tokens" continuation itself ("too many input"
// matches inside "too many input tokens"); instead, any mention of "token"
// rules out splitting outright — every observed per-input size rejection is
// about an input's token budget, which no split can shrink.
func embedBatchLimitRejected(errBody []byte) bool {
	if bytes.Contains(bytes.ToLower(errBody), []byte("token")) {
		return false
	}
	return embedBatchLimitRejection.Match(errBody)
}

// embedRequestBudget caps the total /embeddings requests one embed() call may
// make across its whole split cascade. Halving bounds the recursion depth but
// not the request count: a full cascade on n inputs costs 2n-1 requests and
// the per-leaf local retry can double that again, so the budget is set at
// 4n+8 — room for both, while still bounding a server that rejects every
// batch size.
type embedRequestBudget struct{ remaining int }

// embedTerminalError ends a split cascade. It carries the server's own error
// for the message but deliberately does NOT unwrap to it: the retryable
// sentinels (and transport errors) inside would make the OUTER
// RetryingProvider re-run the whole — already locally retried — cascade up to
// 3 more times, multiplying a doomed batch's request count.
type embedTerminalError struct{ cause error }

func (e *embedTerminalError) Error() string { return e.cause.Error() }

// embedLocalRetryDelay is the backoff base for a sub-batch's one local retry.
// Deliberately short: the retry exists to heal a transient blip on one leaf
// inside the cascade, not to ride out a provider outage (the outer wrapper
// still backs off fully on the un-split path).
const embedLocalRetryDelay = 50 * time.Millisecond

// embedLocalRetryMax is the largest server Retry-After hint the leaf retry
// honors. backoff() raises the wait to the raw hint when it exceeds the
// computed delay, so without this cap a 429 with "Retry-After: 120" would park
// each leaf for two minutes — sequentially, stalling the RAG index call far
// past every surrounding budget until a caller deadline aborts the whole op
// instead of surfacing the throttling as a terminal error.
const embedLocalRetryMax = 2 * time.Second

// embedBatch embeds one batch of inputs. On a batch-limit 400 it splits the
// batch in half and recurses, concatenating the sub-results in input order.
// Halving bounds the recursion depth at log2(len) and a one-input batch is
// never split, so a server that rejects every batch size surfaces the error
// after at most 2n-1 requests instead of looping. Nothing is split up front:
// a provider that accepts the full batch sees exactly one request. budget
// bounds the cascade's total request count; sub-batches additionally get one
// local retry for transient failures (embedSubBatch).
func (b *openaiBase) embedBatch(ctx context.Context, text []string, budget *embedRequestBudget) ([][]float32, error) {
	payload := map[string]any{
		"model": b.embeddingModel,
		"input": text,
	}
	// A refusal already learned for this endpoint + model (by an earlier
	// Embed, or by the parent batch of a split cascade) is not paid again.
	if d := b.embeddingDimensions(); d > 0 && !b.dimensionsRefused() {
		payload["dimensions"] = d
	}
	for {
		if !budget.take() {
			return nil, &embedTerminalError{cause: fmt.Errorf("%s embeddings: request budget exhausted", b.providerLabel)}
		}
		resp, err := b.postEmbeddings(ctx, payload)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return decodeEmbeddings(resp, len(text))
		}
		// Classified like a chat failure so a 401 reads as an invalid key and
		// a 429 is retried by the wrapper chain instead of failing the index.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if _, sent := payload["dimensions"]; sent && rejectsEmbeddingDimensions(resp.StatusCode, errBody) {
			// Deleting the key makes this branch unreachable next time: the
			// loop runs at most twice.
			delete(payload, "dimensions")
			b.rememberDimensionsRefusal()
			logger.Debug("embedding model rejected `dimensions` — retrying at its native width",
				"provider", b.providerLabel, "model", b.embeddingModel)
			continue
		}
		if resp.StatusCode == http.StatusBadRequest && len(text) > 1 && embedBatchLimitRejected(errBody) {
			// Both halves need at least one request each; without the room the
			// cascade would outrun its budget, so the server's own rejection
			// surfaces instead of splitting further.
			if budget.remaining < 2 {
				return nil, &embedTerminalError{cause: fmt.Errorf("%s embeddings: %w", b.providerLabel, b.handleChatError(resp, errBody))}
			}
			logger.Debug("embedding batch exceeded the resource's input cap — splitting",
				"provider", b.providerLabel, "model", b.embeddingModel, "inputs", len(text))
			mid := len(text) / 2
			left, err := b.embedSubBatch(ctx, text[:mid], budget)
			if err != nil {
				return nil, err
			}
			right, err := b.embedSubBatch(ctx, text[mid:], budget)
			if err != nil {
				return nil, err
			}
			return append(left, right...), nil
		}
		return nil, fmt.Errorf("%s embeddings: %w", b.providerLabel, b.handleChatError(resp, errBody))
	}
}

// take consumes one request from the budget; false when it is spent.
func (b *embedRequestBudget) take() bool {
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}

// embedSubBatch runs one half of a split with one local retry (two attempts)
// for transient failures, honoring the server's Retry-After/backoff hints only
// up to embedLocalRetryMax. A transient error on a single leaf used to fail
// the whole cascade AND be re-run whole by the outer RetryingProvider; here it
// heals inside the cascade, and the final error is terminal
// (embedTerminalError) so the outer wrapper never multiplies the request count.
func (b *openaiBase) embedSubBatch(ctx context.Context, text []string, budget *embedRequestBudget) ([][]float32, error) {
	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			if budget.remaining < 1 {
				return nil, &embedTerminalError{cause: lastErr}
			}
			// A hint beyond embedLocalRetryMax says the provider wants this
			// leaf parked for seconds-to-minutes; honoring it here would sleep
			// the whole cascade out past every budget around the index op.
			// Skip the retry and fail the leaf terminally instead.
			if retryAfterFrom(lastErr) > embedLocalRetryMax {
				return nil, &embedTerminalError{cause: lastErr}
			}
			if err := backoff(ctx, attempt-1, lastErr, embedLocalRetryDelay); err != nil {
				return nil, err
			}
		}
		vecs, err := b.embedBatch(ctx, text, budget)
		if err == nil {
			return vecs, nil
		}
		var terminal *embedTerminalError
		if errors.As(err, &terminal) || !isRetryable(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, &embedTerminalError{cause: lastErr}
}

// postEmbeddings sends one /embeddings request with the provider's auth
// headers. The embeddings URL is the chat URL with its trailing
// "/chat/completions" replaced by "/embeddings".
func (b *openaiBase) postEmbeddings(ctx context.Context, payload map[string]any) (*http.Response, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	embedURL := strings.Replace(*b.baseURL, "/chat/completions", "/embeddings", 1)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", embedURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	if err := b.setAuth(ctx, httpReq); err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if b.extraHeaders != nil {
		b.extraHeaders(httpReq, b.embeddingModel)
	}
	return b.client.Do(httpReq)
}

// maxJSONBodyBytes caps a 200 JSON body before decoding. These endpoints
// (embeddings, model listings, OAuth responses) answer bounded payloads; an
// unbounded decode let a hostile or broken server buffer arbitrary memory.
const maxJSONBodyBytes = 32 << 20

// decodeEmbeddings reads a 200 /embeddings response (and closes its body)
// for want inputs. Items are placed by their `index` when every item carries
// one (the schema defines it so array order need not be relied on; a
// permutation stored positionally would be undetectable by the callers,
// which check only the count); a response without indices is read
// positionally. The count must match the inputs.
func decodeEmbeddings(resp *http.Response, want int) ([][]float32, error) {
	defer resp.Body.Close()
	var parsed struct {
		Data []struct {
			Index     *int      `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBodyBytes)).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Data) != want {
		return nil, fmt.Errorf("embeddings response carried %d vectors for %d inputs", len(parsed.Data), want)
	}
	res := make([][]float32, want)
	indexed := true
	for _, d := range parsed.Data {
		if d.Index == nil {
			indexed = false
			break
		}
	}
	if !indexed {
		for i, d := range parsed.Data {
			res[i] = d.Embedding
		}
		return res, nil
	}
	seen := make([]bool, want)
	for _, d := range parsed.Data {
		i := *d.Index
		if i < 0 || i >= want || seen[i] {
			return nil, fmt.Errorf("embeddings response has an invalid or duplicate index %d for %d inputs", i, want)
		}
		seen[i] = true
		res[i] = d.Embedding
	}
	return res, nil
}
