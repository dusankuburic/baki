package ai

import (
	"context"
	"strings"
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
			tokensIn = p.inner.EstimateTokens(req.SystemPrompt)
			for _, m := range req.Messages {
				tokensIn += p.inner.EstimateTokens(m.Content)
			}
		}
		metrics.RecordAITokens(p.providerID, tokensIn, tokensOut)
		p.record(ctx, req.Model, req.OrgID, tokensIn, tokensOut)
	}
	return err
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
	pricing, found := Pricing{}, false
	models, err := p.inner.Models(ctx)
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
func (p *auditedProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return p.inner.Embed(ctx, text)
}
func (p *auditedProvider) EstimateTokens(t string) int { return p.inner.EstimateTokens(t) }
func (p *auditedProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	return p.inner.Models(ctx)
}
func (p *auditedProvider) DefaultModel() string { return p.inner.DefaultModel() }
func (p *auditedProvider) FreeModel() string    { return p.inner.FreeModel() }
