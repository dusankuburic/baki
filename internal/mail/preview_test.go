package mail

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPreviewOf_CutsOnRuneBoundary covers the truncation used for the comment
// excerpt in a notification email. The cut was a plain byte slice, which splits
// a multi-byte UTF-8 character whenever the 300-byte boundary lands inside one —
// html.EscapeString then passes the broken bytes straight into the message body.
//
// The offsets are chosen so the boundary lands at every possible position
// within a multi-byte rune, which is the only way to be sure the snap-back
// handles 2-, 3- and 4-byte sequences rather than just the common case.
func TestPreviewOf_CutsOnRuneBoundary(t *testing.T) {
	for _, r := range []struct {
		name string
		ch   string
	}{
		{"2-byte (é)", "é"},
		{"3-byte (中)", "中"},
		{"4-byte (emoji)", "🙂"},
	} {
		t.Run(r.name, func(t *testing.T) {
			// Slide the rune across the cut point so every intra-rune offset is
			// exercised.
			for pad := commentPreviewBytes - len(r.ch) - 1; pad <= commentPreviewBytes+1; pad++ {
				if pad < 0 {
					continue
				}
				in := strings.Repeat("a", pad) + r.ch + strings.Repeat("b", commentPreviewBytes)
				got := previewOf(in)
				if !utf8.ValidString(got) {
					t.Fatalf("pad=%d: preview is not valid UTF-8: %q", pad, got)
				}
				if !strings.HasPrefix(in, strings.TrimSuffix(got, "…")) {
					t.Fatalf("pad=%d: preview is not a prefix of the input", pad)
				}
			}
		})
	}
}

// TestPreviewOf_ShortInputUnchanged keeps the truncation from firing (and
// appending an ellipsis) on bodies that fit.
func TestPreviewOf_ShortInputUnchanged(t *testing.T) {
	for _, in := range []string{"", "a short comment", strings.Repeat("x", commentPreviewBytes)} {
		if got := previewOf(in); got != in {
			t.Errorf("previewOf(%d bytes) modified an input that fits: %q", len(in), got)
		}
	}
}

// TestPreviewOf_LongInputTruncates confirms it still does its job.
func TestPreviewOf_LongInputTruncates(t *testing.T) {
	in := strings.Repeat("x", commentPreviewBytes+50)
	got := previewOf(in)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected an ellipsis on a truncated preview, got %q", got[len(got)-10:])
	}
	if len(got) > commentPreviewBytes+len("…") {
		t.Errorf("preview is %d bytes, want <= %d", len(got), commentPreviewBytes+len("…"))
	}
}
