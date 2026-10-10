// Package mail renders and sends every email the product sends: a registry of typed templates (templates*.go,
// text/template + html/template), one bounded runner for every send (Service), and a small Mailer interface with an
// SMTP sender for production and a log-only fallback used whenever SMTP is not configured, so deployments without
// email still function.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"time"

	"pad-analyzer/internal/capability"
	"pad-core/logger"

	"pad-analyzer/internal/config"
)

// Mailer sends a single rendered message. Implementations must be safe for
// concurrent use.
type Mailer interface {
	// Send delivers msg to a single recipient. msg.HTML may be empty.
	Send(ctx context.Context, to string, msg Message) error
	// Enabled reports whether real delivery is configured (false for the
	// log-only fallback), so callers can adjust messaging (e.g. surface a
	// token in the API response in dev).
	Enabled() bool
}

// New returns an SMTP mailer when email is configured, otherwise a log-only
// mailer. AppBaseURL is carried separately by callers that build links. caps
// controls whether the log-only fallback may write the message body (which
// carries one-time reset/verification tokens): allowed for local/dev
// recovery, NEVER in the cloud profile, where log/telemetry readers could
// harvest the tokens.
func New(cfg config.EmailConfig, caps capability.Set) Mailer {
	if cfg.Enabled() {
		// Said once at boot rather than only per send. Not a boot failure: the previous release sent a From it could
		// not parse as it was, so refusing to start would turn an upgrade into a crash loop after its migrations ran.
		if _, err := parseAddress(cfg.From); err != nil {
			logger.Error("PAD_EMAIL_FROM is not an email address (\"addr@example.com\" or \"Name <addr@example.com>\"); every email will fail until it is fixed", "error", err)
		}
		return newSMTPMailer(cfg)
	}
	logBody := caps.Profile() != capability.Cloud
	if logBody {
		logger.Warn("email not configured (PAD_SMTP_HOST/PAD_EMAIL_FROM unset); using log-only mailer — password reset / verification links will be written to the server log")
	} else {
		// Cloud mode with no SMTP is a misconfiguration: recovery/verification
		// emails silently go nowhere. Warn loudly, but do NOT log the
		// token-bearing body (a credential leak into the logs).
		logger.Warn("email not configured in cloud mode (PAD_SMTP_HOST/PAD_EMAIL_FROM unset); password reset / verification emails will NOT be delivered and links are NOT logged — configure SMTP")
	}
	return &logMailer{logBody: logBody}
}

// logMailer is the fallback when SMTP is unconfigured. In local/dev mode it
// records the full message (including the link) so the flow stays usable
// without an SMTP relay; in cloud mode it logs only recipient + subject,
// never the token-bearing body.
type logMailer struct{ logBody bool }

func (l *logMailer) Enabled() bool { return false }

func (l *logMailer) Send(_ context.Context, to string, msg Message) error {
	if l.logBody {
		logger.Info("email (log-only mailer)", "template", msg.Template, "to", to, "subject", msg.Subject, "body", msg.Text)
		return nil
	}
	// Never log the body in cloud mode: it carries one-time reset/verify tokens.
	logger.Info("email (log-only mailer; body redacted)", "template", msg.Template, "to", to, "subject", msg.Subject)
	return nil
}

type smtpMailer struct {
	cfg config.EmailConfig
	// conns holds a slot per SMTP session open to the relay (PAD_SMTP_MAX_CONNECTIONS), nil without a cap; bulk is
	// the share of it non-sensitive mail may hold, one connection less, so a burst of notifications against a relay
	// that allows few connections cannot keep a password reset waiting behind all of them.
	conns, bulk chan struct{}
}

// newSMTPMailer is the relay mailer, with the connection cap of cfg.
func newSMTPMailer(cfg config.EmailConfig) *smtpMailer {
	m := &smtpMailer{cfg: cfg}
	if n := cfg.MaxConnections; n > 0 {
		m.conns = make(chan struct{}, n)
		m.bulk = make(chan struct{}, max(n-1, 1))
	}
	return m
}

// connection waits, while ctx lives, for a connection slot to the relay and returns its release.
func (m *smtpMailer) connection(ctx context.Context, sensitive bool) (release func(), err error) {
	if m.conns == nil {
		return func() {}, nil
	}
	if !sensitive {
		select {
		case m.bulk <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case m.conns <- struct{}{}:
	case <-ctx.Done():
		if !sensitive {
			<-m.bulk
		}
		return nil, ctx.Err()
	}
	return func() {
		<-m.conns
		if !sensitive {
			<-m.bulk
		}
	}, nil
}

func (m *smtpMailer) Enabled() bool { return true }

func (m *smtpMailer) Send(ctx context.Context, to string, msg Message) error {
	from, err := parseAddress(m.cfg.From)
	if err != nil {
		return &smtpError{phase: "PAD_EMAIL_FROM", err: err}
	}
	rcpt, err := parseAddress(to)
	if err != nil {
		return &smtpError{phase: "recipient", err: err}
	}
	raw, err := buildMessage(from, rcpt, msg, mimeOpts{now: time.Now()})
	if err != nil {
		return &smtpError{phase: "build", err: err}
	}
	addr := net.JoinHostPort(m.cfg.SMTPHost, fmt.Sprintf("%d", portOf(m.cfg)))
	env := envelope{from: from.Address, to: rcpt.Address, data: raw}
	release, err := m.connection(ctx, msg.Sensitive)
	if err != nil {
		return &smtpError{phase: "connection slot", err: err}
	}
	defer release()

	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password.Reveal(), m.cfg.SMTPHost)
	}

	// Port 465 expects implicit TLS from the first byte; every other port must
	// use STARTTLS. STARTTLS is MANDATORY here (not opportunistic): the message
	// body routinely carries one-time password-reset tokens, so we refuse to
	// transmit it in plaintext even if the relay offers no encryption (the
	// standard smtp.SendMail silently falls back to cleartext — that path is
	// closed).
	if portOf(m.cfg) == 465 {
		return m.sendImplicitTLS(ctx, addr, auth, env)
	}
	return m.sendSTARTTLS(ctx, addr, auth, env)
}

// smtpError is a failed step of a send: its phase (a fixed name: "dial", "rcpt", ...), where (the relay's address,
// for the connection phases) and the cause. Its text is for the local log only: a relay's reply can name the
// recipient ("550 5.1.1 <una@example.com>: unknown user"). The error sink gets reportable(err).
type smtpError struct {
	phase string
	where string
	err   error
}

func (e *smtpError) Error() string {
	if e.where != "" {
		return "mail: " + e.phase + " " + e.where + ": " + e.err.Error()
	}
	return "mail: " + e.phase + ": " + e.err.Error()
}

func (e *smtpError) Unwrap() error { return e.err }

// reportable is what the error sink gets of a failed send: the phase and the relay's reply code ("mail: rcpt: 550"),
// or "timeout", and nothing of the reply's text or the addresses.
func reportable(err error) error {
	phase := "send"
	var se *smtpError
	if errors.As(err, &se) {
		phase = se.phase
	}
	var te *textproto.Error
	var ne net.Error
	switch {
	case errors.As(err, &te):
		return fmt.Errorf("mail: %s: %d", phase, te.Code)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return fmt.Errorf("mail: %s: timeout", phase)
	}
	return fmt.Errorf("mail: %s: failed", phase)
}

// envelope is one SMTP transaction: the bare sender and recipient addresses (MAIL FROM / RCPT TO) and the message.
type envelope struct {
	from, to string
	data     []byte
}

// smtpOpTimeout bounds the entire SMTP exchange (AUTH + MAIL + RCPT + DATA +
// Quit) after the connection is established; without it a stalled relay can
// hang the calling goroutine until the OS TCP timeout (minutes).
const smtpOpTimeout = 30 * time.Second

// connDeadline is now+d, or ctx's deadline when that comes first.
func connDeadline(ctx context.Context, d time.Duration) time.Time {
	t := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Before(t) {
		return dl
	}
	return t
}

// deliver runs the AUTH + MAIL FROM + RCPT TO + DATA sequence on an already
// connected, TLS-wrapped smtp.Client. Shared by the implicit-TLS (port 465)
// and STARTTLS (port 587) paths.
func (m *smtpMailer) deliver(c *smtp.Client, auth smtp.Auth, env envelope) error {
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return &smtpError{phase: "auth", err: err}
		}
	}
	if err := c.Mail(env.from); err != nil {
		return &smtpError{phase: "from", err: err}
	}
	if err := c.Rcpt(env.to); err != nil {
		return &smtpError{phase: "rcpt", err: err}
	}
	wc, err := c.Data()
	if err != nil {
		return &smtpError{phase: "data", err: err}
	}
	if _, err := wc.Write(env.data); err != nil {
		_ = wc.Close()
		return &smtpError{phase: "write", err: err}
	}
	if err := wc.Close(); err != nil {
		return &smtpError{phase: "close data", err: err}
	}
	if err := c.Quit(); err != nil {
		return &smtpError{phase: "quit", err: err}
	}
	return nil
}

func (m *smtpMailer) sendImplicitTLS(ctx context.Context, addr string, auth smtp.Auth, env envelope) error {
	// DialContext (not tls.DialWithDialer, which ignores ctx) so a cancelled
	// request aborts the dial early instead of riding out the 10s timeout —
	// mirroring sendSTARTTLS below.
	d := net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return &smtpError{phase: "dial", where: addr, err: err}
	}
	// net/smtp takes no context: closing the connection when ctx ends is what makes a send honour it.
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	conn := tls.Client(rawConn, &tls.Config{
		ServerName: m.cfg.SMTPHost,
		MinVersion: tls.VersionTLS12, // gosec G402: refuse to negotiate below TLS 1.2
		NextProtos: []string{"smtp"},
	})
	// The deadline additionally bounds a peer that accepts TCP but stalls the
	// TLS handshake; HandshakeContext honors ctx cancellation.
	_ = conn.SetDeadline(connDeadline(ctx, 10*time.Second))
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return &smtpError{phase: "tls handshake", where: addr, err: err}
	}
	c, err := smtp.NewClient(conn, m.cfg.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return &smtpError{phase: "smtp client", err: err}
	}
	defer func() { _ = c.Close() }()
	// Bound the SMTP exchange so a stalled relay can't hang the caller.
	_ = conn.SetDeadline(connDeadline(ctx, smtpOpTimeout))
	return m.deliver(c, auth, env)
}

// sendSTARTTLS connects in plaintext, requires the server to advertise STARTTLS
// (erroring and sending nothing if it does not), upgrades to TLS, and only
// then transmits — failing closed keeps password-reset tokens out of
// cleartext when a relay offers no encryption.
func (m *smtpMailer) sendSTARTTLS(ctx context.Context, addr string, auth smtp.Auth, env envelope) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return &smtpError{phase: "dial", where: addr, err: err}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	// Bound the pre-TLS SMTP exchange (server greeting, EHLO, STARTTLS
	// handshake). The smtp library doesn't accept a context, so without a
	// deadline a relay that accepts TCP but stalls on EHLO/STARTTLS would hang
	// the caller until the OS TCP timeout (minutes) — under password-reset /
	// invite traffic that exhausts goroutines. Mirrors sendImplicitTLS.
	_ = conn.SetDeadline(connDeadline(ctx, 10*time.Second))
	c, err := smtp.NewClient(conn, m.cfg.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return &smtpError{phase: "smtp client", err: err}
	}
	defer func() { _ = c.Close() }()

	// STARTTLS-mandatory: if the server can't encrypt the channel we refuse
	// rather than fall back to cleartext (which would expose the reset link
	// in the message body).
	if ok, _ := c.Extension("STARTTLS"); !ok {
		_ = c.Quit()
		return &smtpError{phase: "starttls", where: addr, err: errors.New("STARTTLS not advertised; refusing to send in plaintext")}
	}
	if err := c.StartTLS(&tls.Config{
		ServerName: m.cfg.SMTPHost,
		MinVersion: tls.VersionTLS12, // gosec G402: refuse to negotiate below TLS 1.2
	}); err != nil {
		return &smtpError{phase: "starttls", err: err}
	}
	// Reset to the longer operational deadline for the authenticated exchange.
	_ = conn.SetDeadline(connDeadline(ctx, smtpOpTimeout))
	return m.deliver(c, auth, env)
}

// port returns the configured SMTP port, defaulting to 587 (submission).
func portOf(cfg config.EmailConfig) int {
	if cfg.SMTPPort == 0 {
		return 587
	}
	return cfg.SMTPPort
}
