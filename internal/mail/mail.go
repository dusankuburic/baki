// Package mail provides transactional email delivery (password reset, email
// verification, org invites). It exposes a small Mailer interface with two
// implementations: an SMTP sender for production and a log-only fallback used
// whenever SMTP is not configured, so deployments without email still function
// (the would-be link is written to the server log).
package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"pad-core/logger"

	"pad-analyzer/internal/config"
)

// Mailer sends a single transactional message. Implementations must be safe for
// concurrent use.
type Mailer interface {
	// Send delivers a message to a single recipient. htmlBody may be empty.
	Send(ctx context.Context, to, subject, textBody, htmlBody string) error
	// Enabled reports whether real delivery is configured (false for the
	// log-only fallback), so callers can adjust messaging (e.g. surface a
	// token in the API response in dev).
	Enabled() bool
}

// New returns an SMTP mailer when email is configured, otherwise a log-only
// mailer. AppBaseURL is carried separately by callers that build links.
func New(cfg config.EmailConfig) Mailer {
	if cfg.Enabled() {
		return &smtpMailer{cfg: cfg}
	}
	logger.Warn("email not configured (PAD_SMTP_HOST/PAD_EMAIL_FROM unset); using log-only mailer — password reset / verification links will be written to the server log")
	return &logMailer{}
}

// logMailer is the fallback when SMTP is unconfigured. It records the message
// (including any link in the body) at info level so the flow remains usable in
// development and single-node setups without an SMTP relay.
type logMailer struct{}

func (l *logMailer) Enabled() bool { return false }

func (l *logMailer) Send(_ context.Context, to, subject, textBody, _ string) error {
	logger.Info("email (log-only mailer)", "to", to, "subject", subject, "body", textBody)
	return nil
}

type smtpMailer struct {
	cfg config.EmailConfig
}

func (m *smtpMailer) Enabled() bool { return true }

func (m *smtpMailer) Send(ctx context.Context, to, subject, textBody, htmlBody string) error {
	addr := net.JoinHostPort(m.cfg.SMTPHost, fmt.Sprintf("%d", portOf(m.cfg)))
	msg := buildMIME(m.cfg.From, to, subject, textBody, htmlBody)

	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.SMTPHost)
	}

	// Port 465 expects implicit TLS from the first byte; every other port uses
	// the STARTTLS upgrade that smtp.SendMail negotiates automatically.
	if portOf(m.cfg) == 465 {
		return m.sendImplicitTLS(ctx, addr, auth, to, msg)
	}
	if err := smtp.SendMail(addr, auth, m.cfg.From, []string{to}, msg); err != nil {
		return fmt.Errorf("mail: send via %s: %w", addr, err)
	}
	return nil
}

func (m *smtpMailer) sendImplicitTLS(ctx context.Context, addr string, auth smtp.Auth, to string, msg []byte) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: m.cfg.SMTPHost})
	if err != nil {
		return fmt.Errorf("mail: tls dial %s: %w", addr, err)
	}
	c, err := smtp.NewClient(conn, m.cfg.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: smtp client: %w", err)
	}
	defer func() { _ = c.Close() }()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := c.Mail(m.cfg.From); err != nil {
		return fmt.Errorf("mail: from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("mail: rcpt: %w", err)
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: data: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		_ = wc.Close()
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("mail: close data: %w", err)
	}
	return c.Quit()
}

// port returns the configured SMTP port, defaulting to 587 (submission).
func portOf(cfg config.EmailConfig) int {
	if cfg.SMTPPort == 0 {
		return 587
	}
	return cfg.SMTPPort
}

// buildMIME assembles an RFC 5322 message. When htmlBody is set it produces a
// multipart/alternative body; otherwise a plain-text message.
func buildMIME(from, to, subject, textBody, htmlBody string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	if htmlBody == "" {
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
		b.WriteString(textBody)
		return []byte(b.String())
	}
	const boundary = "pad_mime_boundary_8a3f"
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(textBody + "\r\n\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
	b.WriteString(htmlBody + "\r\n\r\n")
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}
