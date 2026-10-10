package mail

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
)

// A configured PAD_EMAIL_FROM that does not parse is said once at boot, as an ERROR (every send fails with it); a
// valid one, bare or with a display name, says nothing. It does not stop the boot: the previous release sent it raw.
func TestNew_ReportsAnUnparseableSenderAtBoot(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for from, wantError := range map[string]bool{
		"noreply@example.com":                 false,
		"PAD Analyzer <noreply@example.com>":  false,
		"PAD Analyzer noreply@example.com":    true,
		"noreply@example.com\r\nBcc: x@y.com": true,
	} {
		logs.Reset()
		m := New(config.EmailConfig{SMTPHost: "smtp.example.com", SMTPPort: 587, From: from}, capability.NewSet(capability.Cloud))
		if !m.Enabled() {
			t.Fatalf("From %q: the SMTP mailer is not used", from)
		}
		if got := strings.Contains(logs.String(), "PAD_EMAIL_FROM is not an email address"); got != wantError {
			t.Errorf("From %q: boot error logged = %v, want %v (%s)", from, got, wantError, logs.String())
		}
	}
}
