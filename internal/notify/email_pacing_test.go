package notify

import (
	"context"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/mail"
)

// slowRelay takes 50-100 ms per message, as a real relay does.
type slowRelay struct{ sent atomic.Int32 }

func (r *slowRelay) Enabled() bool { return true }
func (r *slowRelay) Send(context.Context, string, mail.Message) error {
	time.Sleep(50*time.Millisecond + rand.N(50*time.Millisecond))
	r.sent.Add(1)
	return nil
}

// TestDispatch_EmailAlertsArePacedNotDropped is notify-2 end to end: a sweep dispatches its alerts one after
// another, and the deployment email channel used to drop every alert past the mail runner's notification share
// instead of waiting for the relay.
func TestDispatch_EmailAlertsArePacedNotDropped(t *testing.T) {
	relay := &slowRelay{}
	svc := mail.NewServiceWithMailer(relay, "")
	d, err := New(Config{Channels: []ChannelConfig{{Kind: "email", Values: Values{"to": "ops@example.com"}}}, Mailer: svc})
	if err != nil || !d.Enabled() {
		t.Fatalf("New = %v (enabled %v)", err, d.Enabled())
	}
	const alerts = 40
	for i := 0; i < alerts; i++ {
		d.Dispatch(context.Background(), sampleEvent())
	}
	if err := svc.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := relay.sent.Load(); n != alerts {
		t.Fatalf("%d of %d alerts sent", n, alerts)
	}
}

// heldRelay holds every send until release closes.
type heldRelay struct{ release chan struct{} }

func (r *heldRelay) Enabled() bool { return true }
func (r *heldRelay) Send(ctx context.Context, _ string, _ mail.Message) error {
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	return nil
}

// TestDispatch_AnalysisCompleteEmailDoesNotWait: an analysis_complete email (one per interactive analysis) is dropped
// when the notification share is full, as before, instead of holding its dispatch for the delivery timeout.
func TestDispatch_AnalysisCompleteEmailDoesNotWait(t *testing.T) {
	relay := &heldRelay{release: make(chan struct{})}
	svc := mail.NewServiceWithMailer(relay, "")
	defer func() {
		close(relay.release)
		_ = svc.Drain(context.Background())
	}()
	d, err := New(Config{Channels: []ChannelConfig{{Kind: "email", Values: Values{"to": "ops@example.com"}}}, Mailer: svc})
	if err != nil {
		t.Fatal(err)
	}
	// Fill the notification share with held drift alerts: each is queued at once while a slot is free.
	for i := 0; ; i++ {
		if i == 100 {
			t.Fatal("the notification share never filled")
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		d.Dispatch(ctx, sampleEvent())
		cancel()
		if time.Since(start) >= 150*time.Millisecond {
			break // this one waited: the share is full
		}
	}
	start := time.Now()
	d.Dispatch(context.Background(), Event{Type: EventAnalysisComplete, Title: "t", Analysis: &AnalysisSummary{}})
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("an analysis_complete email waited %v for the full share", took)
	}
}
