package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/config"
)

// PAD_SMTP_MAX_CONNECTIONS caps the sessions open to the relay; non-sensitive mail may hold one connection less
// than the cap, so a password reset is never queued behind a burst of notifications.
func TestSMTP_ConnectionCapKeepsOneForSensitiveMail(t *testing.T) {
	m := newSMTPMailer(config.EmailConfig{SMTPHost: "smtp.example.com", From: "a@example.com", MaxConnections: 2})
	ctx := context.Background()
	short := func() context.Context {
		c, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		t.Cleanup(cancel)
		return c
	}

	bulk1, err := m.connection(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.connection(short(), false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second notification got a connection (%v); notifications may hold one less than the cap", err)
	}
	reset, err := m.connection(short(), true)
	if err != nil {
		t.Fatalf("a password reset waited behind a notification: %v", err)
	}
	if _, err := m.connection(short(), true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a third session opened past the cap of 2 (%v)", err)
	}
	reset()
	bulk1()
	// The refused waits released what they took: the full cap is free again.
	for i := range 2 {
		if _, err := m.connection(short(), i == 1); err != nil {
			t.Fatalf("slot %d not free after the releases: %v", i+1, err)
		}
	}
}

// A send waits for a slot before it dials, honouring its context; without a cap nothing waits.
func TestSMTP_SendWaitsForAConnectionSlot(t *testing.T) {
	m := newSMTPMailer(config.EmailConfig{SMTPHost: "127.0.0.1", SMTPPort: 1, From: "a@example.com", MaxConnections: 1})
	release, err := m.connection(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = m.Send(ctx, "b@example.com", Message{Subject: "s", Text: "t", HTML: "<p>t</p>", Sensitive: true})
	if err == nil || !strings.Contains(reportable(err).Error(), "connection slot: timeout") {
		t.Fatalf("Send with every connection taken = %v (reported %v), want a connection-slot timeout before any dial", err, reportable(err))
	}

	if newSMTPMailer(config.EmailConfig{SMTPHost: "h", From: "a@example.com"}).conns != nil {
		t.Error("no cap configured, yet sends are capped")
	}
}
