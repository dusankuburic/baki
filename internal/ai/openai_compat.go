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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"pad-core/cache"
	"pad-core/logger"
)

// Which request field carries the completion-length cap: max_tokens is the original most servers understand;
// max_completion_tokens is its successor, mandatory for reasoning models (o-series, GPT-5) and Foundry.
const (
	maxTokensParamLegacy     = "max_tokens"
	maxTokensParamCompletion = "max_completion_tokens"
)

type openaiBase struct {
	// apiKey is the static bearer token, used when tokenFn is nil.
	apiKey string
	// tokenFn resolves the bearer token per-call for dynamic tokens (Copilot's session token, an Entra access
	// token); takes precedence over apiKey; errors propagate — a stale token wastes no round trip.
	tokenFn func(ctx context.Context) (string, error)
	client  *http.Client
	// longWaitClient, when set, serves the non-streaming chat requests (chat, streamViaChat): the fixed-host
	// family's client with the long header floor (fixedHostClientFor(false)). nil uses client, whose own floor
	// already allows the wait (Azure, local) or is a test's.
	longWaitClient *http.Client
	baseURL        *string
	providerLabel  string
	// providerID is the registry id (the spec's): the provider label of the repair and fallback counters.
	providerID string
	// embeddingModel is the model name sent to /embeddings. Empty means the provider does not expose embeddings
	// and embed() refuses without a network call.
	embeddingModel string
	// embeddingModelDefault is the provider's shipped default, preserved so an empty override can restore it
	// (and so an empty default means "no embeddings").
	embeddingModelDefault string
	// embedDimensions, when > 0, is sent as `dimensions` for embedding models that support Matryoshka
	// truncation (text-embedding-3-*), never above a model's native width (see embeddingDimensionsFor).
	embedDimensions int
	// embedDimensionsFor, when set, replaces embeddingDimensionsFor for providers whose embedding "model" is
	// an opaque alias (Azure AI Foundry deployment names).
	embedDimensionsFor func(model string, n int) int
	// maxTokensParam is maxTokensParamLegacy ("" behaves the same) or maxTokensParamCompletion — see above.
	maxTokensParam string
	extraHeaders   func(req *http.Request, model string)
	handle429      func(resp *http.Response, apiErr openAIErrorResp) error
	// handle401, when set, classifies a 401/403 ahead of unauthorizedError (nil falls through to it): Azure AI
	// Foundry in Entra ID mode names the rejected access token (see entraUnauthorized).
	handle401 func(apiErr openAIErrorResp, respBody []byte) error
	// handle403, when set, classifies a 403 ahead of handle401: Foundry answers 403 for network rules or a role
	// refusal — the credential was accepted, so a fresh token cannot help (azureForbidden).
	handle403 func(apiErr openAIErrorResp, respBody []byte) error
	// rerouteToolsRejection, when set (Azure AI Foundry), owns a tools-with-reasoning rejection (gpt-5.6):
	// reroutes through the Responses API; false falls back to the built-in reasoning_effort "none" repair.
	rerouteToolsRejection func(ctx context.Context, req Request, onChunk func(Chunk)) (bool, error)
	// apiKeyHeader, when set, is the header the static apiKey travels in INSTEAD of Authorization: Bearer (Foundry's `api-key`).
	apiKeyHeader string
}

// setAuth sets the credential header: the bearer token, or the static key in apiKeyHeader when the provider names one.
func (b *openaiBase) setAuth(ctx context.Context, req *http.Request) error {
	token, err := b.resolveToken(ctx)
	if err != nil {
		return fmt.Errorf("get %s token: %w", b.providerLabel, err)
	}
	if b.apiKey == "" && b.tokenFn == nil && b.apiKeyHeader == "" {
		// No credential at all (the local models provider): send no auth header rather than an empty "Authorization: Bearer ".
		return nil
	}
	if b.apiKeyHeader != "" && b.tokenFn == nil {
		req.Header.Set(b.apiKeyHeader, token)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// unauthorized classifies a 401/403: 403 → handle403, then handle401, then the shared unauthorizedError (chat, stream, embeddings).
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
	if status == http.StatusForbidden && b.handle403 == nil {
		// The key was read: a 403 is the provider refusing this account (region, project, billing), and
		// re-entering the key cannot help (R-provider-25).
		return &accessDeniedError{msg: cmp.Or(vendorMessage(apiErr.Error.Message), "the provider answered 403")}
	}
	return unauthorizedError(apiErr)
}

// vendorMessage is a provider's error message on one line, cut at 500 bytes.
func vendorMessage(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 500 {
		msg = strings.ToValidUTF8(msg[:500], "") + "…"
	}
	return msg
}

// quotaExhausted is the handle429 of the OpenAI-family providers that bill by account (OpenAI, xAI, GitHub
// Models, Copilot): a 429 saying the quota is exhausted (OpenAI's insufficient_quota) is the
// balance error — retried three times and told to "wait a moment" it never recovers. nil keeps the rate limit.
func quotaExhausted(_ *http.Response, apiErr openAIErrorResp) error {
	code := strings.ToLower(apiErr.Error.Code + " " + apiErr.Error.Type)
	if strings.Contains(code, "rate_limit_exceeded") {
		return nil // a per-minute limit, whatever its text links to (OpenAI's lower tiers link the billing page)
	}
	msg := strings.ToLower(apiErr.Error.Message)
	if strings.Contains(code, "insufficient_quota") || strings.Contains(msg, "exceeded your current quota") {
		return fmt.Errorf("%w: %s", ErrInsufficientBalance, vendorMessage(apiErr.Error.Message))
	}
	return nil
}

// setEmbeddingModel overrides the model sent to /embeddings; empty resets to the shipped default. Satisfies
// embedModelSetter so ProviderFactory.ForEmbedding can apply a deployer-chosen model.
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

// EmbeddingModel satisfies embedModelNamer so the audited wrapper can price embedding usage against the model actually sent.
func (b *openaiBase) EmbeddingModel() string { return b.embeddingModel }

// resolveToken returns the bearer token: tokenFn when set (dynamic token, e.g. Copilot's session exchange), else the static apiKey.
func (b *openaiBase) resolveToken(ctx context.Context) (string, error) {
	if b.tokenFn != nil {
		return b.tokenFn(ctx)
	}
	return b.apiKey, nil
}

// buildRequest maps the neutral Request onto the OpenAI chat-completions body; exactly one of max_tokens / max_completion_tokens is set (maxTokensParam).
func (b *openaiBase) buildRequest(req Request, stream bool) openAIRequest {
	body := openAIRequest{
		Model:    req.Model,
		Messages: toOpenAIMessages(req.SystemPrompt, req.Messages),
		Tools:    toOpenAITools(req.Tools),
	}
	// An explicitly chosen temperature goes out even when it is 0; an unset zero keeps meaning "the provider's default" and is omitted.
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
	// A user-chosen reasoning depth ("" is the provider default); the gpt-5.6 tools repair overrides it with "none".
	if req.ReasoningEffort != "" {
		body.ReasoningEffort = req.ReasoningEffort
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
	client := b.client
	if !stream && b.longWaitClient != nil {
		client = b.longWaitClient
	}
	return doProviderRequest(client, httpReq)
}

// send POSTs body to the chat endpoint; a 200 comes back with its body UNREAD, any other status read and closed.
// One repair round trip per parameter (capped), memoised per endpoint+model and pre-applied — paid once per deployment.
func (b *openaiBase) send(ctx context.Context, body openAIRequest, stream bool) (*http.Response, []byte, error) {
	resp, errBody, err, _ := b.sendWith(ctx, body, stream, sendOptions{})
	return resp, errBody, err
}

// sendOptions tweaks sendWith's repair behaviour.
type sendOptions struct {
	// escapeToolsReasoning leaves a tools-with-reasoning rejection (gpt-5.6) UNREPAIRED and returns it to the
	// caller, which reroutes the turn through the Responses API instead of losing reasoning to "none".
	escapeToolsReasoning bool
}

// sendWith is send with options; its last return reports whether the request went out with reasoning_effort
// "none" for the tools-with-reasoning repair (freshly or from the memo). A generic drop of a refused reasoning_effort does not count.
func (b *openaiBase) sendWith(ctx context.Context, body openAIRequest, stream bool, opts sendOptions) (*http.Response, []byte, error, bool) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err), false
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
	// repaired guards the loop (each parameter at most once) and is what a 200 memoises; learned marks
	// pre-applied memo entries; toolsNone: the payload carries reasoning_effort "none".
	repaired := map[string]bool{}
	learned := map[string]bool{}
	toolsNone := false
	memoKey := b.repairsKey(body.Model)
	if params := repairsFor(memoKey); len(params) > 0 && ensurePayload() {
		for _, param := range params {
			if (param == reasoningEffortParam || param == reasoningEffortNoneRepair) && isModelRouterAlias(body.Model) {
				// A model router picks the model per request: the model that needed "none" last time says nothing about the next.
				continue
			}
			if param == reasoningEffortNoneRepair {
				if hasTools {
					payload[reasoningEffortParam] = "none"
					repaired[param], learned[param] = true, true
					toolsNone = true
				}
				continue
			}
			// Every other entry removes (or swaps) the refused parameter.
			if repairParam(payload, param) {
				repaired[param], learned[param] = true, true
			}
		}
		if len(learned) > 0 {
			if err := remarshal(); err != nil {
				return nil, nil, err, false
			}
		}
	}
	// memoStale: a pre-applied repair was rejected this round, so the request must not be memoised.
	memoStale := false
	roundTrips := 0
	// toolsResp/toolsBody keep the tools rejection that triggered the repair.
	var (
		toolsResp *http.Response
		toolsBody []byte
	)
	for {
		resp, err := b.post(ctx, jsonBody, body.Model, stream)
		if err != nil {
			return nil, nil, err, false
		}
		if resp.StatusCode == http.StatusOK {
			if len(repaired) > 0 && !memoStale {
				rememberRepairs(memoKey, repaired)
			}
			return resp, nil, nil, toolsNone
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		b.logUpstreamRejection(ctx, resp, body.Model)
		if roundTrips >= maxSendRepairs {
			return resp, errBody, nil, false
		}
		// Before the generic repair (and detectToolsUnsupportedError): the model accepts function tools once reasoning is off.
		if hasTools && !toolsNone && !repaired[reasoningEffortParam] &&
			reasoningToolsRejected(resp.StatusCode, errBody) && ensurePayload() {
			if opts.escapeToolsReasoning {
				// The caller reroutes the turn: hand it the rejection.
				return resp, errBody, nil, false
			}
			payload[reasoningEffortParam] = "none"
			repaired[reasoningEffortNoneRepair] = true
			toolsNone = true
			toolsResp, toolsBody = resp, errBody
			recordParamRepair(b.providerID, reasoningEffortNoneRepair)
			logger.FromContext(ctx).Debug("model rejects function tools with reasoning on chat completions — retrying with reasoning_effort=none",
				"provider", b.providerLabel, "model", body.Model)
			if err := remarshal(); err != nil {
				return nil, nil, err, false
			}
			roundTrips++
			continue
		}
		param, ok := unsupportedParam(resp.StatusCode, errBody)
		if !ok {
			return resp, errBody, nil, false
		}
		if param == reasoningEffortParam && toolsNone {
			if toolsBody != nil {
				// The model refuses "none" as well: report the original tools rejection, which the caller
				// reads as ErrToolsUnsupported (the tool loop degrades, not fails).
				return toolsResp, toolsBody, nil, false
			}
			// The memo's "none" is what the model refuses now (a re-pointed deployment): forget the memo and repair afresh.
			forgetRepairs(memoKey)
			memoStale = true
			delete(learned, reasoningEffortNoneRepair)
			delete(repaired, reasoningEffortNoneRepair)
			toolsNone = false
		}
		// A user-chosen reasoning_effort VALUE the model refuses is retried without the field but NOT memoised —
		// the memo is keyed by parameter, so it would make every later valid value vanish too.
		if param == reasoningEffortParam && !repaired[param] &&
			parameterValueRejected(resp.StatusCode, errBody) && ensurePayload() {
			if _, present := payload[reasoningEffortParam]; present {
				delete(payload, reasoningEffortParam)
				recordParamRepair(b.providerID, reasoningEffortParam)
				logger.FromContext(ctx).Warn("the model refused the configured reasoning effort — retrying without it (the provider default applies)",
					"provider", b.providerLabel, "model", body.Model)
				if err := remarshal(); err != nil {
					return nil, nil, err, false
				}
				roundTrips++
				continue
			}
		}
		if learned[param] {
			// The memo's rewrite is now rejected: forget the memo and repair once more.
			forgetRepairs(memoKey)
			memoStale = true
			delete(learned, param)
			delete(repaired, param)
		}
		if !repaired[param] && ensurePayload() && repairParam(payload, param) {
			repaired[param] = true
			recordParamRepair(b.providerID, param)
			logger.FromContext(ctx).Debug("AI request parameter rejected by the model — retrying without it",
				"provider", b.providerLabel, "model", body.Model, "param", param)
			if err := remarshal(); err != nil {
				return nil, nil, err, false
			}
			roundTrips++
			continue
		}
		return resp, errBody, nil, false
	}
}

// maxSendRepairs caps send's repair round trips per request (each parameter once; bounds a server naming new ones).
const maxSendRepairs = 4

// learnedRepairs remembers, per endpoint and model, the parameters send had to repair — bounded (least recently
// used first, ai.param-repairs) and short-lived (a deployment can be re-pointed); shared with the Responses path via
// repairsFor / rememberRepairs. learnedRepairsMu serialises rememberRepairs's read-merge-write.
var (
	learnedRepairs   = cache.New[learnedRepair](paramRepairsSpec, learnedRepairsMax, cache.WithMaxEntries(learnedRepairsMax), cache.WithTTL(learnedRepairsTTL))
	learnedRepairsMu sync.Mutex
)

type learnedRepair struct {
	params []string
	seen   time.Time
}

const (
	learnedRepairsMax = 1024
	learnedRepairsTTL = time.Hour
)

func (b *openaiBase) repairsKey(model string) string {
	url := ""
	if b.baseURL != nil {
		url = *b.baseURL
	}
	return url + "\x00" + model
}

// repairsFor returns the remembered repairs for key, sorted; nil when none or expired.
func repairsFor(key string) []string {
	e, ok := liveRepair(key)
	if !ok {
		return nil
	}
	return e.params
}

// liveRepair is key's memo entry within its TTL; an expired one is dropped.
func liveRepair(key string) (learnedRepair, bool) {
	k := memoKey(paramRepairsSpec, key)
	e, ok := learnedRepairs.Get(k)
	if !ok {
		return learnedRepair{}, false
	}
	if time.Since(e.seen) > learnedRepairsTTL {
		learnedRepairs.Delete(k)
		return learnedRepair{}, false
	}
	return e, true
}

// rememberRepairs records the repairs a 200 confirmed for key, MERGED with the live entry (R-provider-31): a request
// that did not send a learned parameter (no temperature, no tools for reasoning_effort=none) did not apply that
// repair, and must not make the memo forget it. A repair the model now refuses is dropped by forgetRepairs instead.
func rememberRepairs(key string, repaired map[string]bool) {
	learnedRepairsMu.Lock()
	defer learnedRepairsMu.Unlock()
	merged := make(map[string]bool, len(repaired))
	for p := range repaired {
		merged[p] = true
	}
	if live, ok := liveRepair(key); ok {
		for _, p := range live.params {
			merged[p] = true
		}
	}
	params := make([]string, 0, len(merged))
	for p := range merged {
		params = append(params, p)
	}
	sort.Strings(params)
	learnedRepairs.Set(memoKey(paramRepairsSpec, key), learnedRepair{params: params, seen: time.Now()}, 1, 0)
}

// forgetRepairs drops key's memo (a remembered repair the model now refuses).
func forgetRepairs(key string) {
	learnedRepairsMu.Lock()
	defer learnedRepairsMu.Unlock()
	learnedRepairs.Delete(memoKey(paramRepairsSpec, key))
}

func resetLearnedRepairs() {
	learnedRepairs.Clear()
	streamRefusals.Clear()
}

// reasoningEffortParam is the chat-completions reasoning control; as a learnedRepairs entry it is the generic drop repair.
const reasoningEffortParam = "reasoning_effort"

// reasoningEffortNoneRepair is the learnedRepairs marker of the tools-with-reasoning repair (gpt-5.6): tools
// requests go out with reasoning_effort "none", told apart from a plain drop.
const reasoningEffortNoneRepair = "reasoning_effort=none"

// reasoningToolsRejection matches gpt-5.6's (and later models') chat-completions refusal of function tools while
// reasoning is on ("Function tools with reasoning_effort are not supported … set reasoning_effort to 'none'").
var reasoningToolsRejection = regexp.MustCompile(`(?i)function tools with reasoning_effort are not supported|set reasoning_effort to '?none'?`)

// parameterRejectionStatus reports a status a parameter rejection arrives with: 400 (OpenAI, Azure OpenAI) or
// 422 (the Azure AI Model Inference layer behind Foundry Models sold directly by Azure).
func parameterRejectionStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
}

// parameterValueRejected reports whether a rejection is about the parameter's VALUE (unsupported_value), not its existence.
func parameterValueRejected(status int, errBody []byte) bool {
	if !parameterRejectionStatus(status) {
		return false
	}
	apiErr, ok := decodeOpenAIError(errBody)
	return ok && apiErr.Error.Code == "unsupported_value"
}

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

// decodeOpenAIError decodes an OpenAI {"error":{…}} envelope or the Azure AI Model Inference layer's TOP-LEVEL
// {status,code,message,detail} shape (param from `param` else detail.loc[1]; adopted only with a code/detail, so
// API Management's 401 envelope keeps an empty Error). ok is false when not JSON at all.
func decodeOpenAIError(body []byte) (openAIErrorResp, bool) {
	// code, type and param are decoded raw (rawText): vLLM sends the HTTP status as an integer code, which a
	// string field failed to decode — the whole envelope, message included, was lost (R-provider-33).
	var raw struct {
		Error struct {
			Message string          `json:"message"`
			Type    json.RawMessage `json:"type"`
			Code    json.RawMessage `json:"code"`
			Param   json.RawMessage `json:"param"`
		} `json:"error"`
	}
	var apiErr openAIErrorResp
	strictOK := json.Unmarshal(body, &raw) == nil
	if strictOK {
		apiErr.Error.Message = raw.Error.Message
		apiErr.Error.Type = rawText(raw.Error.Type)
		apiErr.Error.Code = rawText(raw.Error.Code)
		apiErr.Error.Param = rawText(raw.Error.Param)
	}
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

// unsupportedParamPatterns recognise rejections that name the parameter only in prose (no structured `param`):
// "Unsupported parameter: 'x'", "Unrecognized request argument supplied: x", the Model Inference layer's "Extra parameters ['x']".
var unsupportedParamPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)unsupported parameter:? '([a-z_]+)'`),
	regexp.MustCompile(`(?i)unsupported value:? '([a-z_]+)'`),
	regexp.MustCompile(`(?i)unrecognized request argument supplied: ([a-z_]+)`),
	regexp.MustCompile(`(?i)'([a-z_]+)' is not supported with this model`),
	regexp.MustCompile(`(?i)extra parameters? \[?'([a-z_]+)'`),
}

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

// repairableParams lists the ONLY parameters send may drop or swap after a rejection; anything else (model,
// messages, tools, …) is load-bearing and a rejection naming it must surface as a real error.
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
	// The Responses path's include:["reasoning.encrypted_content"]: an endpoint that refuses it answers without replayable reasoning.
	"include": true,
}

// repairParam rewrites payload so the rejected parameter is gone (the two completion-length names swap, same value).
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
	// A completion the filter cut short is not an answer: say so, with the usage riding on the error so the blocked tokens are billed.
	cacheRead := 0
	if d := parsed.Usage.PromptTokensDetails; d != nil {
		cacheRead = min(d.CachedTokens, parsed.Usage.PromptTokens)
	}
	if choice.FinishReason == "content_filter" {
		return nil, &heldUsageError{
			err:         fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered),
			tokensIn:    parsed.Usage.PromptTokens,
			tokensOut:   parsed.Usage.CompletionTokens,
			servedModel: parsed.Model,
		}
	}
	// A leading <think>…</think> block (DeepSeek-R1, …) is reasoning, not the answer; a separate
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
		Content:         content,
		Reasoning:       reasoning,
		TokensIn:        parsed.Usage.PromptTokens,
		TokensOut:       parsed.Usage.CompletionTokens,
		CacheReadTokens: cacheRead,
		FinishReason:    choice.FinishReason,
		ToolCalls:       openAIToolCallsToNeutral(choice.Message.ToolCalls),
		ServedModel:     parsed.Model,
	}, nil
}

// isContentFilterError recognises a prompt the content filter rejected outright: Azure OpenAI / Foundry answer
// 400 with error.code "content_filter" (inner ResponsibleAIPolicyViolation) or "content management policy" prose.
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

// unauthorizedError classifies a 401. Azure AI Foundry's "PermissionDenied" means the credential WAS accepted —
// a valid Entra ID token whose principal lacks the data-plane role — so it keeps the service's explanation.
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

// handleChatError classifies a non-200 chat response keyed by STATUS first — a gateway 429/502 with an empty
// or HTML body must still count for the retry wrapper and the breaker.
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

// upstreamRequestID is the gateway's correlation id for a response — apim-request-id / x-ms-request-id /
// x-request-id — the one handle support can search for. Carries no secret.
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

// requestIDSuffix formats the request id for the residual error texts, so a failure can be correlated with Azure's side.
func requestIDSuffix(resp *http.Response) string {
	if rid := upstreamRequestID(resp); rid != "" {
		return ", request id " + rid
	}
	return ""
}

// logUpstreamRejection records every non-200 once: its status and, when the gateway sent one, its request id
// for Azure support. A rejection without an id is logged too: a bare gateway 503 is exactly the outage that
// must leave a line. A parameter rejection may still be repaired by send, so it logs at debug.
func (b *openaiBase) logUpstreamRejection(ctx context.Context, resp *http.Response, model string) {
	logUpstreamStatus(ctx, b.providerLabel, model, resp, parameterRejectionStatus(resp.StatusCode))
}

// logUpstreamStatus logs one non-2xx upstream response through the caller's context (R-observability-2): status,
// provider, model and the vendor's request id when it sent one. repairable logs at debug: the caller may still
// repair the request and succeed.
func logUpstreamStatus(ctx context.Context, provider, model string, resp *http.Response, repairable bool) {
	attrs := []any{"provider", provider, "model", model, "status", resp.StatusCode}
	if rid := upstreamRequestID(resp); rid != "" {
		attrs = append(attrs, "requestId", rid)
	}
	if repairable {
		logger.FromContext(ctx).Debug("AI upstream rejection", attrs...)
		return
	}
	logger.FromContext(ctx).Warn("AI upstream error", attrs...)
}

func (b *openaiBase) stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	// A provider with a reroute hook (Azure AI Foundry) leaves a tools-with-reasoning rejection unrepaired so
	// the hook can move the turn to the Responses API.
	return b.streamInner(ctx, req, onChunk, b.rerouteToolsRejection != nil && len(req.Tools) > 0)
}

// streamInner is stream with the reroute escape (see sendOptions.escapeToolsReasoning).
func (b *openaiBase) streamInner(ctx context.Context, req Request, onChunk func(Chunk), escape bool) error {
	key := b.repairsKey(req.Model)
	if streamRefused(key) {
		return b.streamViaChat(ctx, req, onChunk)
	}
	resp, errBody, err, reasoningOff := b.sendWith(ctx, b.buildRequest(req, true), true, sendOptions{escapeToolsReasoning: escape})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close() // send already read and closed it; harmless
		if param, ok := unsupportedParam(resp.StatusCode, errBody); ok && param == "stream" {
			// The model does not stream at all (o1 on Azure): answer the turn non-streaming and remember the deployment.
			rememberStreamRefusal(key)
			recordRouteFallback(b.providerID, fallbackStreamRefused)
			logger.FromContext(ctx).Debug("model rejects streaming — answering non-streaming",
				"provider", b.providerLabel, "model", req.Model)
			return b.streamViaChat(ctx, req, onChunk)
		}
		if escape && reasoningToolsRejected(resp.StatusCode, errBody) {
			// The hook reroutes the turn through the Responses API, which keeps reasoning with tools.
			if handled, err := b.rerouteToolsRejection(ctx, req, onChunk); handled {
				return err
			}
			// The Responses route refused the model (memoised): answer on chat completions with the built-in repair.
			return b.streamInner(ctx, req, onChunk, false)
		}
		return b.handleStreamError(resp, errBody)
	}
	if reasoningOff {
		// The turn runs on the "none" repair (gpt-5.6 with tools): say so, or the user pays for reasoning-backed quality that is not there.
		onChunk(Chunk{Notice: &Notice{Name: "reasoning_off",
			Label: "This model can't reason while using tools — answering with reasoning off"}})
	}
	defer drainAndClose(resp.Body)
	return parseOpenAISSE(resp.Body, onChunk)
}

// streamViaChatKeepalive is how often streamViaChat emits a keepalive while its non-streaming request runs:
// the idle watchdog only sees chunks, and a reasoning model can spend minutes before the first header.
var streamViaChatKeepalive = 20 * time.Second

// streamViaChat serves a Stream call with one non-streaming request, replaying the reply as chunks plus a
// keepalive per streamViaChatKeepalive so the idle watchdog cannot cancel it; a content-filter stop keeps its usage.
func (b *openaiBase) streamViaChat(ctx context.Context, req Request, onChunk func(Chunk)) error {
	type chatResult struct {
		resp *Response
		err  error
	}
	done := make(chan chatResult, 1)
	go func() {
		resp, err := b.chat(ctx, req)
		done <- chatResult{resp: resp, err: err}
	}()
	ticker := time.NewTicker(streamViaChatKeepalive)
	defer ticker.Stop()
	var res chatResult
	for {
		select {
		case res = <-done:
		case <-ticker.C:
			onChunk(Chunk{LocalKeepalive: true})
			continue
		}
		break
	}
	resp, err := res.resp, res.err
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
		Done:             true,
		TokensIn:         resp.TokensIn,
		TokensOut:        resp.TokensOut,
		CacheReadTokens:  resp.CacheReadTokens,
		CacheWriteTokens: resp.CacheWriteTokens,
		ToolCalls:        resp.ToolCalls,
		FinishReason:     resp.FinishReason,
		ServedModel:      resp.ServedModel,
	})
	return nil
}

// streamRefusals remembers, per endpoint and deployment (repairsKey), the models that refused `stream: true` —
// separate from learnedRepairs, whose forget path must not sweep it. The value is when the refusal was seen.
var streamRefusals = cache.New[time.Time](streamRefusalsSpec, learnedRepairsMax, cache.WithMaxEntries(learnedRepairsMax), cache.WithTTL(learnedRepairsTTL))

func streamRefused(key string) bool {
	k := memoKey(streamRefusalsSpec, key)
	seen, ok := streamRefusals.Get(k)
	if !ok {
		return false
	}
	if time.Since(seen) > learnedRepairsTTL {
		streamRefusals.Delete(k)
		return false
	}
	return true
}

func rememberStreamRefusal(key string) {
	streamRefusals.Set(memoKey(streamRefusalsSpec, key), time.Now(), 1, 0)
}

// Bounds for drainAndClose: a server that keeps the stream open must not hold the caller.
const (
	drainMaxBytes = 64 << 10
	drainMaxWait  = 200 * time.Millisecond
)

// drainAndClose reads what is left of a finished stream (the chunked terminator) before closing it, so the
// transport reuses the connection; bounded in bytes and time, and Close unblocks the drain.
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

// handleStreamError classifies a non-200 stream response, status first for the same reason as handleChatError;
// the message-dependent branches also require a non-empty error.message, as they always have.
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

// Native vector widths of OpenAI's text-embedding-3 models; Matryoshka truncation can only SHORTEN a vector:
// more than native is a 400.
const (
	textEmbedding3SmallWidth = 1536
	textEmbedding3LargeWidth = 3072
)

// embeddingDimensionsFor returns the `dimensions` to send for model at width n, or 0 for none. Only
// text-embedding-3 accepts the parameter, and never above native width — a wider ask keeps the native vectors.
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

func (b *openaiBase) embeddingDimensions() int {
	if b.embedDimensionsFor != nil {
		return b.embedDimensionsFor(b.embeddingModel, b.embedDimensions)
	}
	return embeddingDimensionsFor(b.embeddingModel, b.embedDimensions)
}

// embedDimsRefusals remembers embedding endpoints (URL + model) that refused `dimensions`, so the repair round
// trip is paid once per process (the factory builds a fresh provider per request) instead of on every Embed. The value
// is when the refusal was seen.
var embedDimsRefusals = cache.New[time.Time](embedDimsRefusalsSpec, servedMemoMax, cache.WithMaxEntries(servedMemoMax), cache.WithTTL(servedMemoTTL))

func (b *openaiBase) embedDimsKey() string {
	url := ""
	if b.baseURL != nil {
		url = *b.baseURL
	}
	return url + "\x00" + b.embeddingModel
}

// dimensionsRefused reports whether this endpoint + model refused `dimensions` within the TTL.
func (b *openaiBase) dimensionsRefused() bool {
	k := memoKey(embedDimsRefusalsSpec, b.embedDimsKey())
	seen, ok := embedDimsRefusals.Get(k)
	if !ok {
		return false
	}
	if time.Since(seen) > servedMemoTTL {
		embedDimsRefusals.Delete(k)
		return false
	}
	return true
}

func (b *openaiBase) rememberDimensionsRefusal() {
	embedDimsRefusals.Set(memoKey(embedDimsRefusalsSpec, b.embedDimsKey()), time.Now(), 1, 0)
}

func resetEmbedDimsRefusals() { embedDimsRefusals.Clear() }

// rejectsEmbeddingDimensions reports whether an /embeddings failure (400 or the Model Inference layer's 422) is
// the model refusing `dimensions` — prose, a structured `param: "dimensions"`, or a width it cannot produce.
func rejectsEmbeddingDimensions(status int, errBody []byte) bool {
	if !parameterRejectionStatus(status) {
		return false
	}
	apiErr, ok := decodeOpenAIError(errBody)
	if !ok {
		return bytes.Contains(bytes.ToLower(errBody), []byte("dimensions"))
	}
	return strings.EqualFold(apiErr.Error.Param, "dimensions") ||
		strings.Contains(strings.ToLower(apiErr.Error.Message), "dimensions")
}

// embed calls the OpenAI-compatible /embeddings endpoint (chat URL with /chat/completions → /embeddings); an
// empty embeddingModel refuses without a network call, a `dimensions` refusal is retried without it, a batch-count 400 splits.
func (b *openaiBase) embed(ctx context.Context, text []string) ([][]float32, error) {
	if b.embeddingModel == "" {
		return nil, fmt.Errorf("embeddings not supported by %s provider", b.providerLabel)
	}
	return b.embedBatch(ctx, text, &embedRequestBudget{remaining: 4*len(text) + 8})
}

// embedBatchLimitRejection matches a 400 for more INPUTS than the resource accepts — a count rejection splitting
// can fix; per-input SIZE rejections must NOT split (see embedBatchLimitRejected).
var embedBatchLimitRejection = regexp.MustCompile(`(?i)too many (inputs?|items?|values?)|batch (size|count|limit)|maximum (number of )?(input items|inputs|items|values)`)

// embedBatchLimitRejected reports whether a 400 is a batch-COUNT rejection a split can fix: RE2 has no negative
// lookahead, so any mention of "token" rules out splitting instead.
func embedBatchLimitRejected(errBody []byte) bool {
	if bytes.Contains(bytes.ToLower(errBody), []byte("token")) {
		return false
	}
	return embedBatchLimitRejection.Match(errBody)
}

// embedRequestBudget caps the total /embeddings requests one embed() call may make across its whole split
// cascade (a full cascade on n inputs costs 2n-1 and the per-leaf retry can double that).
type embedRequestBudget struct{ remaining int }

// embedTerminalError ends a split cascade. It carries the server's own error for the message but deliberately
// does NOT unwrap to it: the retryable sentinels inside would make the outer RetryingProvider re-run the cascade.
type embedTerminalError struct{ cause error }

func (e *embedTerminalError) Error() string { return e.cause.Error() }

// embedLocalRetryDelay is the backoff base for a sub-batch's one local retry: to heal a transient blip on one leaf.
const embedLocalRetryDelay = 50 * time.Millisecond

// embedLocalRetryMax is the largest server Retry-After hint the leaf retry honors: without this cap a
// "Retry-After: 120" would park each leaf for two minutes, sequentially, stalling the index call.
const embedLocalRetryMax = 2 * time.Second

// embedBatch embeds one batch; on a batch-limit 400 it splits in half and recurses (one-input batches never
// split), budget bounding the cascade's request count; sub-batches get one local retry (embedSubBatch).
func (b *openaiBase) embedBatch(ctx context.Context, text []string, budget *embedRequestBudget) ([][]float32, error) {
	payload := map[string]any{
		"model": b.embeddingModel,
		"input": text,
	}
	// A refusal already learned (by an earlier Embed, or by the parent batch of a split cascade) is not paid again.
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
		// Classified like a chat failure so a 401 reads as an invalid key and a 429 is retried by the wrapper chain.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if _, sent := payload["dimensions"]; sent && rejectsEmbeddingDimensions(resp.StatusCode, errBody) {
			// Deleting the key makes this branch unreachable next time.
			delete(payload, "dimensions")
			b.rememberDimensionsRefusal()
			recordRouteFallback(b.providerID, fallbackDimensionsDropped)
			logger.FromContext(ctx).Debug("embedding model rejected `dimensions` — retrying at its native width",
				"provider", b.providerLabel, "model", b.embeddingModel)
			continue
		}
		if resp.StatusCode == http.StatusBadRequest && len(text) > 1 && embedBatchLimitRejected(errBody) {
			// Both halves need a request each; without the room the rejection surfaces instead of splitting.
			if budget.remaining < 2 {
				return nil, &embedTerminalError{cause: fmt.Errorf("%s embeddings: %w", b.providerLabel, b.handleChatError(resp, errBody))}
			}
			recordRouteFallback(b.providerID, fallbackEmbedSplit)
			logger.FromContext(ctx).Debug("embedding batch exceeded the resource's input cap — splitting",
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

func (b *embedRequestBudget) take() bool {
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}

// embedSubBatch runs one half of a split with one local retry, honoring the server's Retry-After hints only up
// to embedLocalRetryMax; the final error is terminal so the outer wrapper never multiplies the request count.
func (b *openaiBase) embedSubBatch(ctx context.Context, text []string, budget *embedRequestBudget) ([][]float32, error) {
	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			if budget.remaining < 1 {
				return nil, &embedTerminalError{cause: lastErr}
			}
			// A hint beyond embedLocalRetryMax says the provider wants this leaf parked: fail it terminally.
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
	return doProviderRequest(b.client, httpReq)
}

// maxJSONBodyBytes caps a 200 JSON body before decoding: an unbounded decode let a hostile or broken server buffer arbitrary memory.
const maxJSONBodyBytes = 32 << 20

// decodeEmbeddings reads a 200 /embeddings response (and closes its body) for want inputs. Items are placed by
// their `index` when every item carries one; a response without indices is read positionally. Count must match.
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
