package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// stalledMailer holds every send until release closes: a request that waited on SMTP would hang with it.
type stalledMailer struct {
	release chan struct{}
	started chan string
}

func (m *stalledMailer) Enabled() bool { return true }
func (m *stalledMailer) Send(ctx context.Context, _ string, msg mailer.Message) error {
	m.started <- msg.Template
	select {
	case <-m.release:
	case <-ctx.Done():
	}
	return nil
}

// TestAuthEmails_AnswerBeforeTheSend is the B7 regression: forgot-password and resend-verification used to send
// inline, so only an existing account paid the SMTP round trip and the response time told which accounts exist.
// With the relay stalled, both answer at once for a known and an unknown address alike, and only the known one
// hands an email to the mail runner.
func TestAuthEmails_AnswerBeforeTheSend(t *testing.T) {
	store := testutil.NewFakeBackend()
	rt := newTestRouter(store, true)
	seedUserWithPassword(t, store, "u-known", "known@example.com", "OldPassw0rd!")
	stalled := &stalledMailer{release: make(chan struct{}), started: make(chan string, 8)}
	email := mailer.NewServiceWithMailer(stalled, "https://app.example.com")
	moduleOf[*AuthHandler](t, rt).authSvc = service.NewAuthService(store, email)
	defer func() {
		close(stalled.release)
		_ = email.Drain(context.Background())
	}()

	post := func(path, address string) <-chan int {
		code := make(chan int, 1)
		go func() {
			body, _ := json.Marshal(map[string]string{"email": address})
			rr := httptest.NewRecorder()
			rt.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
			code <- rr.Code
		}()
		return code
	}
	for _, tc := range []struct {
		path, address, template string
	}{
		{"/api/auth/forgot-password", "known@example.com", "password_reset"},
		{"/api/auth/forgot-password", "unknown@example.com", ""},
		{"/api/auth/resend-verification", "known@example.com", "email_verification"},
		{"/api/auth/resend-verification", "unknown@example.com", ""},
	} {
		select {
		case code := <-post(tc.path, tc.address):
			if code != http.StatusOK {
				t.Fatalf("%s(%s) = %d, want 200", tc.path, tc.address, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s(%s) waited on the stalled SMTP send", tc.path, tc.address)
		}
		if tc.template == "" {
			continue
		}
		select {
		case got := <-stalled.started:
			if got != tc.template {
				t.Fatalf("%s(%s) sent %q, want %q", tc.path, tc.address, got, tc.template)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s(%s) never handed its email to the mail runner", tc.path, tc.address)
		}
	}
	select {
	case got := <-stalled.started:
		t.Fatalf("an unknown address got a %q email", got)
	case <-time.After(50 * time.Millisecond):
	}
}
