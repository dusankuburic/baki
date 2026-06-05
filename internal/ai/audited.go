package ai

import (
	"context"
	"time"

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
		p.record(ctx, req.Model, resp.TokensIn, resp.TokensOut)
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
			p.record(ctx, req.Model, tokensIn, tokensOut)
		}
		onChunk(c)
	}

	return p.inner.Stream(ctx, req, wrappedOnChunk)
}

func (p *auditedProvider) record(ctx context.Context, modelID string, tokensIn, tokensOut int) {
	if p.recorder == nil || (tokensIn == 0 && tokensOut == 0) {
		return
	}

	// Calculate cost
	var inputCost, outputCost float64
	for _, m := range p.inner.Models() {
		if m.ID == modelID {
			inputCost = (float64(tokensIn) / 1000000.0) * m.Pricing.InputCostPerM
			outputCost = (float64(tokensOut) / 1000000.0) * m.Pricing.OutputCostPerM
			break
		}
	}

	metric := interfaces.UsageMetric{
		ID:       uuid.NewString(),
		UserID:   p.scope,
		// OrgID is not available at provider-construction time (the factory is
		// keyed by user scope, not org); usage is attributed per user. Threading
		// the org through would let GetDailyUsage enforce org-level budgets too.
		OrgID:            "",
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