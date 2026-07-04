package mail

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

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
	if New(config.EmailConfig{}, config.ModeLocal).Enabled() {
		t.Error("unconfigured mailer should report Enabled()=false (log-only)")
	}
	if !New(config.EmailConfig{SMTPHost: "smtp.example.com", From: "no-reply@example.com"}, config.ModeCloud).Enabled() {
		t.Error("configured mailer should report Enabled()=true")
	}
}

// TestLogMailer_CloudModeRedactsBody is the M3 regression: the log-only fallback
// must NOT write the token-bearing body to logs in cloud mode (a credential leak
// harvestable from logs/telemetry), while local/dev keeps it for recovery.
func TestLogMailer_CloudModeRedactsBody(t *testing.T) {
	cloud, ok := New(config.EmailConfig{}, config.ModeCloud).(*logMailer)
	if !ok || cloud.logBody {
		t.Fatal("cloud-mode log-only mailer must have logBody=false (body redacted)")
	}
	local, ok := New(config.EmailConfig{}, config.ModeLocal).(*logMailer)
	if !ok || !local.logBody {
		t.Fatal("local-mode log-only mailer should keep logBody=true for dev recovery")
	}
	// Sending must not error in either mode (the body is simply omitted in cloud).
	if err := cloud.Send(context.Background(), "u@x.com", "Reset", "https://app/#resetPassword=SECRET-TOKEN", ""); err != nil {
		t.Fatalf("cloud Send errored: %v", err)
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

// startSMTPStub launches a minimal in-process SMTP responder that speaks just
// enough of the protocol for net/smtp's client to run through EHLO + QUIT. The
// extensions advertised are controlled by advertiseSTARTTLS; the raw DATA body
// (if the client ever sends one) is captured into bodySeen. Returns the host,
// port, a channel for observed bodies, and a cleanup func.
func startSMTPStub(t *testing.T, advertiseSTARTTLS bool) (host string, port int, bodySeen <-chan string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	portI := 0
	for i := 0; i < len(portStr); i++ {
		portI = portI*10 + int(portStr[i]-'0')
	}
	port = portI

	bodyCh := make(chan string, 4)
	var stopOnce sync.Once
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSMTP(conn, advertiseSTARTTLS, bodyCh)
		}
	}()
	cleanup = func() { stopOnce.Do(func() { ln.Close() }) }
	return host, port, bodyCh, cleanup
}

func handleSMTP(conn net.Conn, advertiseSTARTTLS bool, bodyCh chan<- string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	write := func(s string) { _, _ = bw.WriteString(s); _ = bw.Flush() }
	write("220 stub.smtp ESMTP\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		upper := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250-stub.smtp\r\n")
			if advertiseSTARTTLS {
				write("250-STARTTLS\r\n")
			}
			write("250 8BITMIME\r\n")
		case strings.HasPrefix(upper, "STARTTLS"):
			write("220 ready to start tls\r\n")
			return
		case strings.HasPrefix(upper, "QUIT"):
			write("221 bye\r\n")
			return
		case strings.HasPrefix(upper, "MAIL FROM:"), strings.HasPrefix(upper, "RCPT TO:"):
			write("250 ok\r\n")
		case strings.HasPrefix(upper, "DATA"):
			write("354 end with <CRLF>.<CRLF>\r\n")
			var sb strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimSpace(l) == "." {
					break
				}
				sb.WriteString(l)
			}
			select {
			case bodyCh <- sb.String():
			default:
			}
			write("250 ok queued\r\n")
		default:
			write("250 ok\r\n")
		}
	}
}

// TestSMTP_RequiresSTARTTLS_NoSTARTTLS is the regression test for the
// password-reset-over-cleartext fix: when the relay does NOT advertise STARTTLS
// the mailer must refuse to send (return an error mentioning STARTTLS) and must
// NOT transmit the message body. Previously smtp.SendMail would opportunistically
// fall back to plaintext, leaking the reset link onto the wire.
func TestSMTP_RequiresSTARTTLS_NoSTARTTLS(t *testing.T) {
	host, port, bodySeen, cleanup := startSMTPStub(t, false)
	defer cleanup()

	m := &smtpMailer{cfg: config.EmailConfig{
		SMTPHost: host, SMTPPort: port, From: "from@example.com",
	}}
	err := m.Send(context.Background(), "to@example.com", "reset",
		"https://app.example.com/reset?token=SECRET", "")
	if err == nil {
		t.Fatal("expected error when STARTTLS is not advertised, got nil")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("error should mention STARTTLS, got: %v", err)
	}
	// The body (with the reset token) must never have been transmitted.
	select {
	case body := <-bodySeen:
		t.Errorf("message body was transmitted in plaintext: %q", body)
	case <-time.After(100 * time.Millisecond):
		// expected — no body sent
	}
}
