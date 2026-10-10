package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	mailer "pad-analyzer/internal/mail"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// blockingMailer holds every send until release closes and reports what it was given.
type blockingMailer struct {
	release chan struct{}
	sent    chan mailer.Message
}

func (m *blockingMailer) Enabled() bool { return true }
func (m *blockingMailer) Send(ctx context.Context, _ string, msg mailer.Message) error {
	select {
	case <-m.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	m.sent <- msg
	return nil
}

// gatedTokenStore holds every token write (create, invalidate) until release closes, counting the writes started.
type gatedTokenStore struct {
	*testutil.FakeBackend
	release chan struct{}
	writes  atomic.Int32
}

func (s *gatedTokenStore) wait(ctx context.Context) error {
	s.writes.Add(1)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *gatedTokenStore) CreateUserToken(ctx context.Context, t *storageif.UserToken) error {
	if err := s.wait(ctx); err != nil {
		return err
	}
	return s.FakeBackend.CreateUserToken(ctx, t)
}

func (s *gatedTokenStore) InvalidateUserTokens(ctx context.Context, userID string, purposes ...string) error {
	if err := s.wait(ctx); err != nil {
		return err
	}
	return s.FakeBackend.InvalidateUserTokens(ctx, userID, purposes...)
}

// TestTokenEmails_RequestReturnsBeforeTheTokenWrite is notify-3, the rest of B7: forgot-password and
// resend-verification wrote the token on the request path for a known address only, so a slow write still told
// which accounts exist. With the store's token writes held, both return for a known address before any write
// completes; the writes and the email follow on the mail runner. An unknown address writes and sends nothing.
func TestTokenEmails_RequestReturnsBeforeTheTokenWrite(t *testing.T) {
	store := &gatedTokenStore{FakeBackend: testutil.NewFakeBackend(), release: make(chan struct{})}
	if err := store.CreateUser(context.Background(), &storageif.User{ID: "u1", Email: "known@example.com", Role: auth.RoleMember}); err != nil {
		t.Fatal(err)
	}
	m := &blockingMailer{release: make(chan struct{}), sent: make(chan mailer.Message, 4)}
	close(m.release)
	email := mailer.NewServiceWithMailer(m, "https://app.example.com")
	svc := NewAuthService(store, email)

	calls := []struct {
		name string
		call func(address string) (*storageif.User, error)
	}{
		{"forgot-password", func(a string) (*storageif.User, error) { return svc.RequestPasswordReset(context.Background(), a) }},
		{"resend-verification", func(a string) (*storageif.User, error) { return svc.ResendVerification(context.Background(), a) }},
	}
	for _, c := range calls {
		for _, address := range []string{"known@example.com", "unknown@example.com"} {
			done := make(chan *storageif.User, 1)
			go func() {
				user, err := c.call(address)
				if err != nil {
					t.Errorf("%s(%s): %v", c.name, address, err)
				}
				done <- user
			}()
			select {
			case user := <-done:
				if (user != nil) != (address == "known@example.com") {
					t.Errorf("%s(%s) = %v", c.name, address, user)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s(%s) waited on the held token write", c.name, address)
			}
		}
	}
	if len(store.UserTokens) != 0 {
		t.Fatalf("a token was stored while the writes were held: %v", store.UserTokens)
	}
	close(store.release)
	if err := email.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reset: one write; resend: the invalidation and the write. The unknown address: none.
	if n := store.writes.Load(); n != 3 {
		t.Errorf("%d token writes, want 3", n)
	}
	if len(store.UserTokens) != 2 || len(m.sent) != 2 {
		t.Fatalf("%d tokens stored and %d emails sent, want 2 and 2 (the known address only)", len(store.UserTokens), len(m.sent))
	}
	for range 2 {
		msg := <-m.sent
		if msg.Template != "password_reset" && msg.Template != "email_verification" {
			t.Errorf("sent %q", msg.Template)
		}
	}
}

// TestRequestPasswordReset_QueuesTheEmail: the reset returns with the token stored while the relay is still
// stalled; the email (carrying the stored token's link) goes out once the relay answers. An unknown address
// returns the same way and sends nothing.
func TestRequestPasswordReset_QueuesTheEmail(t *testing.T) {
	store := testutil.NewFakeBackend()
	if err := store.CreateUser(context.Background(), &storageif.User{ID: "u1", Email: "known@example.com", Role: auth.RoleMember}); err != nil {
		t.Fatal(err)
	}
	m := &blockingMailer{release: make(chan struct{}), sent: make(chan mailer.Message, 2)}
	email := mailer.NewServiceWithMailer(m, "https://app.example.com")
	svc := NewAuthService(store, email)

	for _, address := range []string{"known@example.com", "unknown@example.com"} {
		done := make(chan error, 1)
		go func() {
			_, err := svc.RequestPasswordReset(context.Background(), address)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RequestPasswordReset(%s): %v", address, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("RequestPasswordReset(%s) waited on the stalled relay", address)
		}
	}
	close(m.release)
	if err := email.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.sent) != 1 {
		t.Fatalf("%d emails sent, want 1 (the known address only)", len(m.sent))
	}
	msg := <-m.sent
	const prefix = "https://app.example.com/#resetPassword="
	i := strings.Index(msg.Text, prefix)
	if msg.Template != "password_reset" || i < 0 {
		t.Fatalf("sent %+v", msg)
	}
	raw := strings.Fields(msg.Text[i+len(prefix):])[0]
	if _, err := svc.ResetPassword(context.Background(), raw, "BrandNewPassw0rd!"); err != nil {
		t.Fatalf("the emailed token does not redeem: %v", err)
	}
}
