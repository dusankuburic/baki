package service

import (
	"strings"
	"testing"
)

func TestTruncateForLog(t *testing.T) {
	if got := truncateForLog("short", 10); got != "short" {
		t.Errorf("under the cap must be untouched: %q", got)
	}
	long := strings.Repeat("a", 100)
	got := truncateForLog(long, 10)
	if !strings.HasPrefix(got, "aaaaaaaaaa…") || !strings.Contains(got, "truncated 90 bytes") {
		t.Errorf("truncation marker missing: %q", got)
	}
	// Never split a multi-byte rune.
	got = truncateForLog("ééééé", 3)
	if !strings.HasPrefix(got, "é…") {
		t.Errorf("cut inside a rune: %q", got)
	}
}
