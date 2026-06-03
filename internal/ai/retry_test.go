package ai

import (
	"context"
	"errors"
	"testing"
)

// retryStub lets a test script a sequence of Chat/Stream outcomes.
type retryStub struct {
	Provider
	chatErrs   []error // returned in order, one per call
	chatCalls  int
	streamPlan []func(onChunk func(Chunk)) error
	streamCall int
}

func (s *retryStub) Chat(_ context.Context, _ Request) (*Response, error) {
	i := s.chatCalls
	s.chatCalls++
	if i < len(s.chatErrs) {
		if s.chatErrs[i] != nil {
			return nil, s.chatErrs[i]
		}
	}
	return &Response{Content: "ok"}, nil
}

func (s *retryStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	i := s.streamCall
	s.streamCall++
	if i < len(s.streamPlan) {
		return s.streamPlan[i](onChunk)
	}
	return nil
}

func TestRetry_Chat_RetriesOnRateLimitThenSucceeds(t *testing.T) {
	stub := &retryStub{chatErrs: []error{ErrRateLimited, ErrRateLimited, nil}}
	rp := NewRetryingProvider(stub)

	resp, err := rp.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("unexpected content %q", resp.Content)
	}
	if stub.chatCalls != 3 {
		t.Errorf("expected 3 Chat calls (2 retries), got %d", stub.chatCalls)
	}
}

func TestRetry_Chat_DoesNotRetryPermanentError(t *testing.T) {
	stub := &retryStub{chatErrs: []error{ErrApiKeyInvalid}}
	rp := NewRetryingProvider(stub)

	_, err := rp.Chat(context.Background(), Request{})
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Fatalf("expected ErrApiKeyInvalid, got %v", err)
	}
	if stub.chatCalls != 1 {
		t.Errorf("expected exactly 1 Chat call for permanent error, got %d", stub.chatCalls)
	}
}

func TestRetry_Chat_StopsAtMaxAttempts(t *testing.T) {
	stub := &retryStub{chatErrs: []error{ErrRateLimited, ErrRateLimited, ErrRateLimited, ErrRateLimited}}
	rp := NewRetryingProvider(stub)

	_, err := rp.Chat(context.Background(), Request{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited after exhausting retries, got %v", err)
	}
	if stub.chatCalls != retryMaxAttempts {
		t.Errorf("expected %d Chat calls, got %d", retryMaxAttempts, stub.chatCalls)
	}
}

func TestRetry_Stream_NoRetryAfterChunkEmitted(t *testing.T) {
	// First attempt emits a chunk THEN fails — must NOT retry (can't replay).
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{Text: "partial"})
			return ErrRateLimited
		},
	}}
	rp := NewRetryingProvider(stub)

	err := rp.Stream(context.Background(), Request{}, func(Chunk) {})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited returned, got %v", err)
	}
	if stub.streamCall != 1 {
		t.Errorf("expected exactly 1 Stream call (no replay after partial output), got %d", stub.streamCall)
	}
}

func TestRetry_Stream_RetriesWhenFailedBeforeAnyChunk(t *testing.T) {
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(func(Chunk)) error { return ErrRateLimited }, // fails before emitting
		func(onChunk func(Chunk)) error { onChunk(Chunk{Text: "ok", Done: true}); return nil },
	}}
	rp := NewRetryingProvider(stub)

	var got string
	err := rp.Stream(context.Background(), Request{}, func(c Chunk) { got += c.Text })
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if got != "ok" {
		t.Errorf("expected streamed content 'ok', got %q", got)
	}
	if stub.streamCall != 2 {
		t.Errorf("expected 2 Stream calls (1 retry), got %d", stub.streamCall)
	}
}
