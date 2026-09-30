package ai

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"

	"github.com/google/uuid"
)

// UsageRecorder represents a function capable of saving usage metrics.
type UsageRecorder func(ctx context.Context, metric *interfaces.UsageMetric) error

type auditedProvider struct {
	inner      Provider
	recorder   UsageRecorder
	scope      string
	providerID string
	recordSem  chan struct{}
	// countTimeout overrides countTokensTimeout when non-zero (tests: proving
	// the bound on a hung count endpoint need not take the full 5 s).
	countTimeout time.Duration
	// memoScope keys the served-model memo: the user scope, plus — for alias
	// providers whose raw provider names its resource (servedMemoResourcer) —
	// the resource, so a new resource's same-named deployment never inherits
	// the old resource's mapping. UsageMetric.UserID stays scope.
	memoScope string
	// usageOrg is the organisation whose SHARED credential this chain runs
	// on ("" = the caller's own; set by ProviderFactory.wrapChainScoped). A
	// usage row without an org of its own — an embedding (Embed carries no
	// Request), the admin's connection test — is attributed to it, so every
	// call on an org's credential is capped by that org's daily budget.
	usageOrg string
	// embedNamer names the embedding model for usage records. The resilience
	// and tracing wrappers hide the raw provider's EmbeddingModel method, so
	// the factory hands it over for alias providers (adoptRawHints); nil falls
	// back to a type assertion on inner (direct wrapping) and DefaultModel.
	embedNamer embedModelNamer
	// tokenCounter is the RAW provider's exact prompt-token count capability
	// (TokenCounter), adopted before the resilience and tracing wrappers hid
	// optional methods; nil when the provider cannot count, and CountTokens
	// answers an error the caller falls back from (to the estimate).
	tokenCounter TokenCounter
	// registry is the RAW provider's model registry (registryModelSource —
	// an Azure AI Foundry resource built by NewAzureFoundryConnectionProvider),
	// adopted before the wrappers hid it: Models lists the entries the live
	// listing lacks and applies their overrides. nil for every other provider.
	registry []registryModel
	// modelsOnce memoises the inner provider's model list for this instance's
	// lifetime. The factory builds a fresh audited chain per request, and one
	// request can consult Models() up to ~8 times (pricing per tool-loop
	// iteration, ContextLimitFor, ModelMaxOutputTokens) — for providers whose
	// Models() dials the wire (GitHub Models) that was a live HTTP GET per
	// usage record. The first call's ctx/error are cached; within a single
	// request that's the right trade (a failed first lookup belongs to a dying
	// request anyway).
	modelsOnce sync.Once
	models     []ModelInfo
	modelsErr  error
}

// NewAuditedProvider wraps an existing provider and intercepts Chat and Stream
// calls to record cost metrics when they finish. scope is the caller's key
// scope (user ID); providerID identifies which provider generated the usage.
const maxConcurrentRecords = 16

func NewAuditedProvider(inner Provider, recorder UsageRecorder, scope, providerID string) Provider {
	return newAuditedProvider(inner, recorder, scope, providerID)
}

// usage is one call's token usage as the audited wrapper records it: In/Out
// are the totals (Response.TokensIn/TokensOut, the usage chunks), CacheRead/
// CacheWrite the cached-input slice INCLUDED in In (see Response). The totals
// drive the meter, the in-flight ledger and the usage row; the cache slice is
// priced at the cached rates in usageMetric.
type usage struct {
	In, Out, CacheRead, CacheWrite int
}

// clamped bounds a call's usage before it is priced. The counts are whatever
// the upstream reported (an operator-allow-listed non-Microsoft host can
// answer anything): a negative total or cached slice would record a negative
// cost and lower the caller's daily spend, and a cached slice beyond the
// total it is part of would price tokens that were never sent. CacheRead is
// kept within [0, In], CacheWrite within [0, In-CacheRead].
func (u usage) clamped() usage {
	u.In, u.Out = max(u.In, 0), max(u.Out, 0)
	u.CacheRead = min(max(u.CacheRead, 0), u.In)
	u.CacheWrite = min(max(u.CacheWrite, 0), u.In-u.CacheRead)
	return u
}

func newAuditedProvider(inner Provider, recorder UsageRecorder, scope, providerID string) *auditedProvider {
	return &auditedProvider{
		inner:      inner,
		recorder:   recorder,
		scope:      scope,
		providerID: providerID,
		recordSem:  make(chan struct{}, maxConcurrentRecords),
		memoScope:  scope,
	}
}

// servedMemoResourcer is implemented by alias providers whose deployment
// names are only unique within one resource (Azure AI Foundry: the endpoint).
type servedMemoResourcer interface {
	servedMemoResource() string
}

// adoptRawHints reads, from the UNWRAPPED provider (the circuit-breaker,
// retry and tracing wrappers expose only the Provider interface), what the
// audited wrapper of an alias provider needs: the resource the served-model
// memo is scoped to and the embedding model name usage is priced by. Every
// other provider is left exactly as NewAuditedProvider built it.
func (p *auditedProvider) adoptRawHints(raw Provider) {
	if tc, ok := raw.(TokenCounter); ok {
		p.tokenCounter = tc
	}
	if !aliasModelProviders[p.providerID] {
		return
	}
	if r, ok := raw.(servedMemoResourcer); ok {
		if res := r.servedMemoResource(); res != "" {
			// memoScope, not scope: a provider built from an org's shared
			// credential memoises under OrgScope (factory.wrapChainScoped).
			p.memoScope = p.memoScope + "\x00" + res
		}
	}
	if n, ok := raw.(embedModelNamer); ok {
		p.embedNamer = n
	}
	if r, ok := raw.(registryModelSource); ok {
		p.registry = r.registryModels()
	}
}

func (p *auditedProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	resp, err := p.inner.Chat(ctx, req)
	metrics.ObserveAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil {
		metrics.RecordAIError(p.providerID)
		// A context-length rejection that states the window sizes the next
		// request to this deployment (alias providers; see learned_window.go).
		learnContextWindow(p.memoScope, p.providerID, req.Model, err)
		// A failed non-streamed call can still have been billed by the
		// provider (a content-filter finish, an agent run stopped for an MCP
		// approval or a client-executed tool): the provider attaches the
		// usage to the error (heldUsageError), as the stream path does.
		if in, out, served, ok := usageOf(err); ok && (in > 0 || out > 0) {
			recordServedModel(p.memoScope, p.providerID, req.Model, served)
			metrics.RecordAITokens(p.providerID, in, out)
			cr, cw := cacheUsageOf(err)
			p.record(ctx, req.Model, served, req.OrgID, usage{In: in, Out: out, CacheRead: cr, CacheWrite: cw})
		}
		return resp, err
	}
	if resp != nil {
		metrics.RecordAITokens(p.providerID, resp.TokensIn, resp.TokensOut)
		recordServedModel(p.memoScope, p.providerID, req.Model, resp.ServedModel)
		p.record(ctx, req.Model, resp.ServedModel, req.OrgID, usage{
			In:         resp.TokensIn,
			Out:        resp.TokensOut,
			CacheRead:  resp.CacheReadTokens,
			CacheWrite: resp.CacheWriteTokens,
		})
	}
	return resp, err
}

func (p *auditedProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	start := time.Now()
	var tokensIn, tokensOut int
	var cacheRead, cacheWrite int
	// sawAny: any upstream chunk — a keepalive proves a 200 body was being
	// parsed (a LocalKeepalive, synthesised while a non-streaming request
	// waits, proves nothing). sawContent: text or reasoning was delivered.
	// estOut is the
	// running token estimate of that content, counted per delta instead of
	// buffering the whole stream (a long-reasoning deployment can emit tens
	// of MB that nobody sees); per-delta BPE counting over-counts slightly
	// at delta boundaries — the safe direction for a spend cap.
	var sawChunkErr, sawDone, sawAny, sawContent bool
	var estOut int
	var servedModel string

	wrappedOnChunk := func(c Chunk) {
		if !c.LocalKeepalive {
			sawAny = true
		}
		if c.TokensIn > 0 {
			tokensIn = c.TokensIn
		}
		if c.TokensOut > 0 {
			tokensOut = c.TokensOut
		}
		if c.CacheReadTokens > 0 || c.CacheWriteTokens > 0 {
			cacheRead, cacheWrite = c.CacheReadTokens, c.CacheWriteTokens
		}
		if c.ServedModel != "" {
			servedModel = c.ServedModel
		}
		if c.Error != nil {
			sawChunkErr = true
			learnContextWindow(p.memoScope, p.providerID, req.Model, c.Error)
		}
		if c.Text != "" {
			sawContent = true
			estOut += p.inner.EstimateTokens(c.Text)
		}
		// Split-off reasoning is billed output too: a stream cancelled or
		// cut while the model is still thinking must not escape the estimate
		// (empty keepalive chunks add nothing).
		if c.Reasoning != "" {
			sawContent = true
			estOut += p.inner.EstimateTokens(c.Reasoning)
		}
		if c.ToolEvent != nil && c.ToolEvent.Done {
			// Hosted steps are billed by Azure per call / session / GB-day,
			// outside the token spend the daily budget meters; count them so
			// the gap is at least visible.
			metrics.RecordAIHostedToolStep(p.providerID, c.ToolEvent.Name)
		}
		if c.Done {
			sawDone = true
			recordServedModel(p.memoScope, p.providerID, req.Model, servedModel)
			// Compat servers that ignore stream_options (older vLLM/Ollama/
			// proxies — reachable because base URLs are env-overridable) end
			// with Done(0,0): without this fallback every such stream billed
			// $0 — no usage metric, budget never trips. Same evidence gate as
			// the no-Done branch: only bill when content actually arrived.
			if tokensIn == 0 && tokensOut == 0 && (sawContent || len(c.ToolCalls) > 0) {
				tokensOut = estOut
				tokensIn = p.estimateRequestTokens(req)
			}
			metrics.RecordAITokens(p.providerID, tokensIn, tokensOut)
			p.record(ctx, req.Model, servedModel, req.OrgID, usage{In: tokensIn, Out: tokensOut, CacheRead: cacheRead, CacheWrite: cacheWrite})
		}
		onChunk(c)
	}

	err := p.inner.Stream(ctx, req, wrappedOnChunk)
	metrics.ObserveAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil || sawChunkErr {
		metrics.RecordAIError(p.providerID)
	}
	if err != nil {
		learnContextWindow(p.memoScope, p.providerID, req.Model, err)
		// The retry wrapper withholds a pre-content error chunk from the
		// chunk stream (so it can replay); when that chunk was the only place
		// the run's usage rode, the wrapper re-attaches it to the error (see
		// heldUsageError) — pick it up here, or the tokens a failed run spent
		// would be estimated instead of recorded.
		if in, out, served, ok := usageOf(err); ok {
			if in > 0 {
				tokensIn = in
			}
			if out > 0 {
				tokensOut = out
			}
			if served != "" {
				servedModel = served
			}
			if cr, cw := cacheUsageOf(err); cr > 0 || cw > 0 {
				cacheRead, cacheWrite = cr, cw
			}
		}
	}
	// A stream that ended without a Done chunk (truncation, mid-stream error,
	// cancel) still consumed provider tokens; usage arrives on the Done chunk.
	// Record whatever the provider reported, falling back to estimates, so the
	// cost still counts against the daily budget. Gated on evidence that the
	// request actually reached the provider — any upstream chunk, including
	// the keepalives Azure's prompt-filter annotation and role delta arrive as
	// (a Stop during a reasoning deployment's silent thinking phase is billed
	// the prompt and, by Azure, the reasoning), or an upstream error event —
	// so failures before the request was accepted (circuit open, connection
	// refused: no chunk at all) aren't billed an input-token estimate for a
	// call that consumed nothing.
	if !sawDone && (sawAny || sawContent || tokensIn > 0 || tokensOut > 0 || sawChunkErr) {
		if tokensOut == 0 {
			tokensOut = estOut
		}
		if tokensIn == 0 {
			tokensIn = p.estimateRequestTokens(req)
		}
		// The model that answered rides on every content chunk, so a stream
		// cut before Done still teaches the memo and records that model (Azure
		// is unpriced, so the row carries the tokens at $0).
		recordServedModel(p.memoScope, p.providerID, req.Model, servedModel)
		metrics.RecordAITokens(p.providerID, tokensIn, tokensOut)
		p.record(ctx, req.Model, servedModel, req.OrgID, usage{In: tokensIn, Out: tokensOut, CacheRead: cacheRead, CacheWrite: cacheWrite})
	}
	return err
}

// estimateRequestTokens estimates a full request's input tokens: system
// prompt + message contents + per-message tool_use argument JSON. Shared by
// the no-Done and Done-with-zero-usage fallbacks.
func (p *auditedProvider) estimateRequestTokens(req Request) int {
	total := p.inner.EstimateTokens(req.SystemPrompt)
	for _, m := range req.Messages {
		c := p.inner.EstimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			c += p.inner.EstimateTokens(string(tc.Input))
		}
		// A replayed provider record (ProviderItems) is re-sent verbatim and
		// its bytes bound the message's real context cost — take the max, not
		// the sum (audit #9 F-28).
		if len(m.ProviderItems) > 0 {
			c = max(c, p.inner.EstimateTokens(string(m.ProviderItems)))
		}
		total += c
	}
	return total
}

// detachedPricingTimeout bounds the background pricing step of
// recordDetached (a provider model listing at worst).
const detachedPricingTimeout = 10 * time.Second

func (p *auditedProvider) record(ctx context.Context, modelID, servedModel, orgID string, u usage) {
	u = u.clamped()
	if p.recorder == nil || (u.In == 0 && u.Out == 0) {
		return
	}
	metric := p.usageMetric(ctx, modelID, servedModel, orgID, u)

	// Asynchronously record the usage so it doesn't block the caller.
	//
	// The semaphore acquire is NON-blocking: when maxConcurrentRecords recorders
	// are already in flight we DROP this metric (counted via
	// metrics.RecordUsageDropped) rather than spawning another goroutine that
	// parks on the send. This bounds the goroutine count to
	// maxConcurrentRecords under any load, even with a slow DB recorder.
	//
	// In practice the drop NEVER fires: each request gets its own auditedProvider
	// (the factory builds a fresh chain per request) with a 16-slot semaphore,
	// and a single request records at most ~7 metrics (one per tool-loop
	// iteration, maxToolIterations=6 + the initial turn). 16 >> 7, so the
	// semaphore is never exhausted. The drop is a safety valve against a
	// pathological stuck recorder, not a routinely-hit path — and the per-request
	// design means a drop here never affects other tenants' budget tracking.
	if !p.acquireRecordSlot(modelID) {
		return
	}
	// #nosec G118 -- usage accounting must survive the request that generated it.
	// Binding this to the caller's context would drop the spend record for exactly
	// the turns a user cancels, under-counting the daily budget the chat path then
	// enforces. The semaphore above bounds concurrency instead.
	go func() {
		defer p.releaseRecordSlot(modelID)
		p.save(&metric)
	}()
}

// recordDetached is record for callers on a short deadline — the per-turn RAG
// query embedding runs under an 800 ms budget. Pricing may need the
// provider's live model listing (Azure AI Foundry: several serial requests on
// a cache miss), so pricing AND saving run in the same bounded background slot
// on a context detached from the caller's cancellation (values kept) with its
// own timeout: the caller's deadline is never spent on a listing, and a
// listing is never cut short (and cached truncated) by the caller's deadline.
func (p *auditedProvider) recordDetached(ctx context.Context, modelID, servedModel, orgID string, u usage) {
	u = u.clamped()
	if p.recorder == nil || (u.In == 0 && u.Out == 0) {
		return
	}
	if !p.acquireRecordSlot(modelID) {
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedPricingTimeout)
	go func() {
		defer cancel()
		defer p.releaseRecordSlot(modelID)
		metric := p.usageMetric(pctx, modelID, servedModel, orgID, u)
		p.save(&metric)
	}()
}

// acquireRecordSlot takes one of the maxConcurrentRecords background slots
// without blocking; false (metric dropped and counted) when all are busy.
func (p *auditedProvider) acquireRecordSlot(modelID string) bool {
	select {
	case p.recordSem <- struct{}{}:
		return true
	default:
		metrics.RecordUsageDropped("saturated")
		logger.Warn("AI usage metric dropped — recorder saturated",
			"provider", p.providerID, "model", modelID)
		return false
	}
}

// releaseRecordSlot is deferred by every background record goroutine: it
// frees the slot and keeps a panicking recorder (or pricing step) from taking
// the process down.
func (p *auditedProvider) releaseRecordSlot(modelID string) {
	<-p.recordSem
	if r := recover(); r != nil {
		logger.Warn("AI usage recording panicked",
			"provider", p.providerID, "model", modelID, "panic", r)
	}
}

// save writes one usage row on its own bounded context.
func (p *auditedProvider) save(metric *interfaces.UsageMetric) {
	// #nosec G118 -- intentionally detached: usage logging must outlive the request ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.recorder(ctx, metric); err != nil {
		logger.Warn("AI usage recording failed",
			"provider", p.providerID, "model", metric.Model, "error", err)
	}
}

// usageMetric prices one call's tokens and builds its usage row. If the model
// has no known price (e.g. a newly released ID the hardcoded catalogue hasn't
// caught up to) it falls back to the provider-wide PricePerMillionTokens so
// usage is still priced — recording $0 would silently let the daily budget
// never trip for that model. The fallback is counted by the pricing-fallback
// metric and logged so the catalogue gap is visible.
func (p *auditedProvider) usageMetric(ctx context.Context, modelID, servedModel, orgID string, u usage) interfaces.UsageMetric {
	pricing, found := p.pricingFor(ctx, modelID, servedModel)
	if !found {
		pricing = p.inner.PricePerMillionTokens()
		metrics.RecordPricingFallback(p.providerID, pricingMetricModel(p.providerID, modelID))
		logger.Warn("AI usage priced from provider default — model not in catalog",
			"provider", p.providerID, "model", modelID, "servedModel", servedModel)
	}
	// Cached input tokens are billed at their own rates — Claude reads at
	// 0.025-0.1× the input rate by family and 5-minute cache writes at 1.25×
	// (cacheReadMultiplier; every other route keeps 1.0, and Azure AI Foundry
	// is unpriced) — so price the cached slice of u.In at those rates instead
	// of the full input rate, which overstated a cached Claude session up to
	// 40× against the daily budget (and understated the writes). u.In stays
	// the TOTAL the meter and the usage row record.
	uncached := max(u.In-u.CacheRead-u.CacheWrite, 0)
	readMul := cacheReadMultiplier(p.providerID, modelID, servedModel)
	inputCost := (float64(uncached) + readMul*float64(u.CacheRead) + cacheWriteMultiplier*float64(u.CacheWrite)) / 1000000.0 * pricing.InputCostPerM
	outputCost := (float64(u.Out) / 1000000.0) * pricing.OutputCostPerM

	if orgID == "" {
		// A chain built from an org's shared credential bills the org even
		// when the call carried no org (embeddings, the connection test).
		orgID = p.usageOrg
	}

	metric := interfaces.UsageMetric{
		ID:     uuid.NewString(),
		UserID: p.scope,
		// OrgID is carried on the request (set by the caller from the flow's
		// OrganizationID) rather than fixed at construction time, so the same
		// audited provider can attribute org-scoped and personal usage correctly.
		// This is what lets GetDailyUsage enforce org-wide daily budgets.
		OrgID:    orgID,
		Provider: p.providerID,
		Model:    modelID,
		// What answered, next to what was asked for (see usageServedModel).
		ServedModel:      usageServedModel(p.providerID, modelID, servedModel),
		PromptTokens:     u.In,
		CompletionTokens: u.Out,
		EstimatedCost:    inputCost + outputCost,
		CreatedAt:        time.Now(),
	}
	// Deployment-wide spend only. Per-tenant attribution is the UsageMetric row
	// built just above — labelling the metric by user+org minted one permanent
	// Prometheus series per user and would eventually take the whole scrape down
	// with it (see the metric's definition).
	metrics.RecordAIUsageCost(inputCost + outputCost)
	return metric
}

// maxUsageServedModel caps the served model stored on a usage row: it is
// whatever the upstream's "model" field said (an operator-allow-listed host
// can answer anything), and the column feeds analytics grouping.
const maxUsageServedModel = 128

// usageServedModel is the served model a usage row records next to the
// requested one, so analytics can group by the billed model. Only alias
// providers (Azure AI Foundry: a deployment name, a "claude:<deployment>" id
// or an agent) record one — for every other provider the id already names
// the model, and a dated answer ("gpt-4o-2024-08-06" for "gpt-4o") only
// split analytics into two series for one model. Blank when the provider
// named none or the answer normalises to the requested id
// (normalizeServedModel: case, whitespace, version date); capped at
// maxUsageServedModel bytes on a rune boundary.
func usageServedModel(providerID, modelID, servedModel string) string {
	if !UsesModelAliases(providerID) {
		return ""
	}
	served := strings.TrimSpace(servedModel)
	if served == "" || normalizeServedModel(served) == normalizeServedModel(modelID) {
		return ""
	}
	if len(served) > maxUsageServedModel {
		cut := maxUsageServedModel
		for cut > 0 && !utf8.RuneStart(served[cut]) {
			cut--
		}
		served = served[:cut]
	}
	return served
}

// ReservationPricing is the rate the chat service reserves in-flight budget
// at for model. Alias providers (Azure AI Foundry) are unpriced, so nothing
// is reserved against the budget (the inner provider's zero rate). Every
// other provider keeps its provider-wide rate.
func (p *auditedProvider) ReservationPricing(ctx context.Context, model string) Pricing {
	// Alias providers (Azure AI Foundry) carry no price table: their cost is
	// read in the Azure portal, so nothing is reserved against the budget.
	return p.inner.PricePerMillionTokens()
}

// UsagePricing is the rate a finished turn on model (answered by served) was
// billed at — the same resolution as usageMetric, without recording — so the
// chat service can put it on the done payload. false only when nothing at all
// prices the call (a non-alias provider with no listing entry and a zero
// provider rate, e.g. GitHub Models' deliberately free listing is still found
// as $0).
func (p *auditedProvider) UsagePricing(ctx context.Context, model, served string) (Pricing, bool) {
	if pricing, ok := p.pricingFor(ctx, model, served); ok {
		return pricing, true
	}
	if pricing := p.inner.PricePerMillionTokens(); pricing != (Pricing{}) {
		return pricing, true
	}
	return Pricing{}, false
}

// pricingFor resolves the per-million rates a call is billed at; false sends
// the caller to the provider-wide fallback. p.Models (the memoised wrapper)
// keeps repeated records on one request from each hitting the inner
// provider's Models().
//
// Providers whose model ids ARE model names: the request model's entry in the
// provider's own listing, whatever its price — a live listing is
// authoritative, including GitHub Models' and Copilot's deliberately $0
// entries. The served model is never consulted for them: GitHub Models
// answers a free call as "gpt-4o-2024-11-20", which the catalogue would price
// at list rates.
//
// Alias providers (Azure AI Foundry deployments, model-router, agents) are
// unpriced: baki keeps no price table for them (cost is read in the Azure
// portal), so the result is always a zero price that counts as found — no
// fallback metric or warning.
func (p *auditedProvider) pricingFor(ctx context.Context, modelID, servedModel string) (Pricing, bool) {
	if aliasModelProviders[p.providerID] {
		return Pricing{}, true
	}
	if models, err := p.Models(ctx); err == nil {
		for _, m := range models {
			if m.ID == modelID {
				return m.Pricing, true
			}
		}
	}
	return Pricing{}, false
}

// Delegate everything else to the inner provider
func (p *auditedProvider) SupportsTools() bool            { return p.inner.SupportsTools() }
func (p *auditedProvider) ID() string                     { return p.inner.ID() }
func (p *auditedProvider) Name() string                   { return p.inner.Name() }
func (p *auditedProvider) ContextLimit() int              { return p.inner.ContextLimit() }
func (p *auditedProvider) PricePerMillionTokens() Pricing { return p.inner.PricePerMillionTokens() }

// embedModelNamer is implemented by providers that expose the model name sent
// to their /embeddings-equivalent endpoint (openaiBase family + Gemini), so
// embedding usage is priced against the model actually used.
type embedModelNamer interface {
	EmbeddingModel() string
}

func (p *auditedProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	start := time.Now()
	res, err := p.inner.Embed(ctx, text)
	metrics.ObserveAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil {
		metrics.RecordAIError(p.providerID)
		return res, err
	}
	// Embedding spend hit NO usage record before: RAG indexing (up to 500
	// chunks/document) and every per-turn query embedding bypassed the daily
	// budget entirely. Embedding APIs bill per INPUT token (no output), so
	// estimate the batch's input tokens and record with the embedding model's
	// name for pricing (falling back to provider-wide pricing when unknown).
	// Embed carries no Request, so no org rides on the call: embedding spend
	// on the caller's own credential attributes to the user (personal
	// budget), never an org the caller may not be in; on an org's shared
	// credential it is attributed to that org (usageMetric, usageOrg).
	if len(text) > 0 {
		tokensIn := 0
		for _, t := range text {
			tokensIn += p.inner.EstimateTokens(t)
		}
		model := ""
		if p.embedNamer != nil {
			model = p.embedNamer.EmbeddingModel()
		} else if namer, ok := p.inner.(embedModelNamer); ok {
			model = namer.EmbeddingModel()
		}
		if model == "" {
			model = p.inner.DefaultModel()
		}
		metrics.RecordAITokens(p.providerID, tokensIn, 0)
		// Pricing may need the live model listing; the per-turn RAG query
		// embeds under an 800 ms budget, which must never be spent on it.
		p.recordDetached(ctx, model, "", "", usage{In: tokensIn})
	}
	return res, err
}
func (p *auditedProvider) EstimateTokens(t string) int { return p.inner.EstimateTokens(t) }

// countTokensTimeout bounds one exact prompt-token count. The count runs on
// the turn's context ahead of the model call, and the transports' header
// timeouts are sized for a long first token (Azure's is minutes): a stalled
// count endpoint must cost the turn seconds, then the estimate, not the turn.
const countTokensTimeout = 5 * time.Second

// CountTokens forwards the RAW provider's exact prompt-token count (TokenCounter,
// adopted in adoptRawHints — the resilience and tracing wrappers in between hide
// optional methods, so a type assertion on this chain's inner providers would
// never match), bounded by countTokensTimeout. An error means "no exact count
// available" (the raw provider cannot count, the endpoint refused or did not
// answer in time): callers fall back to the estimate.
func (p *auditedProvider) CountTokens(ctx context.Context, req Request) (int, error) {
	if p.tokenCounter == nil {
		return 0, fmt.Errorf("%s: no exact token counting available", p.providerID)
	}
	timeout := countTokensTimeout
	if p.countTimeout > 0 {
		timeout = p.countTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return p.tokenCounter.CountTokens(ctx, req)
}
func (p *auditedProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	p.modelsOnce.Do(func() {
		p.models, p.modelsErr = p.inner.Models(ctx)
		if p.modelsErr == nil {
			registry := p.appliedRegistry()
			// A registered model is listed whatever the live listing returned
			// (an agent beyond its cap, a failed or partial project listing),
			// so it is sized like a listed one below.
			p.models = addUnlistedRegistryModels(p.models, registry)
			// Deployment aliases (Azure AI Foundry) learn their served model,
			// price and — for a stable alias — context window from the model
			// that served them last on the same resource, and are clamped to
			// the window their own context-length rejection stated.
			p.models = enrichServedModels(p.memoScope, p.providerID, p.models)
			// The user's own sizing of a registered model (a small distilled
			// model nothing else can size) beats the catalogue, the project
			// listing and the memo — but not the model's own rejection: the
			// learned window below still clamps it.
			p.models = applyRegistryOverrides(p.models, registry)
			p.models = applyLearnedWindows(p.memoScope, p.providerID, p.models)
		}
	})
	return p.models, p.modelsErr
}

// appliedRegistry is the model registry Models applies: the one adopted from
// the raw provider (factory chains), else the inner provider's own when it
// was wrapped directly (NewAuditedProvider).
func (p *auditedProvider) appliedRegistry() []registryModel {
	if p.registry != nil {
		return p.registry
	}
	if r, ok := p.inner.(registryModelSource); ok {
		return r.registryModels()
	}
	return nil
}

func (p *auditedProvider) DefaultModel() string { return p.inner.DefaultModel() }
func (p *auditedProvider) FreeModel() string    { return p.inner.FreeModel() }
