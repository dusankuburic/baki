package mail

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/lifecycle"
)

// gateMailer blocks every Send until release is closed, recording what arrives.
type gateMailer struct {
	release chan struct{}
	started atomic.Int32
	mu      sync.Mutex
	sent    []Message
	err     error
}

func newGateMailer() *gateMailer { return &gateMailer{release: make(chan struct{})} }

func (g *gateMailer) Enabled() bool { return true }

func (g *gateMailer) Send(ctx context.Context, _ string, msg Message) error {
	g.started.Add(1)
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent = append(g.sent, msg)
	return g.err
}

func (g *gateMailer) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sent)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

var (
	resetMail   = PasswordReset.With(PasswordResetData{Token: "T"})
	commentMail = FindingComment.With(FindingCommentData{RecipientName: "Una", CommenterName: "Ari", FindingTitle: "t", Comment: "c"})
)

// TestEnqueue_NeverWaitsOnTheMailer: Enqueue returns while the send is still blocked, and the send completes on the
// runner once the relay answers.
func TestEnqueue_NeverWaitsOnTheMailer(t *testing.T) {
	g := newGateMailer()
	svc := NewServiceWithMailer(g, "https://app.example.com/")
	done := make(chan error, 1)
	go func() { done <- svc.Enqueue(context.Background(), "una@example.com", resetMail) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue waited on the blocked mailer")
	}
	waitFor(t, "the send to start", func() bool { return g.started.Load() == 1 })
	if g.count() != 0 {
		t.Fatal("sent before the relay answered")
	}
	close(g.release)
	if err := svc.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.count() != 1 || g.sent[0].Template != "password_reset" || g.sent[0].Text == "" {
		t.Fatalf("sent = %+v", g.sent)
	}
}

// TestEnqueue_ReservesSlotsForSensitiveMail: notifications fill at most their share; a password reset still gets a
// slot, and only a full runner refuses it.
func TestEnqueue_ReservesSlotsForSensitiveMail(t *testing.T) {
	g := newGateMailer()
	svc := NewServiceWithMailer(g, "")
	defer func() {
		close(g.release)
		_ = svc.Drain(context.Background())
	}()
	ctx := context.Background()
	bulk := maxConcurrentSends - reservedSensitiveSends
	for i := 0; i < bulk; i++ {
		if err := svc.Enqueue(ctx, "una@example.com", commentMail); err != nil {
			t.Fatalf("notification %d: %v", i, err)
		}
	}
	if err := svc.Enqueue(ctx, "una@example.com", commentMail); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("a notification past its share = %v, want ErrNotQueued", err)
	}
	// An alert waits for the share while its context lives (TestSendAlert_WaitsForASlotInsteadOfDropping).
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := svc.SendAlert(short, "ops@example.com", "s", "t", ""); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("an alert past the notification share = %v, want ErrNotQueued once its context ends", err)
	}
	for i := 0; i < reservedSensitiveSends; i++ {
		if err := svc.Enqueue(ctx, "una@example.com", resetMail); err != nil {
			t.Fatalf("reset %d with notifications saturated: %v", i, err)
		}
	}
	if err := svc.Enqueue(ctx, "una@example.com", resetMail); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("a reset on a full runner = %v, want ErrNotQueued", err)
	}
	waitFor(t, "every slot to be busy", func() bool { return int(g.started.Load()) == maxConcurrentSends })
}

// TestEnqueue_FailingAndPanickingSendsFreeTheirSlots: a relay error or a panic in the mailer is logged and the
// slot (and the notification share) comes back.
func TestEnqueue_FailingAndPanickingSendsFreeTheirSlots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mailer Mailer
	}{
		{"error", &funcMailer{send: func() error { return errors.New("550 relay says no") }}},
		{"panic", &funcMailer{send: func() error { panic("boom") }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewServiceWithMailer(tc.mailer, "")
			for i := 0; i <= maxConcurrentSends; i++ {
				if err := svc.Enqueue(context.Background(), "una@example.com", commentMail); err != nil {
					t.Fatalf("send %d: %v", i, err)
				}
				waitFor(t, "the slot to free", func() bool { return svc.runner.Running() == 0 && len(svc.bulk) == 0 })
			}
			if err := svc.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type funcMailer struct{ send func() error }

func (f *funcMailer) Enabled() bool                               { return true }
func (f *funcMailer) Send(context.Context, string, Message) error { return f.send() }

// TestDrain_WaitsThenRefuses: shutdown waits for the sends in flight; afterwards nothing is queued.
func TestDrain_WaitsThenRefuses(t *testing.T) {
	g := newGateMailer()
	svc := NewServiceWithMailer(g, "")
	if err := svc.Enqueue(context.Background(), "una@example.com", resetMail); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the send to start", func() bool { return g.started.Load() == 1 })
	drained := make(chan error, 1)
	go func() { drained <- svc.Drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) with a send in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(g.release)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if g.count() != 1 {
		t.Fatal("the in-flight send was lost")
	}
	if err := svc.Enqueue(context.Background(), "una@example.com", resetMail); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("Enqueue after Drain = %v, want ErrNotQueued", err)
	}
}

// TestDrain_GivesUpAtTheDeadline: a send stuck on the relay does not hold shutdown past its context.
func TestDrain_GivesUpAtTheDeadline(t *testing.T) {
	g := newGateMailer()
	defer close(g.release)
	svc := NewServiceWithMailer(g, "")
	if err := svc.Enqueue(context.Background(), "una@example.com", resetMail); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := svc.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v, want the deadline", err)
	}
}

// TestEnqueue_RefusesBeforeQueueing: an invalid recipient or a Mail without a template is an error at Enqueue, and
// nothing reaches the mailer.
func TestEnqueue_RefusesBeforeQueueing(t *testing.T) {
	g := newGateMailer()
	close(g.release)
	svc := NewServiceWithMailer(g, "")
	for _, to := range []string{"", "una@example.com\r\nBcc: evil@example.com", "a@x.com, b@y.com"} {
		if err := svc.Enqueue(context.Background(), to, resetMail); err == nil {
			t.Errorf("Enqueue(%q) queued", to)
		}
		if err := svc.SendAlert(context.Background(), to, "s", "t", ""); err == nil {
			t.Errorf("SendAlert(%q) queued", to)
		}
	}
	if err := svc.Enqueue(context.Background(), "una@example.com", Mail{}); err == nil {
		t.Error("an empty Mail queued")
	}
	if err := svc.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.started.Load() != 0 {
		t.Errorf("%d sends reached the mailer", g.started.Load())
	}
}

// TestSendAlert_GoesThroughTheRunner: the notify email kind's sends are queued like every other email, tagged with
// the governance_alert template and a one-line subject.
func TestSendAlert_GoesThroughTheRunner(t *testing.T) {
	g := newGateMailer()
	svc := NewServiceWithMailer(g, "")
	msg, err := RenderGovernanceAlert(GovernanceAlertData{Event: "drift", Title: "t", At: goldenAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SendAlert(context.Background(), "ops@example.com", msg.Subject+"\r\nX: y", msg.Text, msg.HTML); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the send to start", func() bool { return g.started.Load() == 1 })
	close(g.release)
	if err := svc.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := g.sent[0]; got.Template != "governance_alert" || got.Subject != "[DRIFT] t X: y" || got.HTML != msg.HTML {
		t.Fatalf("sent = %+v", got)
	}
}

// TestService_NilIsANoOp: callers holding a nil *Service (tests, unwired deployments) neither panic nor send.
func TestService_NilIsANoOp(t *testing.T) {
	var svc *Service
	if svc.Enabled() || svc.Enqueue(context.Background(), "a@b.c", resetMail) != nil || svc.SendAlert(context.Background(), "a@b.c", "s", "t", "") != nil || svc.Drain(context.Background()) != nil {
		t.Fatal("a nil Service must be an inert no-op")
	}
}

// TestSendAlert_WaitsForASlotInsteadOfDropping is notify-2: the scanner hands its alerts to the deployment email
// channel one after another (Dispatch), each under notify's 10 s delivery timeout. Queued on a full notification
// share, every alert past it was dropped; it now waits for a slot, so the relay paces the sweep again.
func TestSendAlert_WaitsForASlotInsteadOfDropping(t *testing.T) {
	var sent atomic.Int32
	relay := &funcMailer{send: func() error {
		time.Sleep(50*time.Millisecond + rand.N(50*time.Millisecond))
		sent.Add(1)
		return nil
	}}
	// A small runner: 4 sessions, 2 of them for notifications.
	svc := &Service{mailer: relay, runner: (&lifecycle.Group{}).NewBounded("mail_test", 4), bulk: make(chan struct{}, 2)}
	const alerts = 40
	for i := 0; i < alerts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := svc.SendAlert(ctx, "ops@example.com", "s", "t", "")
		cancel()
		if err != nil {
			t.Fatalf("alert %d: %v", i, err)
		}
	}
	if err := svc.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := sent.Load(); n != alerts {
		t.Fatalf("%d of %d alerts sent", n, alerts)
	}
}

// TestSendAlert_WithoutWaitingDropsAtOnce: an alert raised per request (analysis_complete) keeps Enqueue's
// behaviour, dropped when the notification share is full rather than waiting for it.
func TestSendAlert_WithoutWaitingDropsAtOnce(t *testing.T) {
	g := newGateMailer()
	svc := &Service{mailer: g, runner: (&lifecycle.Group{}).NewBounded("mail_test", 2), bulk: make(chan struct{}, 1)}
	defer func() {
		close(g.release)
		_ = svc.Drain(context.Background())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := svc.SendAlert(ctx, "ops@example.com", "s", "t", ""); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := svc.SendAlert(WithoutWaiting(ctx), "ops@example.com", "s", "t", ""); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("SendAlert without waiting on a full share = %v, want ErrNotQueued", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("SendAlert without waiting took %v", d)
	}
}

// captureSink records what reaches the error sink.
type captureSink struct {
	mu   sync.Mutex
	errs []string
}

func (c *captureSink) CaptureError(_ context.Context, err error, _ errreport.Attrs) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, err.Error())
}

func (c *captureSink) CapturePanic(context.Context, any, []byte, errreport.Attrs) {}

func (c *captureSink) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.errs, "\n")
}

// refusingRelay accepts one SMTP session (no TLS) and answers RCPT TO the way a relay refuses an unknown mailbox,
// naming it.
func refusingRelay(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		br, bw := bufio.NewReader(conn), bufio.NewWriter(conn)
		write := func(s string) { _, _ = bw.WriteString(s); _ = bw.Flush() }
		write("220 relay.example ESMTP\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				write("250 relay.example\r\n")
			case strings.HasPrefix(cmd, "RCPT TO:"):
				write("550 5.1.1 <una@example.com>: Recipient address rejected: User unknown\r\n")
			case strings.HasPrefix(cmd, "QUIT"):
				write("221 bye\r\n")
				return
			default:
				write("250 ok\r\n")
			}
		}
	}()
	return ln.Addr().String()
}

// TestSendFailure_ReportsPhaseAndCodeOnly is notify-6: a relay's refusal names the recipient, and the whole SMTP
// text went to the error sink. The sink now gets the phase and the reply code; the local log keeps the full text.
func TestSendFailure_ReportsPhaseAndCodeOnly(t *testing.T) {
	conn, err := net.Dial("tcp", refusingRelay(t))
	if err != nil {
		t.Fatal(err)
	}
	c, err := smtp.NewClient(conn, "relay.example")
	if err != nil {
		t.Fatal(err)
	}
	relayErr := (&smtpMailer{}).deliver(c, nil, envelope{from: "from@example.com", to: "una@example.com", data: []byte("x")})
	_ = c.Close()
	if relayErr == nil || !strings.Contains(relayErr.Error(), "una@example.com") {
		t.Fatalf("deliver = %v, want the relay's refusal naming the recipient", relayErr)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	sink := &captureSink{}
	errreport.Register(sink)
	defer errreport.Register(nil)

	svc := NewServiceWithMailer(&funcMailer{send: func() error { return relayErr }}, "")
	if err := svc.Enqueue(context.Background(), "una@example.com", commentMail); err != nil {
		t.Fatal(err)
	}
	if err := svc.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	reported := sink.text()
	if reported != "mail: rcpt: 550" {
		t.Errorf("reported %q, want %q", reported, "mail: rcpt: 550")
	}
	if strings.Contains(reported, "@") {
		t.Errorf("the error report names an address: %s", reported)
	}
	if !strings.Contains(logs.String(), "Recipient address rejected") {
		t.Errorf("the local log lost the relay's answer:\n%s", logs.String())
	}
}

// TestReportable: what the error sink gets of each kind of failure.
func TestReportable(t *testing.T) {
	timeout := &net.OpError{Op: "dial", Net: "tcp", Err: &timeoutErr{}}
	for err, want := range map[error]string{
		&smtpError{phase: "dial", where: "smtp.example:587", err: timeout}:                       "mail: dial: timeout",
		&smtpError{phase: "starttls", where: "smtp.example:587", err: errors.New("not offered")}: "mail: starttls: failed",
		&smtpError{phase: "data", err: context.DeadlineExceeded}:                                 "mail: data: timeout",
		errors.New("550 <una@example.com> no"):                                                   "mail: send: failed",
	} {
		if got := reportable(err).Error(); got != want {
			t.Errorf("reportable(%v) = %q, want %q", err, got, want)
		}
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "i/o timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

// TestEnqueueJob_PreparesOnTheRunner: the caller returns while prepare is blocked; the Mail prepare returns is sent
// afterwards, and a failing prepare sends nothing.
func TestEnqueueJob_PreparesOnTheRunner(t *testing.T) {
	g := newGateMailer()
	close(g.release)
	svc := NewServiceWithMailer(g, "https://app.example.com")
	unblock := make(chan struct{})
	queued := make(chan error, 1)
	go func() {
		queued <- svc.EnqueueJob(context.Background(), "una@example.com", func(ctx context.Context) (Mail, error) {
			select {
			case <-unblock:
			case <-ctx.Done():
				return Mail{}, ctx.Err()
			}
			return resetMail, nil
		})
	}()
	select {
	case err := <-queued:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnqueueJob waited on prepare")
	}
	if g.started.Load() != 0 {
		t.Fatal("sent before prepare returned")
	}
	close(unblock)
	if err := svc.EnqueueJob(context.Background(), "una@example.com", func(context.Context) (Mail, error) {
		return Mail{}, errors.New("token write failed")
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnqueueJob(context.Background(), "una@example.com\r\nBcc: x@y.z", func(context.Context) (Mail, error) {
		t.Error("prepare ran for a refused address")
		return resetMail, nil
	}); err == nil {
		t.Error("EnqueueJob queued a bad address")
	}
	if err := svc.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.count() != 1 || g.sent[0].Template != "password_reset" {
		t.Fatalf("sent = %+v, want the one prepared reset", g.sent)
	}
}
