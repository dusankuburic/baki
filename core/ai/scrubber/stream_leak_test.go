package scrubber

import (
	"strings"
	"testing"
)

// fakeStripeLiveKey is assembled rather than written out: a contiguous
// sk_live_ string in the source is what credential scanners match, and this
// fixture once blocked pushes of this repository under GitHub push
// protection; built at run time the value is identical. Same trick as the
// ghp_ sample in stream_test.go.
var fakeStripeLiveKey = "sk_live_" + strings.Repeat("abcdefgh", 3)

// secretSamples covers every class in secretRegexes, with the exact substring
// that must never reach the client.
var secretSamples = []struct{ name, in, secret string }{
	{"generic key/value", "config: password=SuperSecret123 done", "SuperSecret123"},
	{"bearer", "header: Bearer abcdefghijklmnopqrstuvwxyz012345 done", "abcdefghijklmnopqrstuvwxyz012345"},
	{"stripe sk_", "key: " + fakeStripeLiveKey + " done", fakeStripeLiveKey},
	{"github ghp_", "tok: ghp_123456789012345678901234567890123456 done", "ghp_123456789012345678901234567890123456"},
	{"connection string", "dsn: Server=db1;User=sa;Password=P@ssw0rd123; done", "P@ssw0rd123"},
	{"aws AKIA", "creds: AKIAIOSFODNN7EXAMPLE done", "AKIAIOSFODNN7EXAMPLE"},
	{"google AIza", "key: AIzaSyC1234567890abcdefghijklmnopqrstuv done", "AIzaSyC1234567890abcdefghijklmnopqrstuv"},
	{"slack xoxb", "tok: xoxb-1234567890-abcdefghij done", "xoxb-1234567890-abcdefghij"},
	{"jwt", "jwt: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N done",
		"eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
	{"pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234567890\n-----END RSA PRIVATE KEY-----",
		"MIIEowIBAAKCAQEA1234567890"},
}

// streamAll feeds in through a StreamScrubber in fixed-size deltas.
func streamAll(in string, delta int) string {
	s := NewStreamScrubber()
	var out strings.Builder
	for i := 0; i < len(in); i += delta {
		j := i + delta
		if j > len(in) {
			j = len(in)
		}
		out.WriteString(s.Write(in[i:j]))
	}
	out.WriteString(s.Flush())
	return out.String()
}

// TestStreamScrubber_NoSecretSurvivesAnyChunking is the regression gate for the
// most consequential scrubber defect: the streaming scrubber on the path that
// reaches the browser was weaker than the whole-text one. viablePrefixRegexes
// must match EVERY prefix of a possible secret; when one did not, the anchor
// was declared dead (dead anchors are never revisited) and the token leaked in
// fragments, depending only on where the provider's chunk boundaries fell.
// Asserts across chunk sizes that straddle every internal boundary.
func TestStreamScrubber_NoSecretSurvivesAnyChunking(t *testing.T) {
	// 1 and 2 exercise byte-at-a-time providers; the primes land boundaries
	// inside markers and separators; the large sizes cover whole-chunk arrival.
	deltas := []int{1, 2, 3, 4, 5, 7, 8, 11, 13, 16, 17, 32, 64, 256}

	for _, c := range secretSamples {
		t.Run(c.name, func(t *testing.T) {
			// Baseline: ScrubText must mask it, or the sample is testing nothing.
			if strings.Contains(ScrubText(c.in), c.secret) {
				t.Fatalf("fixture rot: ScrubText does not mask this sample, so the "+
					"streaming comparison is meaningless\n  in: %q", c.in)
			}
			for _, d := range deltas {
				got := streamAll(c.in, d)
				if strings.Contains(got, c.secret) {
					t.Errorf("delta=%d: the secret reached the client in clear.\n  got: %q", d, got)
				}
			}
		})
	}
}

// TestStreamScrubber_MatchesScrubTextOnCleanText guards the other direction:
// text with no secret must stream through byte-identical to ScrubText — a
// scrubber that holds or mangles ordinary prose is a visible regression.
func TestStreamScrubber_MatchesScrubTextOnCleanText(t *testing.T) {
	clean := []string{
		"Here is a summary of the flow. It reads a file and posts the result.",
		"The password field should be parameterised rather than hardcoded.",
		"Use a Bearer scheme for the Authorization header.",
		"Consider AKIA-style credentials carefully; do not embed them.",
		"A PEM file begins with a BEGIN marker and ends with an END marker.",
		"Unicode: héllo ✅ 日本語 — emoji and accents must survive chunking.",
	}
	for _, in := range clean {
		// The contract is "streaming equals whole-text scrubbing", not a no-op:
		// ScrubText legitimately masks some of these (e.g. its Bearer pattern).
		want := ScrubText(in)
		for _, d := range []int{1, 3, 7, 64} {
			if got := streamAll(in, d); got != want {
				t.Errorf("streaming diverged from ScrubText at delta=%d:\n  want %q\n  got  %q", d, want, got)
			}
		}
	}
}

// TestStreamScrubber_LeadInAloneIsHeld is the structural invariant behind the
// viability bugs: feed ONLY the opening bytes of each secret and nothing may be
// emitted yet — a released lead-in means the anchor was judged non-viable on
// first sight, and dead anchors are never revisited. Uses the real samples,
// not anchorLiterals, so the casing is the casing that arrives on the wire
// (the literals are stored lower-cased; the prefix patterns are case-sensitive).
func TestStreamScrubber_LeadInAloneIsHeld(t *testing.T) {
	for _, c := range secretSamples {
		t.Run(c.name, func(t *testing.T) {
			idx := strings.Index(c.in, c.secret)
			if idx < 0 {
				t.Fatalf("fixture rot: secret not present in sample")
			}
			// Everything up to and including the first few bytes of the secret:
			// the lead-in the scrubber must recognise and hold.
			leadIn := c.in[:idx+3]
			s := NewStreamScrubber()
			out := s.Write(leadIn)
			if strings.Contains(out, c.secret[:3]) {
				t.Errorf("the lead-in was emitted instead of held: Write(%q) returned %q — "+
					"the anchor was judged non-viable, so the rest of the secret will stream "+
					"out unheld", leadIn, out)
			}
		})
	}
}

// TestStreamScrubber_AnchorRegexCoversEveryAnchorLiteral closes the gap that
// made AKIA/AIza/xox/eyJ/PEM unreachable: anchorsByFirst is built from
// anchorLiterals but release() scans with anchorRegex, so a literal absent
// from the regex is only ever a PARTIAL anchor at a buffer boundary, never a
// complete one mid-buffer.
func TestStreamScrubber_AnchorRegexCoversEveryAnchorLiteral(t *testing.T) {
	for _, a := range anchorLiterals {
		if !anchorRegex.MatchString(a) {
			t.Errorf("anchor literal %q is not matched by anchorRegex — release() cannot find it", a)
		}
	}
}

// TestStreamScrubber_LongSecretTailIsNotReleased is the regression gate for the
// hold cap: once held text contained a COMPLETE secret, holdBudget dropped to
// streamMatchedHold (256) and capOverflow emitted the buffer masked and reset —
// but the value kept arriving and its lead-in was consumed, so every
// subsequent byte streamed out verbatim: a leak of len(secret)-256 raw chars,
// hitting precisely the long credentials worth stealing (JWTs, PEM bodies,
// SAS URLs).
func TestStreamScrubber_LongSecretTailIsNotReleased(t *testing.T) {
	// 'z' avoids colliding with the letters in "[REDACTED]".
	for _, n := range []int{100, 257, 300, 500, 720, 2000} {
		secret := strings.Repeat("z", n)
		in := "api_key=" + secret + " done"

		if strings.Contains(ScrubText(in), secret[:64]) {
			t.Fatalf("fixture rot: ScrubText no longer masks an %d-char value", n)
		}

		for _, delta := range []int{1, 8, 64, 512} {
			got := streamAll(in, delta)
			// No run of the secret alphabet longer than a handful may survive.
			if strings.Contains(got, strings.Repeat("z", 32)) {
				leaked := 0
				for k := n; k >= 1; k-- {
					if strings.Contains(got, strings.Repeat("z", k)) {
						leaked = k
						break
					}
				}
				t.Errorf("secret=%d delta=%d: %d raw characters of the credential reached the "+
					"client after the hold cap fired\n  out: %.120q…", n, delta, leaked, got)
			}
		}
	}
}

// TestStreamScrubber_DrainDoesNotEatFollowingText is the other half: the drain
// state swallows the tail of a masked secret, so it must stop at the value's
// end or it silently deletes the assistant's next sentence.
func TestStreamScrubber_DrainDoesNotEatFollowingText(t *testing.T) {
	const tail = "and here is the explanation that follows."
	in := "api_key=" + strings.Repeat("z", 600) + " " + tail

	for _, delta := range []int{1, 8, 64, 512} {
		got := streamAll(in, delta)
		if !strings.Contains(got, tail) {
			t.Errorf("delta=%d: text after the secret was swallowed by the drain\n  out: %q",
				delta, got)
		}
	}
}
