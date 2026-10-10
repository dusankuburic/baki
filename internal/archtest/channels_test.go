package archtest

import (
	"strings"
	"testing"
)

// TestArch_APIDoesNotImportNotify: the notification channels' kinds, validation, redaction and deliveries belong to
// service.ChannelService (ADR 0021); a handler that reached into notify again would grow a second copy of them.
func TestArch_APIDoesNotImportNotify(t *testing.T) {
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/api/") && imports(f.ast, "pad-analyzer/internal/notify") {
			t.Errorf("%s imports internal/notify: go through service.ChannelService", f.path)
		}
	}
}
