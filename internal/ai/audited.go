package ai

import (
	"context"
	"strings"
	"sync"
	"time"

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
	return &auditedProvider{
		inner:      inner,
		recorder:   recorder,
		scope:      scope,
		providerID: providerID,
		recordSem:  make(chan struct{}, maxConcurrentRecords),
	}
}

func (p *auditedProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	resp, err := p.inner.Chat(ctx, req)
	metrics.ObserveAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil {
		metrics.RecordAIError(p.providerID)
		return resp, err
	}
	if resp != nil {
		metrics.RecordAITokens(p.providerID, resp.TokensIn, resp.TokensOut)
		p.record(ctx, req.Model, req.OrgID, resp.TokensIn, resp.TokensOut)
	}
	return resp, err
}

func (p *auditedProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	start := time.Now()
	var tokensIn, tokensOut int
	var sawChunkErr, sawDone bool
	var streamed strings.Builder

	wrappedOnChunk := func(c Chunk) {
		if c.TokensIn > 0 {
			tokensIn = c.TokensIn
		}
		if c.TokensOut > 0 {
			tokensOut = c.TokensOut
		}
		if c.Error != nil {
			sawChunkErr = true
		}
		streamed.WriteString(c.Text)
		if c.Done {
			sawDone = true
			// Compat servers that ignore stream_options (older vLLM/Ollama/
			// proxies — reachable because base URLs are env-overridable) end
			// with Done(0,0): without this fallback every such stream billed
			// $0 — no usage metric, budget never trips. Same evidence gate as
			// the no-Done branch: only bill when content actually arrived.
			if tokensIn == 0 && tokensOut == 0 && (streamed.Len() > 0 || len(c.ToolCalls) > 0) {
				tokensOut = p.inner.EstimateTokens(streamed.String())
				tokensIn = p.estimateRequestTokens(req)
			}
			metrics.RecordAITokens(p.providerID, tokensIn, tokensOut)
			p.record(ctx, req.Model, req.OrgID, tokensIn, tokensOut)
		}
		onChunk(c)
	}

	err := p.inner.Stream(ctx, req, wrappedOnChunk)
	metrics.ObserveAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil || sawChunkErr {
		metrics.RecordAIError(p.providerID)
	}
	// A stream that ended without a Done chunk (truncation, mid-stream error,
	// cancel) still consumed provider tokens; usage arrives on the Done chunk.
	// Record whatever the provider reported, falling back to estimates, so the
	// cost still counts against the daily budget. Gated on evidence that the
	// request actually reached the provider (some output or an upstream error
	// event) so failures before the request was accepted — circuit open,
	// connection refused — aren't billed an input-token estimate for a call
	// that consumed nothing.
	if !sawDone && (streamed.Len() > 0 || tokensIn > 0 || tokensOut > 0 || sawChunkErr) {
		if tokensOut == 0 {
			tokensOut = p.inner.EstimateTokens(streamed.String())
		}
		if tokensIn == 0 {
			tokensIn = p.estimateRequestTokens(req)
		}
		metrics.RecordAITokens(p.providerID, tokensIn, tokensOut)
		p.record(ctx, req.Model, req.OrgID, tokensIn, tokensOut)
	}
	return err
}

// estimateRequestTokens estimates a full request's input tokens: system
// prompt + message contents + per-message tool_use argument JSON. Shared by
// the no-Done and Done-with-zero-usage fallbacks.
func (p *auditedProvider) estimateRequestTokens(req Request) int {
	total := p.inner.EstimateTokens(req.SystemPrompt)
	for _, m := range req.Messages {
		total += p.inner.EstimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			total += p.inner.EstimateTokens(string(tc.Input))
		}
	}
	return total
}

func (p *auditedProvider) record(ctx context.Context, modelID, orgID string, tokensIn, tokensOut int) {
	if p.recorder == nil || (tokensIn == 0 && tokensOut == 0) {
		return
	}

	// Calculate cost from the model's catalog pricing. If the model isn't in the
	// catalog (e.g. a newly released ID the hardcoded list hasn't caught up to),
	// fall back to the provider-wide PricePerMillionTokens so usage is still
	// priced — recording $0 would silently let the daily budget never trip for
	// that model. The fallback is logged so the catalog gap is visible.
	// p.Models (the memoised wrapper) keeps repeated records on one request
	// from each hitting the inner provider's Models().
	pricing, found := Pricing{}, false
	models, err := p.Models(ctx)
	if err == nil {
		for _, m := range models {
			if m.ID == modelID {
				pricing, found = m.Pricing, true
				break
			}
		}
	}
	if !found {
		pricing = p.inner.PricePerMillionTokens()
		metrics.RecordPricingFallback(p.providerID, modelID)
		logger.Warn("AI usage priced from provider default — model not in catalog",
			"provider", p.providerID, "model", modelID)
	}
	inputCost := (float64(tokensIn) / 1000000.0) * pricing.InputCostPerM
	outputCost := (float64(tokensOut) / 1000000.0) * pricing.OutputCostPerM

	metric := interfaces.UsageMetric{
		ID:     uuid.NewString(),
		UserID: p.scope,
		// OrgID is carried on the request (set by the caller from the flow's
		// OrganizationID) rather than fixed at construction time, so the same
		// audited provider can attribute org-scoped and personal usage correctly.
		// This is what lets GetDailyUsage enforce org-wide daily budgets.
		OrgID:            orgID,
		Provider:         p.providerID,
		Model:            modelID,
		PromptTokens:     tokensIn,
		CompletionTokens: tokensOut,
		EstimatedCost:    inputCost + outputCost,
		CreatedAt:        time.Now(),
	}
	// Attribute the spend to the caller's user+org so on-call can see per-tenant
	// cost without querying the usage_metrics table.
	metrics.RecordAIUsageCost(p.scope, orgID, inputCost+outputCost)

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
	// #nosec G118 -- intentionally detached: usage logging must outlive the request ctx.
	select {
	case p.recordSem <- struct{}{}:
	default:
		metrics.RecordUsageDropped("saturated")
		logger.Warn("AI usage metric dropped — recorder saturated",
			"provider", p.providerID, "model", modelID)
		return
	}
	go func() {
		defer func() {
			<-p.recordSem
			if r := recover(); r != nil {
				logger.Warn("AI usage recording panicked",
					"provider", p.providerID, "model", modelID, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.recorder(ctx, &metric); err != nil {
			logger.Warn("AI usage recording failed",
				"provider", p.providerID, "model", modelID, "error", err)
		}
	}()
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
	// Embed carries no Request, so OrgID is empty: embedding spend attributes
	// to the user (personal budget), never an org the caller may not be in.
	if len(text) > 0 {
		tokensIn := 0
		for _, t := range text {
			tokensIn += p.inner.EstimateTokens(t)
		}
		model := ""
		if namer, ok := p.inner.(embedModelNamer); ok {
			model = namer.EmbeddingModel()
		}
		if model == "" {
			model = p.inner.DefaultModel()
		}
		metrics.RecordAITokens(p.providerID, tokensIn, 0)
		p.record(ctx, model, "", tokensIn, 0)
	}
	return res, err
}
func (p *auditedProvider) EstimateTokens(t string) int { return p.inner.EstimateTokens(t) }
func (p *auditedProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	p.modelsOnce.Do(func() {
		p.models, p.modelsErr = p.inner.Models(ctx)
	})
	return p.models, p.modelsErr
}
func (p *auditedProvider) DefaultModel() string { return p.inner.DefaultModel() }
func (p *auditedProvider) FreeModel() string    { return p.inner.FreeModel() }
