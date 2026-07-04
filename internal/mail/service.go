package mail

import (
	"context"
	"fmt"
	"strings"

	"pad-analyzer/internal/config"
)

// Service renders and sends the app's transactional emails. It centralizes
// templating and link construction so handlers just call SendX. A nil-safe,
// always-usable value is returned by NewService even without SMTP (it falls back
// to the log-only mailer).
type Service struct {
	mailer  Mailer
	baseURL string
}

// NewService builds the email service from config, choosing an SMTP or log-only
// mailer automatically. mode is passed through so the log-only fallback never
// writes token-bearing bodies to the log in cloud mode.
func NewService(cfg config.EmailConfig, mode config.DeploymentMode) *Service {
	return &Service{
		mailer:  New(cfg, mode),
		baseURL: strings.TrimRight(cfg.AppBaseURL, "/"),
	}
}

// Enabled reports whether real SMTP delivery is configured (vs log-only).
func (s *Service) Enabled() bool { return s.mailer != nil && s.mailer.Enabled() }

// link builds a frontend deep link carrying the token, or — when no public base
// URL is configured — returns the bare token so it is still recoverable (e.g.
// from the log-only mailer in development).
func (s *Service) link(hashParam, token string) string {
	if s.baseURL == "" {
		return token
	}
	return fmt.Sprintf("%s/#%s=%s", s.baseURL, hashParam, token)
}

func (s *Service) SendPasswordReset(ctx context.Context, to, rawToken string) error {
	url := s.link("resetPassword", rawToken)
	text := fmt.Sprintf(
		"We received a request to reset your PAD Analyzer password.\n\n"+
			"Use this link to choose a new password (valid for 1 hour):\n%s\n\n"+
			"If you didn't request this, you can safely ignore this email.", url)
	html := fmt.Sprintf(
		"<p>We received a request to reset your PAD Analyzer password.</p>"+
			"<p><a href=\"%s\">Reset your password</a> (valid for 1 hour).</p>"+
			"<p>If you didn't request this, you can safely ignore this email.</p>", url)
	return s.mailer.Send(ctx, to, "Reset your PAD Analyzer password", text, html)
}

func (s *Service) SendEmailVerification(ctx context.Context, to, rawToken string) error {
	url := s.link("verifyEmail", rawToken)
	text := fmt.Sprintf(
		"Welcome to PAD Analyzer!\n\nConfirm your email address with this link (valid for 24 hours):\n%s", url)
	html := fmt.Sprintf(
		"<p>Welcome to PAD Analyzer!</p><p><a href=\"%s\">Confirm your email address</a> (valid for 24 hours).</p>", url)
	return s.mailer.Send(ctx, to, "Confirm your PAD Analyzer email", text, html)
}

func (s *Service) SendOrgInvite(ctx context.Context, to, orgName, rawToken string) error {
	url := s.link("invite", rawToken)
	text := fmt.Sprintf(
		"You've been invited to join the %q organization on PAD Analyzer.\n\nAccept the invitation:\n%s", orgName, url)
	html := fmt.Sprintf(
		"<p>You've been invited to join the <strong>%s</strong> organization on PAD Analyzer.</p>"+
			"<p><a href=\"%s\">Accept the invitation</a>.</p>", orgName, url)
	return s.mailer.Send(ctx, to, "You've been invited to PAD Analyzer", text, html)
}
