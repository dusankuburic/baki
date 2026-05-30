package ai

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- DemoLimiter ------------------------------------------------------------

func newTestLimiter(t *testing.T) *DemoLimiter {
	t.Helper()
	return NewDemoLimiter(t.TempDir())
}

func TestDemoLimiter_Remaining_Fresh(t *testing.T) {
	l := newTestLimiter(t)
	got, err := l.Remaining()
	if err != nil {
		t.Fatalf("Remaining: %v", err)
	}
	if got != l.dailyLimit {
		t.Errorf("fresh limiter: got %d remaining, want %d", got, l.dailyLimit)
	}
}

func TestDemoLimiter_ReserveForDisplay_DecrementsRemaining(t *testing.T) {
	l := newTestLimiter(t)

	before, _ := l.Remaining()
	rem, err := l.ReserveForDisplay()
	if err != nil {
		t.Fatalf("ReserveForDisplay: %v", err)
	}
	if rem != before-1 {
		t.Errorf("after one reserve: returned %d, want %d", rem, before-1)
	}

	after, _ := l.Remaining()
	if after != before-1 {
		t.Errorf("Remaining after reserve: %d, want %d", after, before-1)
	}
}

func TestDemoLimiter_ReserveForDisplay_ExhaustsLimit(t *testing.T) {
	l := newTestLimiter(t)

	for i := 0; i < l.dailyLimit; i++ {
		if _, err := l.ReserveForDisplay(); err != nil {
			t.Fatalf("ReserveForDisplay call %d: %v", i+1, err)
		}
	}

	// The next call must fail.
	_, err := l.ReserveForDisplay()
	if err == nil {
		t.Fatal("expected error when daily limit is exhausted, got nil")
	}
}

// TestDemoLimiter_ConcurrentReserve_NoLostIncrements stresses the
// concurrency invariant claimed by N-4. The audit suggested that two
// goroutines could both load, both see headroom, both write back, losing
// an increment. Reading the code shows the mutex covers the full
// load-modify-write cycle (defer Unlock runs after saveState), so this
// shouldn't happen — but a race-detector test makes the invariant
// machine-checked rather than just commented.
//
// Behaviour locked in:
//
//   - exactly `dailyLimit` reservations succeed across all goroutines
//   - all subsequent calls return an "exhausted" error
//   - no `-race` violations
func TestDemoLimiter_ConcurrentReserve_NoLostIncrements(t *testing.T) {
	l := newTestLimiter(t)

	const N = 32
	var (
		ok      atomic.Int64
		failed  atomic.Int64
		wg      sync.WaitGroup
		release = make(chan struct{})
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-release // release all goroutines simultaneously
			if _, err := l.ReserveForDisplay(); err != nil {
				failed.Add(1)
			} else {
				ok.Add(1)
			}
		}()
	}
	close(release)
	wg.Wait()

	if got := ok.Load(); got != int64(l.dailyLimit) {
		t.Errorf("expected exactly %d successful reservations, got %d (lost increments?)", l.dailyLimit, got)
	}
	if got := failed.Load(); got != int64(N-l.dailyLimit) {
		t.Errorf("expected %d failures, got %d", N-l.dailyLimit, got)
	}

	// And Remaining should report 0 — corroborates that the persisted state
	// matches the count of successful reservations.
	rem, err := l.Remaining()
	if err != nil {
		t.Fatalf("Remaining: %v", err)
	}
	if rem != 0 {
		t.Errorf("Remaining after full exhaustion: got %d, want 0", rem)
	}
}

func TestDemoLimiter_Remaining_AfterExhaustion(t *testing.T) {
	l := newTestLimiter(t)
	for i := 0; i < l.dailyLimit; i++ {
		l.ReserveForDisplay() //nolint:errcheck
	}

	got, err := l.Remaining()
	if err != nil {
		t.Fatalf("Remaining after exhaustion: %v", err)
	}
	if got != 0 {
		t.Errorf("expected 0 remaining after exhaustion, got %d", got)
	}
}

func TestDemoLimiter_ResetsOnNewDay(t *testing.T) {
	l := newTestLimiter(t)

	// Write a state file dated yesterday so that today's count appears exhausted.
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := l.saveState(&demoState{Date: yesterday, Count: l.dailyLimit}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	// Remaining should return the full daily limit (yesterday's count is ignored).
	got, err := l.Remaining()
	if err != nil {
		t.Fatalf("Remaining: %v", err)
	}
	if got != l.dailyLimit {
		t.Errorf("after date reset: got %d remaining, want full limit %d", got, l.dailyLimit)
	}
}

func TestDemoLimiter_CorruptStateFile_ReturnsFullLimit(t *testing.T) {
	l := newTestLimiter(t)

	// Create the directory first (saveState would do this, but write directly here).
	if err := os.MkdirAll(l.counterFile[:len(l.counterFile)-len("demo.json")-1], 0755); err != nil {
		// Fallback: just write a valid state first to trigger dir creation.
		_ = l.saveState(&demoState{})
	}
	// Overwrite with corrupt JSON.
	if err := os.WriteFile(l.counterFile, []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	got, err := l.Remaining()
	if err != nil {
		t.Fatalf("Remaining with corrupt state: %v", err)
	}
	if got != l.dailyLimit {
		t.Errorf("corrupt state: got %d remaining, want %d (full limit)", got, l.dailyLimit)
	}
}

// ---- DemoProvider -----------------------------------------------------------

func TestDemoProvider_Identity(t *testing.T) {
	d := NewDemoProvider()
	if d.ID() != "demo" {
		t.Errorf("ID() = %q, want %q", d.ID(), "demo")
	}
	if d.Name() != "Demo" {
		t.Errorf("Name() = %q, want %q", d.Name(), "Demo")
	}
	if d.ContextLimit() <= 0 {
		t.Errorf("ContextLimit() = %d, want > 0", d.ContextLimit())
	}
	if d.DefaultModel() == "" {
		t.Error("DefaultModel() must not be empty")
	}
	p := d.PricePerMillionTokens()
	if p.InputCostPerM != 0 || p.OutputCostPerM != 0 {
		t.Errorf("demo provider should have zero pricing, got %+v", p)
	}
}

func TestDemoProvider_EstimateTokens(t *testing.T) {
	d := NewDemoProvider()
	n := d.EstimateTokens("hello world")
	if n <= 0 {
		t.Errorf("EstimateTokens(\"hello world\") = %d, want > 0", n)
	}
}

func TestDemoProvider_Models_NonEmpty(t *testing.T) {
	d := NewDemoProvider()
	models := d.Models()
	if len(models) == 0 {
		t.Fatal("Models() must return at least one entry")
	}
	for _, m := range models {
		if m.ID == "" {
			t.Error("model with empty ID")
		}
	}
}

func TestDemoProvider_Chat_NoDemoURL(t *testing.T) {
	orig := DemoProxyURL
	DemoProxyURL = ""
	defer func() { DemoProxyURL = orig }()

	d := NewDemoProvider()
	_, err := d.Chat(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error when DemoProxyURL is empty")
	}
}

func TestDemoProvider_Stream_NoDemoURL(t *testing.T) {
	orig := DemoProxyURL
	DemoProxyURL = ""
	defer func() { DemoProxyURL = orig }()

	d := NewDemoProvider()
	err := d.Stream(context.Background(), Request{}, func(Chunk) {})
	if err == nil {
		t.Fatal("expected error when DemoProxyURL is empty")
	}
}
