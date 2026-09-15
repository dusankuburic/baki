package websocket

import (
	"testing"
	"time"
)

// TestInboundLimiter_AllowsBurstThenThrottles pins the shape: an interactive
// burst passes untouched, a sustained flood does not.
//
// Connection count and the outgoing buffer were bounded, but the inbound RATE
// was not — and every accepted message is amplified into K marshals (K = room
// size) plus a 64 KB Redis PUBLISH that every replica decodes.
func TestInboundLimiter_AllowsBurstThenThrottles(t *testing.T) {
	l := newInboundLimiter(inboundRate, inboundBurst)

	for i := 0; i < inboundBurst; i++ {
		if !l.allow() {
			t.Fatalf("message %d of the burst was shed; burst is %d", i+1, inboundBurst)
		}
	}
	// The bucket is empty: the next message in the same instant must be shed.
	if l.allow() {
		t.Error("a message past the burst was accepted without any refill")
	}
}

// TestInboundLimiter_RefillsOverTime proves throttling is temporary — a client
// that slows down is served again rather than being wedged shut.
func TestInboundLimiter_RefillsOverTime(t *testing.T) {
	l := newInboundLimiter(inboundRate, inboundBurst)
	for i := 0; i < inboundBurst; i++ {
		l.allow()
	}
	if l.allow() {
		t.Fatal("bucket was not empty")
	}

	// Rewind the clock rather than sleeping: 1/inboundRate seconds is enough for
	// exactly one token, and a real sleep here would make the test slow and
	// timing-dependent for no added confidence.
	l.last = l.last.Add(-time.Second)
	allowed := 0
	for i := 0; i < inboundRate+10; i++ {
		if l.allow() {
			allowed++
		}
	}
	if allowed < inboundRate-1 || allowed > inboundRate+1 {
		t.Errorf("after 1s of refill, allowed %d messages, want about %d", allowed, inboundRate)
	}
}

// TestInboundLimiter_WarningIsItselfRateLimited: the shed path sends an error
// frame back, so without this a flooding client would be answered at flood rate
// and the mitigation would become an outbound amplifier.
func TestInboundLimiter_WarningIsItselfRateLimited(t *testing.T) {
	l := newInboundLimiter(inboundRate, inboundBurst)
	if !l.shouldWarn() {
		t.Fatal("first warning suppressed")
	}
	warns := 0
	for i := 0; i < 1000; i++ {
		if l.shouldWarn() {
			warns++
		}
	}
	if warns != 0 {
		t.Errorf("sent %d additional warnings within the 5s window, want 0", warns)
	}

	l.warnedAt = l.warnedAt.Add(-6 * time.Second)
	if !l.shouldWarn() {
		t.Error("no warning after the window elapsed")
	}
}
