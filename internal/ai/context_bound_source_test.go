package ai

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// padSource synthesises size bytes of PAD-like source (CRLF, indented, the
// mix of literals, numbers and %variables% a real flow carries) so the
// benchmarks and the bounded-context tests below see realistic tokenisation.
func padSource(size int) string {
	var b strings.Builder
	b.Grow(size + 512)
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "    Variables.SetVariable NewValue: $'''Value %d''' Name: Var%d\r\n", i*7919%10000, i%97)
		fmt.Fprintf(&b, "    WebAutomation.NavigateTo Url: $'''https://example.com/page%d''' BrowserInstance: %%Browser%%\r\n", i)
		fmt.Fprintf(&b, "    IF %%Counter%d%% > %d THEN\r\n        Display.ShowMessageBox Message: $'''Message %d''' Icon: Display.Icon.None => ButtonPressed\r\n    END\r\n", i%13, i%50, i)
	}
	return b.String()
}

// A selected 5 MB source file costs the context what its first 64 KB cost:
// the file is bounded by bytes before it is appended, the assembled text
// is tokenised by prefix, and the context message is byte-identical to what
// appending and tokenising the whole file produced.
func TestBuildContext_BoundsSourceFilesBeforeTokenising(t *testing.T) {
	tkm := requireTokenizer(t)
	src := padSource(5 << 20)
	doc := makeMinimalDoc("Big")
	req := ContextRequest{
		Flow:           doc,
		RawSourceFiles: map[string]string{"Main.txt": src},
		TokenBudget:    4000,
	}

	encoded := countEncodedBytes(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, ctx := BuildContext(req)
	runtime.ReadMemStats(&after)

	if n := *encoded; n >= 128<<10 {
		t.Errorf("tokenised %d bytes for a %d-byte source, want under 128 KB", n, len(src))
	}
	// Tokenising the 64 KB head alone is ~8 MB of tokenizer garbage; the
	// unbounded file adds its own copies (≥ 18 MB measured).
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= 12<<20 {
		t.Errorf("BuildContext allocated %d bytes, want under 12 MB (the file must be bounded before it is appended)", allocated)
	}

	// The old implementation's result: the whole file appended, the whole
	// text encoded, the first budget-3 tokens kept.
	old := "## Flow: Big\n\n### Source Files (1)\n\n#### Main.txt\n\n```\n" + src + "\n```\n\n"
	want := refTruncate(t, tkm, old, 4000)
	if ctx != want {
		t.Fatalf("context differs from the whole-file result:\n got %d bytes %q…\nwant %d bytes %q…", len(ctx), ctx[:min(120, len(ctx))], len(want), want[:min(120, len(want))])
	}
	if !strings.HasSuffix(ctx, "...") || strings.Contains(ctx, SourceTruncatedMarker) {
		t.Errorf("the budget cut must land inside the kept part of the file: %q", ctx[max(0, len(ctx)-60):])
	}

	if allocs := testing.AllocsPerRun(3, func() { BuildContext(req) }); allocs > 120_000 {
		t.Errorf("BuildContext made %.0f allocations, want at most 120000 (tokenising ~64 KB)", allocs)
	}
}

// BoundSourceText keeps a file within tokenBudget × 16 bytes, cuts after a
// line break when one is near the cap (else on a rune boundary), marks the
// cut, and leaves a file that fits — or a non-positive budget — untouched.
func TestBoundSourceText(t *testing.T) {
	short := "LAUNCH Browser\nCLICK btn\n"
	if got := BoundSourceText(short, 4000); got != short {
		t.Errorf("a fitting file changed: %q", got)
	}
	long := padSource(200 << 10)
	if got := BoundSourceText(long, 0); got != long {
		t.Error("a non-positive budget must not bound")
	}
	got := BoundSourceText(long, 4000)
	if !strings.HasSuffix(got, SourceTruncatedMarker) {
		t.Fatalf("no marker on a bounded file: %q", got[max(0, len(got)-40):])
	}
	body := strings.TrimSuffix(got, SourceTruncatedMarker)
	if len(body) > 4000*16 || len(body) < 4000*16-4096 {
		t.Errorf("kept %d bytes, want within 4 KB under the 64 KB cap", len(body))
	}
	if !strings.HasSuffix(body, "\n") || !strings.HasPrefix(long, body) {
		t.Errorf("the cut must keep whole lines of the original: ends %q", body[max(0, len(body)-20):])
	}

	// No line break near the cap: the cut is on a rune boundary.
	runes := strings.Repeat("日本語テキスト", 20000)
	got = BoundSourceText(runes, 100)
	body = strings.TrimSuffix(got, SourceTruncatedMarker)
	if !utf8.ValidString(body) || len(body) > 1600 || !strings.HasPrefix(runes, body) {
		t.Errorf("rune-snapped cut broken: %d bytes, valid %v", len(body), utf8.ValidString(body))
	}
}

func BenchmarkBuildContext_5MBSource(b *testing.B) {
	src := padSource(5 << 20)
	doc := makeMinimalDoc("Big")
	req := ContextRequest{Flow: doc, RawSourceFiles: map[string]string{"Main.txt": src}, TokenBudget: 4000}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, ctx := BuildContext(req)
		if len(ctx) == 0 {
			b.Fatal("empty context")
		}
	}
}
