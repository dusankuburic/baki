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

// secretSamples covers every class in secretClasses (class names it), with the exact substring that must never
// reach the client. TestSecretClasses_Structure fails when a class, or a token-prefix anchor, has none.
var secretSamples = []struct{ class, name, in, secret string }{
	{"key-value", "generic key/value", "config: password=SuperSecret123 done", "SuperSecret123"},
	{"key-value", "subscription key header", "Ocp-Apim-Subscription-Key: 0123456789abcdef0123456789abcdef done", "0123456789abcdef0123456789abcdef"},
	{"key-value", "storage account key", "AccountName=acct;AccountKey=" + fakeStorageKey + ";EndpointSuffix=x", fakeStorageKey},
	{"key-value", "functions key header", "x-functions-key: 0123456789abcdef0123456789abcdef done", "0123456789abcdef0123456789abcdef"},
	{"key-value-symbols", "yaml value with a symbol", "set password: Winter#2024 now", "Winter#2024"},
	{"quoted-double", "json value with spaces", `body {"password": "correct horse battery"} done`, "correct horse battery"},
	{"quoted-double", "json api key", `{"api_key":"q8Wz3kLp0mNs7Rt2"} done`, "q8Wz3kLp0mNs7Rt2"},
	{"quoted-double", "yaml quoted value with spaces", `cfg password: "correct horse battery" done`, "correct horse battery"},
	{"quoted-single", "python dict secret", "{'client_secret': 'Zx9!Yw8@Vu7'} done", "Zx9!Yw8@Vu7"},
	{"quoted-single", "yaml single-quoted passphrase", "cfg password: 'supersecret pass' done", "supersecret pass"},
	{"basic-auth", "basic auth header", "Authorization: Basic dXNlcjpwYXNzd29yZA== done", "dXNlcjpwYXNzd29yZA=="},
	{"bearer", "bearer", "header: Bearer abcdefghijklmnopqrstuvwxyz012345 done", "abcdefghijklmnopqrstuvwxyz012345"},
	{"url-userinfo", "url userinfo password", "url https://svc:Pr0d!Pass@ftp.example.com/in done", "Pr0d!Pass"},
	{"url-credential-param", "sas signature", "url https://a.blob.core.windows.net/c/f.csv?sv=1&sig=Ab12Cd34Ef56%2BGh78%3D done", "Ab12Cd34Ef56"},
	{"url-credential-param", "functions code", "url https://fn.azurewebsites.net/api/Run?code=AbCdEf123456789xyz%3D%3D&name=x done", "AbCdEf123456789xyz"},
	{"url-credential-param", "aws sigv4 signature", "url https://b.s3.amazonaws.com/k?X-Amz-Signature=0a1b2c3d4e5f60718293a4b5c6d7e8f9 done", "0a1b2c3d4e5f60718293a4b5c6d7e8f9"},
	{"url-credential-param", "oauth token before more parameters", "see https://x.example.com/cb?access_token=AbCdEfGh12345678&state=xyz&page=2 next", "AbCdEfGh12345678"},
	{"url-credential-param", "short token in a url", "see https://h/cb?token=abc12 done", "abc12"},
	{"url-credential-param", "aws session token", "url https://b.s3.amazonaws.com/k?X-Amz-Credential=AKIA%2F1&X-Amz-Security-Token=FwoGZXIvYXdzEBYaDHk4Q1dmZ2Zh done", "FwoGZXIvYXdzEBYaDHk4Q1dmZ2Zh"},
	{"url-credential-param", "sas in a storage connection string", "BlobEndpoint=https://a.blob.core.windows.net/;SharedAccessSignature=sv=2022&sr=c&sig=AbC123XyZ%3D;EndpointSuffix=core.windows.net", "AbC123XyZ"},
	{"stripe", "stripe sk_", "key: " + fakeStripeLiveKey + " done", fakeStripeLiveKey},
	{"stripe", "stripe pk_", "key: " + fakeStripePublishable + " done", fakeStripePublishable},
	{"stripe", "stripe rk_", "key: " + fakeStripeRestricted + " done", fakeStripeRestricted},
	{"stripe", "stripe whsec_", "key: " + fakeStripeWebhook + " done", fakeStripeWebhook},
	{"github", "github ghp_", "tok: ghp_123456789012345678901234567890123456 done", "ghp_123456789012345678901234567890123456"},
	{"github", "github gho_", "tok: " + fakeGitHubOAuth + " done", fakeGitHubOAuth},
	{"github", "github ghu_", "tok: " + fakeGitHubClassic("u") + " done", fakeGitHubClassic("u")},
	{"github", "github ghs_", "tok: " + fakeGitHubClassic("s") + " done", fakeGitHubClassic("s")},
	{"github", "github ghr_", "tok: " + fakeGitHubClassic("r") + " done", fakeGitHubClassic("r")},
	{"github", "github fine-grained pat", "tok: " + fakeGitHubPAT + " done", fakeGitHubPAT},
	{"gitlab", "gitlab glpat-", "tok: " + fakeGitLabPAT + " done", fakeGitLabPAT},
	{"gitlab", "gitlab glpat_", "tok: " + fakeGitLabUnderscore + " done", fakeGitLabUnderscore},
	{"sk-dash", "openai sk-proj-", "key " + fakeOpenAIKey + " done", fakeOpenAIKey},
	{"sk-dash", "anthropic sk-ant-", "key " + fakeAnthropicKey + " done", fakeAnthropicKey},
	{"connection-string", "connection string", "dsn: Server=db1;User=sa;Password=P@ssw0rd123; done", "P@ssw0rd123"},
	{"aws-access-key", "aws AKIA", "creds: AKIAIOSFODNN7EXAMPLE done", "AKIAIOSFODNN7EXAMPLE"},
	{"google-api-key", "google AIza", "key: AIzaSyC1234567890abcdefghijklmnopqrstuv done", "AIzaSyC1234567890abcdefghijklmnopqrstuv"},
	{"slack", "slack xoxb", "tok: xoxb-1234567890-abcdefghij done", "xoxb-1234567890-abcdefghij"},
	{"jwt", "jwt", "jwt: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N done",
		"eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
	{"pem-private-key", "pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234567890\n-----END RSA PRIVATE KEY-----",
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

// TestStreamScrubber_AnchorScanFindsEveryAnchorLiteral closes the gap that
// once made AKIA/AIza/xox/eyJ/PEM unreachable: the partial-anchor check and
// release()'s scan used two different literal lists, so a literal absent from
// the scan was only ever a PARTIAL anchor at a buffer boundary, never a
// complete one mid-buffer. Both now read anchorsByFirst; this pins that the
// scan finds every literal, in either case, wherever it sits.
func TestStreamScrubber_AnchorScanFindsEveryAnchorLiteral(t *testing.T) {
	for _, a := range anchorLiterals {
		for _, spelled := range []string{a, strings.ToUpper(a)} {
			in := []byte("xx " + spelled + " yy")
			if start, end := findAnchor(in, 0); start != 3 || end != 3+len(a) {
				t.Errorf("findAnchor(%q) = (%d, %d), want (3, %d) — release() cannot find the literal %q",
					in, start, end, 3+len(a), a)
			}
		}
	}
}

// longSecretLeadIns are the lead-ins of the classes whose value can outgrow streamMatchedHold while still
// arriving: every one needs a terminal state, or the cap emits the held prefix masked and the rest of the
// value streams out in clear. The value is a run of 'z' ('z' avoids colliding with the letters in
// "[REDACTED]"); the Basic lead-in is base64 for "user:" so the value decodes to credentials.
var longSecretLeadIns = map[string]string{
	"key-value":           "api_key=",
	"key-value-symbols":   "set password: z#",
	"url oauth token":     "see https://x.example.com/cb?access_token=",
	"url aws session":     "url https://b.s3.amazonaws.com/k?X-Amz-Credential=AKIA%2F1&X-Amz-Security-Token=",
	"url token after sig": "url https://h/x?sig=abc123&other=1&token=",
	"basic auth":          "Authorization: Basic dXNlcjo",
	"jwt":                 "jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.",
	"bearer":              "Authorization: Bearer ",
}

// TestStreamScrubber_LongSecretTailIsNotReleased is the regression gate for the
// hold cap: once held text contained a COMPLETE secret, holdBudget dropped to
// streamMatchedHold (256) and capOverflow emitted the buffer masked and reset —
// but the value kept arriving and its lead-in was consumed, so every
// subsequent byte streamed out verbatim: a leak of len(secret)-256 raw chars,
// hitting precisely the long credentials worth stealing (JWTs, session tokens,
// SAS URLs).
func TestStreamScrubber_LongSecretTailIsNotReleased(t *testing.T) {
	for name, lead := range longSecretLeadIns {
		t.Run(name, func(t *testing.T) {
			for _, n := range []int{100, 257, 300, 500, 720, 2000} {
				secret := strings.Repeat("z", n)
				in := lead + secret + " done"

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
		})
	}
}

// TestStreamScrubber_DrainDoesNotEatFollowingText is the other half: the drain
// state swallows the tail of a masked secret, so it must stop at the value's
// end or it silently deletes the assistant's next sentence.
func TestStreamScrubber_DrainDoesNotEatFollowingText(t *testing.T) {
	const tail = "and here is the explanation that follows."
	for name, lead := range longSecretLeadIns {
		t.Run(name, func(t *testing.T) {
			in := lead + strings.Repeat("z", 600) + " " + tail

			for _, delta := range []int{1, 8, 64, 512} {
				got := streamAll(in, delta)
				if !strings.Contains(got, tail) {
					t.Errorf("delta=%d: text after the secret was swallowed by the drain\n  out: %q",
						delta, got)
				}
			}
		})
	}
}
