package ai

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracedProvider wraps an AI Provider with OpenTelemetry tracing. A span says how the call ended: the status, a
// closed error class, the served model, the usage and the finish reason — for an error chunk the stream
// delivered as well as for a returned error (the SSE parsers report an upstream failure as Chunk{Error} and
// return nil), and with the usage the retry wrapper carried on a held-back error chunk (usageOf).
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

// errorClass is a closed name for err's provider sentinel, for the ai.error_class span attribute.
func errorClass(err error) string {
	for _, c := range []struct {
		sentinel error
		class    string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline"},
		{ErrContentFiltered, "content_filtered"},
		{ErrCircuitOpen, "circuit_open"},
		{ErrRateLimited, "rate_limited"},
		{ErrInsufficientBalance, "balance"},
		{ErrProviderDown, "provider_down"},
		{ErrContextLimit, "context_limit"},
		{ErrOutputLimit, "output_limit"},
		{ErrThoughtSignatureMissing, "thought_signature_missing"},
		{ErrToolsUnsupported, "tools_unsupported"},
		{ErrProviderAccessDenied, "access_denied"},
		{ErrProviderPermissionDenied, "permission_denied"},
		{ErrApiKeyInvalid, "auth"},
		{ErrKeyNotConfigured, "not_configured"},
		{ErrProviderConfigInvalid, "config_invalid"},
	} {
		if errors.Is(err, c.sentinel) {
			return c.class
		}
	}
	return "other"
}

// endFailed marks span failed with err: an exception event, the Error status and the error class. A caller's own
// cancel (a user's Stop) keeps its class but no Error status: it is not a provider failure.
func endFailed(span trace.Span, err error) {
	span.SetAttributes(attribute.String("ai.error_class", errorClass(err)))
	if errors.Is(err, context.Canceled) {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func (tp *TracedProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	ctx, span := tp.tracer.Start(ctx, fmt.Sprintf("%s.Chat", tp.ID()), trace.WithAttributes(
		attribute.String("ai.provider", tp.ID()),
		attribute.String("ai.model", req.Model),
	))
	defer span.End()

	resp, err := tp.Provider.Chat(ctx, req)
	if err != nil {
		if in, out, served, ok := usageOf(err); ok {
			span.SetAttributes(attribute.Int("ai.tokens_in", in), attribute.Int("ai.tokens_out", out))
			if served != "" {
				span.SetAttributes(attribute.String("ai.served_model", served))
			}
		}
		endFailed(span, err)
		return nil, err
	}

	span.SetAttributes(
		attribute.Int("ai.tokens_in", resp.TokensIn),
		attribute.Int("ai.tokens_out", resp.TokensOut),
		attribute.Int("ai.cache_read_tokens", resp.CacheReadTokens),
		attribute.Int("ai.cache_write_tokens", resp.CacheWriteTokens),
		attribute.String("ai.finish_reason", resp.FinishReason),
	)
	if resp.ServedModel != "" {
		span.SetAttributes(attribute.String("ai.served_model", resp.ServedModel))
	}
	return resp, nil
}

func (tp *TracedProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	ctx, span := tp.tracer.Start(ctx, fmt.Sprintf("%s.Embed", tp.ID()), trace.WithAttributes(
		attribute.String("ai.provider", tp.ID()),
		attribute.Int("ai.inputs", len(text)),
	))
	defer span.End()

	res, err := tp.Provider.Embed(ctx, text)
	if err != nil {
		endFailed(span, err)
		return nil, err
	}
	return res, nil
}

// Models spans the provider's model listing (a live listing is a network call).
func (tp *TracedProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	ctx, span := tp.tracer.Start(ctx, fmt.Sprintf("%s.Models", tp.ID()), trace.WithAttributes(
		attribute.String("ai.provider", tp.ID()),
	))
	defer span.End()
	models, err := tp.Provider.Models(ctx)
	if err != nil {
		endFailed(span, err)
		return models, err
	}
	span.SetAttributes(attribute.Int("ai.models", len(models)))
	return models, nil
}

func (tp *TracedProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	ctx, span := tp.tracer.Start(ctx, fmt.Sprintf("%s.Stream", tp.ID()), trace.WithAttributes(
		attribute.String("ai.provider", tp.ID()),
		attribute.String("ai.model", req.Model),
	))
	defer span.End()

	var tokensIn, tokensOut, cacheRead, cacheWrite int
	var servedModel, finishReason string
	var chunkErr error

	err := tp.Provider.Stream(ctx, req, func(c Chunk) {
		if c.attemptDiscarded {
			// A retried attempt's usage: recorded by the audited wrapper, not this span's last attempt.
			span.AddEvent("ai.attempt_discarded", trace.WithAttributes(
				attribute.Int("ai.tokens_in", c.TokensIn), attribute.Int("ai.tokens_out", c.TokensOut)))
			tokensIn, tokensOut, cacheRead, cacheWrite = 0, 0, 0, 0
			onChunk(c)
			return
		}
		if c.TokensIn > 0 {
			tokensIn = c.TokensIn
		}
		if c.TokensOut > 0 {
			tokensOut = c.TokensOut
		}
		if c.CacheReadTokens > 0 {
			cacheRead = c.CacheReadTokens
		}
		if c.CacheWriteTokens > 0 {
			cacheWrite = c.CacheWriteTokens
		}
		if c.ServedModel != "" {
			servedModel = c.ServedModel
		}
		if c.FinishReason != "" {
			finishReason = c.FinishReason
		}
		if c.Error != nil {
			chunkErr = c.Error
		}
		onChunk(c)
	})

	// A held-back error chunk's usage rides on the returned error (the retry wrapper's heldUsageError).
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
	}
	span.SetAttributes(
		attribute.Int("ai.tokens_in", tokensIn),
		attribute.Int("ai.tokens_out", tokensOut),
		attribute.Int("ai.cache_read_tokens", cacheRead),
		attribute.Int("ai.cache_write_tokens", cacheWrite),
	)
	if servedModel != "" {
		span.SetAttributes(attribute.String("ai.served_model", servedModel))
	}
	if finishReason != "" {
		span.SetAttributes(attribute.String("ai.finish_reason", finishReason))
	}
	if failure := cmp.Or(err, chunkErr); failure != nil {
		endFailed(span, failure)
	}
	return err
}
