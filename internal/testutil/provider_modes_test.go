package testutil

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
)

// R-testing-5: the shared fake could only script text and a Done chunk, ignored the context and kept its call
// counter without a lock, so every failure-mode test wrote its own provider. These pin the modes it now has.

func collect(t *testing.T, p *FakeProvider, ctx context.Context) ([]ai.Chunk, error) {
	t.Helper()
	var got []ai.Chunk
	err := p.Stream(ctx, ai.Request{Model: "m"}, func(c ai.Chunk) { got = append(got, c) })
	return got, err
}

// Calls and Requests are read while the chat worker streams on another goroutine: run with -race.
func TestFakeProvider_CallsAndRequestsAreSafeAcrossGoroutines(t *testing.T) {
	p := &FakeProvider{Fallback: &FakeTurn{Text: "ok"}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = p.Stream(context.Background(), ai.Request{Model: "m"}, func(ai.Chunk) {})
		}()
		go func() {
			defer wg.Done()
			_, _ = p.Calls(), p.Requests()
		}()
	}
	wg.Wait()
	if p.Calls() != 8 || len(p.Requests()) != 8 {
		t.Errorf("calls %d, requests %d, want 8 each", p.Calls(), len(p.Requests()))
	}
}

// Requests hands out a copy: a caller appending to it does not change what the fake recorded.
func TestFakeProvider_RequestsIsACopy(t *testing.T) {
	p := &FakeProvider{}
	_ = p.Stream(context.Background(), ai.Request{Model: "m"}, func(ai.Chunk) {})
	got := p.Requests()
	got[0].Model = "changed"
	if p.Requests()[0].Model != "m" {
		t.Error("Requests returned the fake's own slice")
	}
}

// Record lets a stub that scripts its own chunks keep the shared recorder; it answers the call's index.
func TestFakeProvider_RecordAnswersTheIndex(t *testing.T) {
	p := &FakeProvider{}
	if i := p.Record(ai.Request{Model: "a"}); i != 0 {
		t.Errorf("first Record = %d, want 0", i)
	}
	if i := p.Record(ai.Request{Model: "b"}); i != 1 {
		t.Errorf("second Record = %d, want 1", i)
	}
	if got := p.Requests(); len(got) != 2 || got[1].Model != "b" {
		t.Errorf("Requests = %+v", got)
	}
}

// With HonourCtx, a call on an ended context fails with its error before any chunk; without it the fake streams
// as it always did (TestStream_LegacyCancelledBeforeBegin_RecordsTerminalState depends on that).
func TestFakeProvider_HonourCtxIsOptIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	honouring := &FakeProvider{HonourCtx: true, Fallback: &FakeTurn{Text: "late"}}
	got, err := collect(t, honouring, ctx)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Errorf("HonourCtx on an ended context: %d chunks, err %v; want none and context.Canceled", len(got), err)
	}
	legacy := &FakeProvider{Fallback: &FakeTurn{Text: "late"}}
	got, err = collect(t, legacy, ctx)
	if err != nil || len(got) != 2 {
		t.Errorf("without HonourCtx: %d chunks, err %v; want the text and Done", len(got), err)
	}
}

// A blocking turn streams its text, then waits for the stream's context to end and returns its error.
func TestFakeProvider_BlockReturnsWhenTheContextEnds(t *testing.T) {
	p := &FakeProvider{Turns: []FakeTurn{{Text: "thinking", Block: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var got []ai.Chunk
	var mu sync.Mutex
	go func() {
		done <- p.Stream(ctx, ai.Request{}, func(c ai.Chunk) { mu.Lock(); got = append(got, c); mu.Unlock() })
	}()
	select {
	case err := <-done:
		t.Fatalf("a blocking turn returned before its context ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a blocking turn did not return after its context ended")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Text != "thinking" || got[0].Done {
		t.Errorf("chunks = %+v, want the text and no Done", got)
	}
}

// ChunkErr ends a turn with an error chunk carrying the turn's usage and served model, in place of Done.
func TestFakeProvider_ChunkErrEndsTheTurnWithAnErrorChunk(t *testing.T) {
	boom := errors.New("overloaded")
	p := &FakeProvider{Turns: []FakeTurn{{Text: "part", ChunkErr: boom, TokensIn: 7, TokensOut: 2, ServedModel: "m-2"}}}
	got, err := collect(t, p, context.Background())
	if err != nil {
		t.Fatalf("Stream = %v, want nil (the error rides on a chunk)", err)
	}
	if len(got) != 2 || got[0].Text != "part" {
		t.Fatalf("chunks = %+v", got)
	}
	last := got[1]
	if !errors.Is(last.Error, boom) || last.Done || last.TokensIn != 7 || last.TokensOut != 2 || last.ServedModel != "m-2" {
		t.Errorf("last chunk = %+v, want the error with the turn's usage and served model, not Done", last)
	}
}

// A turn's Err fails that call before any chunk, and the next call goes on with the next turn.
func TestFakeProvider_TurnErrFailsOneCall(t *testing.T) {
	boom := errors.New("rate limited")
	p := &FakeProvider{Turns: []FakeTurn{{Err: boom}, {Text: "second"}}}
	if got, err := collect(t, p, context.Background()); !errors.Is(err, boom) || len(got) != 0 {
		t.Errorf("first call: %d chunks, err %v", len(got), err)
	}
	if got, err := collect(t, p, context.Background()); err != nil || len(got) != 2 || got[0].Text != "second" {
		t.Errorf("second call: %+v, err %v", got, err)
	}
	if p.Calls() != 2 {
		t.Errorf("Calls = %d, want 2", p.Calls())
	}
}

// The Done chunk carries the turn's served model.
func TestFakeProvider_DoneCarriesTheServedModel(t *testing.T) {
	p := &FakeProvider{Turns: []FakeTurn{{Text: "a", ServedModel: "gpt-x"}}}
	got, _ := collect(t, p, context.Background())
	if len(got) != 2 || !got[1].Done || got[1].ServedModel != "gpt-x" {
		t.Errorf("chunks = %+v", got)
	}
}
