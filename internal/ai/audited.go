package ai

import (
	"context"
	"time"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/storage/interfaces"

	"github.com/google/uuid"
)

// UsageRecorder represents a function capable of saving usage metrics.
type UsageRecorder func(ctx context.Context, metric *interfaces.UsageMetric) error

type auditedProvider struct {
	inner      Provider
	recorder   UsageRecorder
	scope      string // the user ID (key scope)
	providerID string // e.g. "openai", "claude"
}

// NewAuditedProvider wraps an existing provider and intercepts Chat and Stream
// calls to record cost metrics when they finish. scope is the caller's key
// scope (user ID); providerID identifies which provider generated the usage.
func NewAuditedProvider(inner Provider, recorder UsageRecorder, scope, providerID string) Provider {
	return &auditedProvider{
		inner:      inner,
		recorder:   recorder,
		scope:      scope,
		providerID: providerID,
	}
}

func (p *auditedProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	resp, err := p.inner.Chat(ctx, req)
	if err == nil && resp != nil {
		p.record(ctx, req.Model, req.OrgID, resp.TokensIn, resp.TokensOut)
	}
	return resp, err
}

func (p *auditedProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	var tokensIn, tokensOut int

	wrappedOnChunk := func(c Chunk) {
		if c.TokensIn > 0 {
			tokensIn = c.TokensIn
		}
		if c.TokensOut > 0 {
			tokensOut = c.TokensOut
		}
		if c.Done {
			p.record(ctx, req.Model, req.OrgID, tokensIn, tokensOut)
		}
		onChunk(c)
	}

	return p.inner.Stream(ctx, req, wrappedOnChunk)
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
	for _, m := range p.inner.Models() {
		if m.ID == modelID {
			pricing, found = m.Pricing, true
			break
		}
	}
	if !found {
		pricing = p.inner.PricePerMillionTokens()
		logger.Warn("AI usage priced from provider default — model not in catalog",
			"provider", p.providerID, "model", modelID)
	}
	inputCost := (float64(tokensIn) / 1000000.0) * pricing.InputCostPerM
	outputCost := (float64(tokensOut) / 1000000.0) * pricing.OutputCostPerM

	metric := interfaces.UsageMetric{
		ID:   uuid.NewString(),
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

	// Asynchronously record the usage so it doesn't block the caller
	go func() {
		// Use a detached context because the request context might be canceled
		_ = p.recorder(context.Background(), &metric)
	}()
}

// Delegate everything else to the inner provider
func (p *auditedProvider) SupportsTools() bool     { return p.inner.SupportsTools() }
func (p *auditedProvider) ID() string              { return p.inner.ID() }
func (p *auditedProvider) Name() string            { return p.inner.Name() }
func (p *auditedProvider) ContextLimit() int       { return p.inner.ContextLimit() }
func (p *auditedProvider) PricePerMillionTokens() Pricing { return p.inner.PricePerMillionTokens() }
func (p *auditedProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return p.inner.Embed(ctx, text)
}
func (p *auditedProvider) EstimateTokens(t string) int { return p.inner.EstimateTokens(t) }
func (p *auditedProvider) Models() []ModelInfo     { return p.inner.Models() }
func (p *auditedProvider) DefaultModel() string    { return p.inner.DefaultModel() }
func (p *auditedProvider) FreeModel() string       { return p.inner.FreeModel() }