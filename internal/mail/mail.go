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
// mailer. AppBaseURL is carried separately by callers that build links. mode
// controls whether the log-only fallback may write the message body (which
// carries one-time reset/verification tokens): allowed for local/dev recovery,
// but NEVER in cloud mode, where log/telemetry readers could harvest the tokens.
func New(cfg config.EmailConfig, mode config.DeploymentMode) Mailer {
	if cfg.Enabled() {
		return &smtpMailer{cfg: cfg}
	}
	logBody := mode != config.ModeCloud
	if logBody {
		logger.Warn("email not configured (PAD_SMTP_HOST/PAD_EMAIL_FROM unset); using log-only mailer — password reset / verification links will be written to the server log")
	} else {
		// Cloud mode with no SMTP is a misconfiguration: recovery/verification
		// emails silently go nowhere. Warn loudly, but do NOT log the token-
		// bearing body (that would be a credential leak into the logs).
		logger.Warn("email not configured in cloud mode (PAD_SMTP_HOST/PAD_EMAIL_FROM unset); password reset / verification emails will NOT be delivered and links are NOT logged — configure SMTP")
	}
	return &logMailer{logBody: logBody}
}

// logMailer is the fallback when SMTP is unconfigured. In local/dev mode it
// records the full message (including the link) so the flow stays usable without
// an SMTP relay; in cloud mode it logs only the recipient + subject, never the
// token-bearing body.
type logMailer struct{ logBody bool }

func (l *logMailer) Enabled() bool { return false }

func (l *logMailer) Send(_ context.Context, to, subject, textBody, _ string) error {
	if l.logBody {
		logger.Info("email (log-only mailer)", "to", to, "subject", subject, "body", textBody)
		return nil
	}
	// Never log the body in cloud mode: it carries one-time reset/verify tokens.
	logger.Info("email (log-only mailer; body redacted)", "to", to, "subject", subject)
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

	// Port 465 expects implicit TLS from the first byte; every other port must
	// use STARTTLS. STARTTLS is MANDATORY here (not opportunistic): the message
	// body routinely carries one-time password-reset tokens, so we refuse to
	// transmit it in plaintext even if the relay offers no encryption. The
	// standard smtp.SendMail only upgrades when the server advertises STARTTLS,
	// silently falling back to cleartext otherwise — that path is now closed.
	if portOf(m.cfg) == 465 {
		return m.sendImplicitTLS(ctx, addr, auth, to, msg)
	}
	return m.sendSTARTTLS(ctx, addr, auth, to, msg)
}

// deliver runs the AUTH + MAIL FROM + RCPT TO + DATA sequence on an already
// connected, TLS-wrapped smtp.Client. Shared by the implicit-TLS (port 465)
// and STARTTLS (port 587) paths.
func (m *smtpMailer) deliver(c *smtp.Client, auth smtp.Auth, to string, msg []byte) error {
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

func (m *smtpMailer) sendImplicitTLS(ctx context.Context, addr string, auth smtp.Auth, to string, msg []byte) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(&d, "tcp", addr, &tls.Config{
		ServerName: m.cfg.SMTPHost,
		MinVersion: tls.VersionTLS12, // gosec G402: refuse to negotiate below TLS 1.2
		NextProtos: []string{"smtp"},
	})
	if err != nil {
		return fmt.Errorf("mail: tls dial %s: %w", addr, err)
	}
	c, err := smtp.NewClient(conn, m.cfg.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: smtp client: %w", err)
	}
	defer func() { _ = c.Close() }()
	return m.deliver(c, auth, to, msg)
}

// sendSTARTTLS connects in plaintext, requires the server to advertise STARTTLS
// (returning an error and sending nothing if it does not), upgrades to TLS, and
// only then transmits the message. Failing closed keeps password-reset tokens
// out of cleartext when a relay doesn't offer encryption.
func (m *smtpMailer) sendSTARTTLS(ctx context.Context, addr string, auth smtp.Auth, to string, msg []byte) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	c, err := smtp.NewClient(conn, m.cfg.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: smtp client: %w", err)
	}
	defer func() { _ = c.Close() }()

	// STARTTLS-mandatory: if the server can't encrypt the channel we refuse to
	// proceed rather than fall back to cleartext (which would expose the reset
	// link in the message body).
	if ok, _ := c.Extension("STARTTLS"); !ok {
		_ = c.Quit()
		return fmt.Errorf("mail: smtp server %s does not advertise STARTTLS; refusing to send in plaintext", addr)
	}
	if err := c.StartTLS(&tls.Config{
		ServerName: m.cfg.SMTPHost,
		MinVersion: tls.VersionTLS12, // gosec G402: refuse to negotiate below TLS 1.2
	}); err != nil {
		return fmt.Errorf("mail: starttls: %w", err)
	}
	return m.deliver(c, auth, to, msg)
}

// port returns the configured SMTP port, defaulting to 587 (submission).
func portOf(cfg config.EmailConfig) int {
	if cfg.SMTPPort == 0 {
		return 587
	}
	return cfg.SMTPPort
}

// sanitizeHeader strips CR/LF so a value can never smuggle extra headers into
// the message. Callers validate their inputs today; this is the last line of
// defense at the layer that actually writes header syntax.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

// buildMIME assembles an RFC 5322 message. When htmlBody is set it produces a
// multipart/alternative body; otherwise a plain-text message.
func buildMIME(from, to, subject, textBody, htmlBody string) []byte {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(to) + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(subject) + "\r\n")
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
