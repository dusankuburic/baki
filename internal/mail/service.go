package mail

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"pad-analyzer/internal/capability"
	"pad-core/logger"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/metrics"
)

// Service renders the registered templates and sends them on one bounded runner, so no caller ever waits on SMTP:
// a request answers before the relay is even dialled, and shutdown drains the sends in flight (lifecycle.DrainAll).
// NewService always returns a usable value, with the log-only mailer when SMTP is not configured.
type Service struct {
	mailer Mailer
	env    Env
	// runner sends every email; bulk caps the non-sensitive share of it (see reservedSensitiveSends).
	runner *lifecycle.Bounded
	bulk   chan struct{}
}

const (
	// maxConcurrentSends caps the SMTP sessions in flight across every email the product sends.
	maxConcurrentSends = 32
	// reservedSensitiveSends of those slots only Sensitive mail (reset, verification, invite) may take, so a burst
	// of finding notifications against a slow relay cannot crowd out a password reset.
	reservedSensitiveSends = 8
	// sendTimeout bounds one send: the 10 s dial and handshake plus smtpOpTimeout for the exchange.
	sendTimeout = 45 * time.Second
	// prepareTimeout bounds an EnqueueJob's prepare step (a token write).
	prepareTimeout = 10 * time.Second
	// runnerName is the runner's metric and log label.
	runnerName = "mail"
)

// ErrNotQueued is returned by Enqueue and EnqueueJob when the mail runner is saturated or shutting down, and by
// SendAlert when its context ends before a slot frees: the email was dropped (and logged and counted as a dropped
// background task).
var ErrNotQueued = errors.New("mail: not queued (the mail runner is saturated or shutting down)")

// NewService builds the email service from config, choosing an SMTP or log-only
// mailer automatically. caps is passed through so the log-only fallback never
// writes token-bearing bodies to the log in the cloud profile.
func NewService(cfg config.EmailConfig, caps capability.Set) *Service {
	return NewServiceWithMailer(New(cfg, caps), cfg.AppBaseURL)
}

// NewServiceWithMailer builds the email service around an explicit Mailer
// (tests capture the rendered messages this way); baseURL as in NewService.
func NewServiceWithMailer(m Mailer, baseURL string) *Service {
	return &Service{
		mailer: m,
		env:    Env{BaseURL: strings.TrimRight(baseURL, "/")},
		runner: lifecycle.NewBounded(runnerName, maxConcurrentSends),
		bulk:   make(chan struct{}, maxConcurrentSends-reservedSensitiveSends),
	}
}

// Enabled reports whether real SMTP delivery is configured (vs log-only).
func (s *Service) Enabled() bool { return s != nil && s.mailer != nil && s.mailer.Enabled() }

// Enqueue renders m for to and hands the send to the mail runner; it never waits on SMTP. It returns an error when
// the address is invalid, the template fails to render, or the runner refuses the send (ErrNotQueued); a failed
// send is reported by the runner (errreport location "mail.send", with the template's ID, never the body). Inside
// a transaction, call it from a txhook.Register hook: an email must not describe a write that is then rolled back.
// ctx only lends its logger to a refusal. Nil-safe.
func (s *Service) Enqueue(ctx context.Context, to string, m Mail) error {
	if s == nil {
		return nil
	}
	if _, err := parseAddress(to); err != nil {
		return err
	}
	msg, err := m.render(s.env)
	if err != nil {
		return err
	}
	return s.enqueue(ctx, to, msg, false)
}

// EnqueueJob hands an email whose making must stay off the request too to the mail runner, as one task on the
// share reserved for Sensitive mail: prepare (writing a one-time token, say) runs there with a context of its own,
// then the Mail it returns is rendered and sent. The caller pays neither the write nor SMTP (B7). An error from
// prepare sends nothing and is reported like a failed send. It returns an address error, or ErrNotQueued when the
// runner refuses. Nil-safe.
func (s *Service) EnqueueJob(ctx context.Context, to string, prepare func(context.Context) (Mail, error)) error {
	if s == nil {
		return nil
	}
	if _, err := parseAddress(to); err != nil {
		return err
	}
	log := logger.FromContext(ctx)
	started := s.runner.Go(func() {
		msg, err := s.prepare(prepare)
		if err != nil {
			log.Warn("email not prepared", "err", err)
			errreport.CaptureError(context.Background(), "mail.prepare", err, nil)
			return
		}
		s.send(log, to, msg)
	})
	if !started {
		return ErrNotQueued
	}
	return nil
}

// prepare runs an EnqueueJob's prepare step, bounded by prepareTimeout, and renders the Mail it returns.
func (s *Service) prepare(prepare func(context.Context) (Mail, error)) (Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), prepareTimeout)
	defer cancel()
	m, err := prepare(ctx)
	if err != nil {
		return Message{}, err
	}
	return m.render(s.env)
}

// SendAlert queues a governance alert rendered by the caller (RenderGovernanceAlert) on the mail runner. It
// satisfies notify.EmailSender. Unlike Enqueue it waits for a slot while ctx lives, so a sweep's alerts are paced by
// the relay instead of dropped (notify bounds ctx with its delivery timeout); it returns ErrNotQueued when ctx ends
// first or the runner is draining. Under WithoutWaiting it queues like Enqueue. A failed send is reported by the
// runner, not returned.
func (s *Service) SendAlert(ctx context.Context, to, subject, plainBody, htmlBody string) error {
	if s == nil {
		return nil
	}
	if _, err := parseAddress(to); err != nil {
		return err
	}
	noWait, _ := ctx.Value(noWaitKey{}).(bool)
	return s.enqueue(ctx, to, Message{
		Template: GovernanceAlert.ID(),
		Locale:   DefaultLocale,
		Subject:  oneLine(subject),
		Text:     plainBody,
		HTML:     htmlBody,
	}, !noWait)
}

type noWaitKey struct{}

// WithoutWaiting marks ctx so that SendAlert drops the alert (ErrNotQueued) when the notification share is full, as
// Enqueue does, instead of waiting for a slot: for an alert raised once per request (analysis_complete), not a
// sweep's.
func WithoutWaiting(ctx context.Context) context.Context {
	return context.WithValue(ctx, noWaitKey{}, true)
}

// Drain stops the runner and waits for the sends in flight (or ctx). Shutdown drains every runner through
// lifecycle.DrainAll; tests call this to wait for their sends.
func (s *Service) Drain(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.runner.Drain(ctx)
}

// enqueue hands msg's send to the runner. Non-sensitive mail takes a slot of the notification share first. With
// wait, a full share or runner is waited for while ctx lives; without it, the send is dropped.
func (s *Service) enqueue(ctx context.Context, to string, msg Message, wait bool) error {
	log := logger.FromContext(ctx)
	release := func() {}
	if !msg.Sensitive {
		select {
		case s.bulk <- struct{}{}:
		default:
			if !wait {
				metrics.RecordBackgroundDropped(runnerName, "saturated")
				log.Warn("email dropped: the mail runner's notification share is full", "template", msg.Template)
				return ErrNotQueued
			}
			select {
			case s.bulk <- struct{}{}:
			case <-ctx.Done():
				metrics.RecordBackgroundDropped(runnerName, "cancelled")
				log.Warn("email dropped: no slot of the mail runner's notification share freed in time", "template", msg.Template)
				return ErrNotQueued
			}
		}
		release = func() { <-s.bulk }
	}
	task := func() {
		defer release()
		s.send(log, to, msg)
	}
	var started bool
	if wait {
		started = s.runner.GoWait(ctx, task)
	} else {
		started = s.runner.Go(task)
	}
	if !started {
		release()
		return ErrNotQueued
	}
	return nil
}

// send runs one SMTP session, bounded by sendTimeout and detached from any request. A failure is logged in full
// with log, and reported to the error sink as its phase and reply code only (reportable): a relay's reply text can
// name the recipient.
func (s *Service) send(log *slog.Logger, to string, msg Message) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if err := s.mailer.Send(ctx, to, msg); err != nil {
		log.Warn("email send failed", "template", msg.Template, "err", err)
		errreport.CaptureError(ctx, "mail.send", reportable(err), errreport.Attrs{"template": msg.Template})
	}
}
