package scrubber

import (
	"strings"
	"testing"
)

// streamScrub runs text through a StreamScrubber in chunks of the given size
// and returns the concatenated output (including the final Flush).
func streamScrub(text string, chunkSize int) string {
	s := NewStreamScrubber()
	var out strings.Builder
	for i := 0; i < len(text); i += chunkSize {
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}
		out.WriteString(s.Write(text[i:end]))
	}
	out.WriteString(s.Flush())
	return out.String()
}

// TestStreamScrubber_MatchesScrubTextForAnyChunking is the core contract: no
// matter how the text is sliced into chunks, the streamed result equals
// scrubbing the whole text at once — a secret split across chunk boundaries
// is masked exactly as if it had arrived in one piece.
func TestStreamScrubber_MatchesScrubTextForAnyChunking(t *testing.T) {
	samples := []string{
		"the connection uses password=supersecret123 and then continues",
		"Set PWD = hunter2secret99; Initial Catalog=db; done",
		"line one has password=abcdef123456\nline two is normal prose",
		"header Authorization: Bearer abc123def456ghi789 was sent",
		"use sk_live_abcdefghijklmnopqrstu for billing",
		"pat is ghp_" + strings.Repeat("a1B2", 9) + " here",
		"api_key: 'abcdef123456' trailing prose",
		"private-key = \"MIIEvQIBADANBg81u\" end",
		"the password field is empty, please fill in the token count",
		"just a normal sentence with no secrets at all",
		"héllo wörld — unicode prose with a password=sécrets ascii12345678 tail",
		"secret:tiny", // value too short to ever match — must pass through
		"password=",   // anchor with no value, resolved only at flush
	}
	for _, sample := range samples {
		want := ScrubText(sample)
		// Fixed chunk sizes, including pathological 1-byte streaming.
		for _, size := range []int{1, 2, 3, 5, 8, 13, len(sample)} {
			if got := streamScrub(sample, size); got != want {
				t.Errorf("chunkSize=%d\nsample %q\ngot    %q\nwant   %q", size, sample, got, want)
			}
		}
		// Every possible two-chunk split.
		for i := 0; i <= len(sample); i++ {
			s := NewStreamScrubber()
			got := s.Write(sample[:i]) + s.Write(sample[i:]) + s.Flush()
			if got != want {
				t.Errorf("split at %d\nsample %q\ngot    %q\nwant   %q", i, sample, got, want)
			}
		}
	}
}

// TestSecretPatternRepresentationsStayInSync is a structural guard, not an
// example-based one: the "anyChunking" test above only proves the SAMPLES it
// already knows about stream-scrub correctly, so a NEW secretRegexes pattern
// added without a matching viablePrefixRegexes entry (and a representative
// sample here) would pass CI silently — the exact "streaming under-masks a
// secret type that ScrubText catches" failure mode this package's streaming
// design exists to prevent.
//
// secretRegexes (scrubber.go) and viablePrefixRegexes (stream.go) are meant to
// correspond 1:1 in the same order: each full-match pattern has exactly one
// "is this buffer tail still a viable prefix of that pattern" counterpart. If
// this test fails after adding a pattern to secretRegexes, add its
// viable-prefix counterpart to viablePrefixRegexes (same index) AND a
// representative sample to TestStreamScrubber_MatchesScrubTextForAnyChunking
// above — a count match alone doesn't prove the new pair actually agrees on
// what a viable prefix looks like, only the chunking test does that.
func TestSecretPatternRepresentationsStayInSync(t *testing.T) {
	if len(secretRegexes) != len(viablePrefixRegexes) {
		t.Fatalf("secretRegexes has %d pattern(s) but viablePrefixRegexes has %d — "+
			"every full-match pattern needs exactly one corresponding viable-prefix "+
			"pattern in stream.go (same index), or streamed responses can under-mask "+
			"a secret type that ScrubText still catches",
			len(secretRegexes), len(viablePrefixRegexes))
	}
}

// TestStreamScrubber_NeverEmitsHeldSecretEarly: the first Write ending inside
// a secret must not release any part of the value.
func TestStreamScrubber_NeverEmitsHeldSecretEarly(t *testing.T) {
	s := NewStreamScrubber()
	out := s.Write("the key is password=super")
	if strings.Contains(out, "super") || strings.Contains(out, "password") {
		t.Fatalf("partial secret leaked in early emit: %q", out)
	}
	out += s.Write("secret123 and we continue")
	out += s.Flush()
	if strings.Contains(out, "supersecret123") {
		t.Fatalf("split secret leaked: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("split secret was not masked: %q", out)
	}
	// The Password=/PWD= pattern greedily masks to end of line, so streamed
	// output must equal the whole-text result, not preserve the trailing prose.
	if want := ScrubText("the key is password=supersecret123 and we continue"); out != want {
		t.Fatalf("streamed output %q != whole-text scrub %q", out, want)
	}
}

// TestStreamScrubber_MaskStopsAtLineEnd: an unterminated password= mask must
// stop at the end of its line — the rest of the message survives instead of
// being swallowed into [REDACTED].
func TestStreamScrubber_MaskStopsAtLineEnd(t *testing.T) {
	s := NewStreamScrubber()
	out := s.Write("uses password=sup") + s.Write("ersecret123\nwhich is risky, fix it.") + s.Flush()
	if strings.Contains(out, "supersecret123") {
		t.Fatalf("secret leaked: %q", out)
	}
	want := "uses password=[REDACTED]\nwhich is risky, fix it."
	if out != want {
		t.Fatalf("out = %q, want %q (text after the newline must survive)", out, want)
	}
}

// TestStreamScrubber_ProseReleasedNextChunk: a bare keyword at a chunk
// boundary ("...the password") is held one chunk, then released untouched
// once the continuation proves it is prose.
func TestStreamScrubber_ProseReleasedNextChunk(t *testing.T) {
	s := NewStreamScrubber()
	first := s.Write("please enter the password")
	if strings.Contains(first, "password") {
		t.Fatalf("keyword tail should be held, got %q", first)
	}
	rest := s.Write(" in the form below")
	if !strings.Contains(first+rest, "please enter the password in the form") {
		t.Fatalf("prose was altered or withheld: %q", first+rest)
	}
}

// TestStreamScrubber_FlushMasksHeldSecret: a stream ending mid-secret still
// masks it on the final flush.
func TestStreamScrubber_FlushMasksHeldSecret(t *testing.T) {
	s := NewStreamScrubber()
	out := s.Write("token=abcdefgh12")
	if out != "" {
		t.Fatalf("growing secret must be held, got %q", out)
	}
	if got := s.Flush(); got != "token=[REDACTED]" {
		t.Fatalf("Flush = %q, want token=[REDACTED]", got)
	}
}

// TestStreamScrubber_HoldCapReleases: a pathological never-ending value stops
// stalling the stream once it exceeds the hold cap. (This is the one case
// where streamed output may differ from whole-text scrubbing — documented on
// streamMaxHold.)
func TestStreamScrubber_HoldCapReleases(t *testing.T) {
	s := NewStreamScrubber()
	var out strings.Builder
	out.WriteString(s.Write("password=" + strings.Repeat("a", streamMaxHold)))
	out.WriteString(s.Write(strings.Repeat("b", 512)))
	if out.Len() == 0 {
		t.Fatal("hold cap did not release any text for an oversized value")
	}
	out.WriteString(s.Flush())
	if strings.Contains(out.String(), "password=aaaa") {
		t.Fatalf("the capped emit leaked the key=value lead-in unmasked")
	}
}
