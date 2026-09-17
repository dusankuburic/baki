package mail

import (
	"context"
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

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

// appLink is the app's entry point, or "" when no public base URL is configured
// (callers then omit the link rather than print a relative one).
//
// It is deliberately not a per-finding deep link: the frontend has no route
// for one, and every finding email is paired with a targeted in-app alert
// whose click opens the flow. Sending the reader to the app, where that alert
// is waiting, is the link that actually works.
func (s *Service) appLink() string {
	if s.baseURL == "" {
		return ""
	}
	return s.baseURL + "/"
}

// findingLinkText/HTML render the shared "view it" footer of the finding
// emails; empty when there is no base URL to link to.
func (s *Service) findingLinkText() string {
	if url := s.appLink(); url != "" {
		return fmt.Sprintf("\nView it in PAD Analyzer (it is waiting in your notifications):\n%s\n", url)
	}
	return ""
}

func (s *Service) findingLinkHTML() string {
	if url := s.appLink(); url != "" {
		// The base URL is operator config, not user input, but it still lands
		// in an href: escape it so a stray quote cannot break the attribute.
		return fmt.Sprintf("<p><a href=\"%s\">View the finding</a> — it is waiting in your notifications.</p>", html.EscapeString(url))
	}
	return ""
}

// SendAlert delivers a generic governance alert email (the body is provided by
// the notify package). It satisfies notify.EmailSender so the notify package can
// route events to email without depending on mail directly.
func (s *Service) SendAlert(ctx context.Context, to, subject, plainBody, htmlBody string) error {
	if s == nil {
		return nil
	}
	return s.mailer.Send(ctx, to, subject, plainBody, htmlBody)
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
	// orgName is user-controlled; escape it so a crafted org name cannot
	// inject markup into the invite email.
	htmlBody := fmt.Sprintf(
		"<p>You've been invited to join the <strong>%s</strong> organization on PAD Analyzer.</p>"+
			"<p><a href=\"%s\">Accept the invitation</a>.</p>", html.EscapeString(orgName), url)
	return s.mailer.Send(ctx, to, "You've been invited to PAD Analyzer", text, htmlBody)
}

// SendFindingAssigned notifies a user that a finding was assigned to them.
// findingTitle/flowName are user/content-controlled and HTML-escaped. The link
// is built here from the configured base URL (see appLink) and omitted when
// none is set.
func (s *Service) SendFindingAssigned(ctx context.Context, to, assigneeName, assignerName, flowName, findingTitle string) error {
	if s == nil {
		return nil
	}
	text := fmt.Sprintf(
		"Hi %s,\n\n%s assigned a finding to you on the flow %q:\n\n  %s\n",
		assigneeName, assignerName, flowName, findingTitle) + s.findingLinkText()
	htmlBody := fmt.Sprintf(
		"<p>Hi %s,</p><p><strong>%s</strong> assigned a finding to you on the flow <strong>%s</strong>:</p>"+
			"<blockquote>%s</blockquote>",
		html.EscapeString(assigneeName), html.EscapeString(assignerName), html.EscapeString(flowName), html.EscapeString(findingTitle)) +
		s.findingLinkHTML()
	return s.mailer.Send(ctx, to, "A finding was assigned to you", text, htmlBody)
}

// SendFindingComment notifies the assignee that someone commented on their
// assigned finding. commentBody is user-controlled and HTML-escaped; long bodies
// are truncated to a 300-char preview. Links back like SendFindingAssigned.
func (s *Service) SendFindingComment(ctx context.Context, to, recipientName, commenterName, flowName, findingTitle, commentBody string) error {
	if s == nil {
		return nil
	}
	preview := previewOf(commentBody)
	text := fmt.Sprintf(
		"Hi %s,\n\n%s commented on a finding assigned to you on the flow %q:\n\n  %s\n",
		recipientName, commenterName, flowName, preview) + s.findingLinkText()
	htmlBody := fmt.Sprintf(
		"<p>Hi %s,</p><p><strong>%s</strong> commented on a finding assigned to you on the flow <strong>%s</strong>:</p>"+
			"<blockquote>%s</blockquote>",
		html.EscapeString(recipientName), html.EscapeString(commenterName), html.EscapeString(flowName), html.EscapeString(preview)) +
		s.findingLinkHTML()
	return s.mailer.Send(ctx, to, "New comment on your assigned finding", text, htmlBody)
}

// commentPreviewBytes bounds the comment excerpt embedded in a notification
// email.
const commentPreviewBytes = 300

// previewOf truncates a comment body to a preview, cutting on a RUNE boundary.
//
// The cut used to be a plain byte slice (s[:300]), which splits a multi-byte
// UTF-8 character whenever the boundary lands inside one — so a comment in any
// non-ASCII script could put a mangled byte sequence into the email body, which
// html.EscapeString passes straight through. Same class as the stream-scrubber
// and chat-resume fixes; this mirrors ai.truncateResult's approach.
func previewOf(s string) string {
	if len(s) <= commentPreviewBytes {
		return s
	}
	cut := commentPreviewBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
