package mail

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/config"
)

func TestPortOfDefault(t *testing.T) {
	if got := portOf(config.EmailConfig{}); got != 587 {
		t.Errorf("default port = %d, want 587", got)
	}
	if got := portOf(config.EmailConfig{SMTPPort: 465}); got != 465 {
		t.Errorf("explicit port = %d, want 465", got)
	}
}

func TestNewFallsBackToLogMailer(t *testing.T) {
	if New(config.EmailConfig{}).Enabled() {
		t.Error("unconfigured mailer should report Enabled()=false (log-only)")
	}
	if !New(config.EmailConfig{SMTPHost: "smtp.example.com", From: "no-reply@example.com"}).Enabled() {
		t.Error("configured mailer should report Enabled()=true")
	}
}

func TestBuildMIME(t *testing.T) {
	plain := string(buildMIME("a@x.com", "b@y.com", "Hi", "hello", ""))
	if !strings.Contains(plain, "Content-Type: text/plain") || strings.Contains(plain, "multipart") {
		t.Errorf("plain message should be text/plain only:\n%s", plain)
	}
	multi := string(buildMIME("a@x.com", "b@y.com", "Hi", "hello", "<b>hi</b>"))
	for _, want := range []string{"multipart/alternative", "text/plain", "text/html", "<b>hi</b>"} {
		if !strings.Contains(multi, want) {
			t.Errorf("multipart message missing %q:\n%s", want, multi)
		}
	}
}

// captureMailer records the last message instead of sending it.
type captureMailer struct {
	to, subject, text, html string
	calls                   int
}

func (c *captureMailer) Enabled() bool { return true }
func (c *captureMailer) Send(_ context.Context, to, subject, text, html string) error {
	c.to, c.subject, c.text, c.html = to, subject, text, html
	c.calls++
	return nil
}

func TestServiceLinkAndSend(t *testing.T) {
	cap := &captureMailer{}
	svc := &Service{mailer: cap, baseURL: "https://app.example.com"}

	if err := svc.SendPasswordReset(context.Background(), "user@example.com", "TOKEN123"); err != nil {
		t.Fatalf("SendPasswordReset: %v", err)
	}
	if cap.calls != 1 {
		t.Fatalf("expected 1 send, got %d", cap.calls)
	}
	if cap.to != "user@example.com" {
		t.Errorf("to = %q", cap.to)
	}
	wantLink := "https://app.example.com/#resetPassword=TOKEN123"
	if !strings.Contains(cap.text, wantLink) || !strings.Contains(cap.html, wantLink) {
		t.Errorf("reset link %q missing from bodies:\ntext=%s\nhtml=%s", wantLink, cap.text, cap.html)
	}
}

func TestServiceLinkWithoutBaseURLFallsBackToToken(t *testing.T) {
	cap := &captureMailer{}
	svc := &Service{mailer: cap, baseURL: ""}
	if err := svc.SendEmailVerification(context.Background(), "u@x.com", "RAWTOKEN"); err != nil {
		t.Fatalf("SendEmailVerification: %v", err)
	}
	if !strings.Contains(cap.text, "RAWTOKEN") {
		t.Errorf("expected bare token in body when no base URL set:\n%s", cap.text)
	}
}
