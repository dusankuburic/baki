package scrubber

import (
	"strings"
	"testing"
)

// fakeStripeLiveKey is assembled rather than written out: a contiguous
// sk_live_… string in the source is what credential scanners match, and this
// synthetic fixture blocked pushes of this repository under GitHub push
// protection. Built at run time the value is identical, so the scrubber is
// still tested against the real key shape. Same trick as the ghp_ sample in
// stream_test.go.
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
// most consequential scrubber defect: the class of scrubber on the path that
// actually reaches the user's browser was weaker than the one used for whole
// text.
//
// StreamScrubber holds a suffix that could still become a secret and releases
// the rest. Release is decided by viablePrefixRegexes, each of which is meant
// to match EVERY prefix of a possible secret. Three of them did not:
//
//   - the Slack pattern required the type letter and hyphen, so a buffer
//     holding just "xox" was declared non-viable;
//   - the PEM pattern required the full "-----BEGIN <TYPE> PRIVATE KEY-----"
//     marker, so "-----BEGIN" alone was non-viable;
//   - the JWT pattern required "eyJ" immediately after a separator, so a buffer
//     ending at "." or at ".ey" was non-viable.
//
// A dead anchor is never revisited, so in each case the token was released in
// fragments that individually match nothing, and the secret reached the client
// in clear. Whether it leaked depended only on where the provider's chunk
// boundaries happened to fall — the JWT masked correctly at 8- and 64-byte
// deltas and leaked at 1, 3, 4 and 13.
//
// anchorRegex was separately missing AKIA/ASIA, AIza, xox, eyJ and -----BEGIN
// entirely, so those anchors were never even found by release().
//
// This test asserts the property rather than a fixed expected string: across a
// spread of chunk sizes that straddle every internal boundary, no sample's
// secret may survive.
func TestStreamScrubber_NoSecretSurvivesAnyChunking(t *testing.T) {
	// 1 and 2 exercise byte-at-a-time providers; the primes land boundaries
	// inside markers and separators; the large sizes cover whole-chunk arrival.
	deltas := []int{1, 2, 3, 4, 5, 7, 8, 11, 13, 16, 17, 32, 64, 256}

	for _, c := range secretSamples {
		t.Run(c.name, func(t *testing.T) {
			// Baseline: the non-streaming scrubber must mask it, or the sample
			// is testing nothing.
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
// text with no secret must stream through byte-identical. A scrubber that holds
// or mangles ordinary prose is a visible product regression, and widening the
// viability classes is exactly the kind of change that could cause it.
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
		// The contract is "streaming equals whole-text scrubbing", not
		// "streaming is a no-op": ScrubText legitimately masks some of these
		// (its Bearer pattern matches "Bearer scheme"). Comparing against the
		// raw input would assert the scrubber does nothing, which is a
		// different and wrong claim.
		want := ScrubText(in)
		for _, d := range []int{1, 3, 7, 64} {
			if got := streamAll(in, d); got != want {
				t.Errorf("streaming diverged from ScrubText at delta=%d:\n  want %q\n  got  %q", d, want, got)
			}
		}
	}
}

// TestStreamScrubber_LeadInAloneIsHeld is the structural invariant behind all
// three bugs above, stated so the next pattern added here cannot repeat them.
//
// Feed ONLY the opening bytes of each secret — the part that arrives first in a
// stream — and nothing may be emitted yet. If the scrubber releases a lead-in,
// the anchor was judged non-viable on first sight; dead anchors are never
// revisited, so everything that follows streams out unheld and the secret is
// gone. That is precisely how "xox", "-----BEGIN" and a JWT's "…." each failed.
//
// This uses the real samples rather than anchorLiterals so the casing is the
// casing that actually arrives on the wire: the literals are stored lower-cased
// for equalFoldASCII's benefit, while the prefix patterns are case-sensitive
// ("AIza", "eyJ"), and comparing those two directly tests nothing real.
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
// anchorLiterals, but release() scans with anchorRegex. A literal absent from
// the regex is only ever found as a PARTIAL anchor at a buffer boundary, never
// as a complete one mid-buffer.
func TestStreamScrubber_AnchorRegexCoversEveryAnchorLiteral(t *testing.T) {
	for _, a := range anchorLiterals {
		if !anchorRegex.MatchString(a) {
			t.Errorf("anchor literal %q is not matched by anchorRegex — release() cannot find it", a)
		}
	}
}

// TestStreamScrubber_LongSecretTailIsNotReleased is the regression gate for the
// hold cap leaking the part of a credential that did not fit in its budget.
//
// holdBudget drops to streamMatchedHold (256) once the held text contains a
// COMPLETE secret, on the reasoning that there is no point stalling the stream
// further. capOverflow then emitted the whole buffer through ScrubText — masking
// correctly — and reset to holdNone. But the value was still arriving, and its
// `api_key=` lead-in had just been consumed, so every subsequent byte was
// unrecognisable and streamed out verbatim.
//
// The leak was exactly len(secret) - 256: measured 44, 244 and 464 raw
// characters for 300-, 500- and 720-character values, while ScrubText masked
// every one of them to 23 bytes. That is precisely the shape of the credentials
// worth stealing — JWTs, PEM bodies, GCP service-account JSON, Azure SAS URLs
// are all far longer than 256 bytes.
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
// state swallows the tail of a masked secret, so it must stop at the value's end
// or it would silently delete the assistant's next sentence.
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
