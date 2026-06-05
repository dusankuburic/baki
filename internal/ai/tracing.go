package ai

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TracedProvider wraps an AI Provider with OpenTelemetry tracing.
type TracedProvider struct {
	Provider
	tracer trace.Tracer
}

func NewTracedProvider(p Provider) *TracedProvider {
	return &TracedProvider{
		Provider: p,
		tracer:   otel.Tracer("ai-provider"),
	}
}

func (tp *TracedProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	ctx, span := tp.tracer.Start(ctx, fmt.Sprintf("%s.Chat", tp.ID()), trace.WithAttributes(
		attribute.String("ai.provider", tp.ID()),
		attribute.String("ai.model", req.Model),
	))
	defer span.End()

	resp, err := tp.Provider.Chat(ctx, req)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	span.SetAttributes(
		attribute.Int("ai.tokens_in", resp.TokensIn),
		attribute.Int("ai.tokens_out", resp.TokensOut),
		attribute.String("ai.finish_reason", resp.FinishReason),
	)

	return resp, nil
}

func (tp *TracedProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	ctx, span := tp.tracer.Start(ctx, fmt.Sprintf("%s.Embed", tp.ID()), trace.WithAttributes(
		attribute.String("ai.provider", tp.ID()),
	))
	defer span.End()

	res, err := tp.Provider.Embed(ctx, text)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	return res, nil
}

func (tp *TracedProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	ctx, span := tp.tracer.Start(ctx, fmt.Sprintf("%s.Stream", tp.ID()), trace.WithAttributes(
		attribute.String("ai.provider", tp.ID()),
		attribute.String("ai.model", req.Model),
	))
	defer span.End()

	var totalTokensIn, totalTokensOut int

	err := tp.Provider.Stream(ctx, req, func(c Chunk) {
		if c.TokensIn > 0 {
			totalTokensIn = c.TokensIn
		}
		if c.TokensOut > 0 {
			totalTokensOut = c.TokensOut
		}
		onChunk(c)
	})

	if err != nil {
		span.RecordError(err)
	}

	span.SetAttributes(
		attribute.Int("ai.tokens_in", totalTokensIn),
		attribute.Int("ai.tokens_out", totalTokensOut),
	)

	return err
}
