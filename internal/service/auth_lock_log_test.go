package service

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/testutil"
)

// TestAuthenticate_LockWarningRedactsTheEmail: the warning logged when wrong passwords lock an account names the
// address redacted (audit.RedactEmail), as the other log lines do. It logged the address whole.
func TestAuthenticate_LockWarningRedactsTheEmail(t *testing.T) {
	logs := captureLogs(t)
	b := testutil.NewFakeBackend()
	u := seedLoginUser(t, b, "lock-warning", true)
	svc := NewAuthService(b, nil)
	for range failedLoginLockThreshold {
		svc.Authenticate(context.Background(), u.Email, "wrong")
	}
	out := logs.String()
	if !strings.Contains(out, "account locked") {
		t.Fatalf("no lock warning logged:\n%s", out)
	}
	if strings.Contains(out, u.Email) {
		t.Errorf("the lock warning names the address in full:\n%s", out)
	}
	if !strings.Contains(out, audit.RedactEmail(u.Email)) {
		t.Errorf("the lock warning does not name the redacted address %q:\n%s", audit.RedactEmail(u.Email), out)
	}
}
