package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var updateWire = flag.Bool("update-wire", false, "rewrite testdata/wire/<family>/<case>.golden from the parsers' output")

// wireFamilies maps each testdata/wire/<family> directory to the parser its fixtures run through and the cases
// every family must carry (R-testing-10): a plain answer, a tool-call turn, a mid-stream error, a content-filter
// stop, a truncated body, and the API's thinking or reasoning phase.
var wireFamilies = map[string]struct {
	parser   string
	parse    func(io.Reader, func(Chunk)) error
	required []string
}{
	"anthropic": {"parseClaudeSSE", parseClaudeSSE, []string{"plain", "tool-call", "error", "content-filter", "truncated", "thinking"}},
	"openai":    {"parseOpenAISSE", parseOpenAISSE, []string{"plain", "tool-call", "error", "content-filter", "truncated", "reasoning"}},
	"responses": {"parseResponsesSSE", parseResponsesSSE, []string{"plain", "tool-call", "error", "content-filter", "truncated", "reasoning"}},
	"gemini":    {"parseGeminiSSE", parseGeminiSSE, []string{"plain", "tool-call", "error", "content-filter", "truncated", "thinking"}},
}

// TestWireGolden runs every provider wire fixture (testdata/wire/<family>/<case>.sse) through its SSE parser and
// compares the emitted chunk sequence, and the parser's return, with <case>.golden. A change in what a parser makes
// of a provider's stream — or a refreshed capture the parser reads differently — shows in review. Refresh with
// -update-wire; the capture recipe is in testdata/wire/README.md.
func TestWireGolden(t *testing.T) {
	root := filepath.Join("testdata", "wire")
	families := make([]string, 0, len(wireFamilies))
	for f := range wireFamilies {
		families = append(families, f)
	}
	sort.Strings(families)
	for _, family := range families {
		fam := wireFamilies[family]
		t.Run(family, func(t *testing.T) {
			dir := filepath.Join(root, family)
			fixtures, err := filepath.Glob(filepath.Join(dir, "*.sse"))
			if err != nil {
				t.Fatal(err)
			}
			have := map[string]bool{}
			for _, f := range fixtures {
				have[strings.TrimSuffix(filepath.Base(f), ".sse")] = true
			}
			for _, c := range fam.required {
				if !have[c] {
					t.Errorf("missing required fixture %s/%s.sse", family, c)
				}
			}
			goldens, err := filepath.Glob(filepath.Join(dir, "*.golden"))
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range goldens {
				if c := strings.TrimSuffix(filepath.Base(g), ".golden"); !have[c] {
					t.Errorf("%s has no fixture %s.sse: delete it or restore the fixture", g, c)
				}
			}
			for _, fixture := range fixtures {
				name := strings.TrimSuffix(filepath.Base(fixture), ".sse")
				t.Run(name, func(t *testing.T) {
					raw, err := os.ReadFile(fixture)
					if err != nil {
						t.Fatal(err)
					}
					got := renderWire(family, name, fam.parser, fam.parse, raw)
					path := filepath.Join(dir, name+".golden")
					if *updateWire {
						if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
							t.Fatal(err)
						}
						return
					}
					want, err := os.ReadFile(path)
					if err != nil {
						t.Fatalf("%v (run with -update-wire)", err)
					}
					if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
						t.Errorf("%s differs from what %s makes of %s (refresh with -update-wire and review the diff):\n--- want\n%s\n--- got\n%s",
							path, fam.parser, fixture, want, got)
					}
				})
			}
		})
	}
}

// wireToolIDMasker records a tool-call id as is when the fixture carries it, and as <minted N> (in stream order)
// when the parser invented it: parseGeminiSSE mints process-wide ids (nextGeminiCallID) whose numbers depend on
// what ran before. A provider-supplied id is never masked, whatever its shape.
func wireToolIDMasker(raw []byte) func(string) string {
	minted := map[string]string{}
	return func(id string) string {
		quoted, _ := json.Marshal(id)
		if id == "" || bytes.Contains(raw, quoted) {
			return strconv.Quote(id)
		}
		if s, ok := minted[id]; ok {
			return s
		}
		s := fmt.Sprintf("<minted %d>", len(minted)+1)
		minted[id] = s
		return s
	}
}

// wireProviderMessages collects every "message" string in the fixture's data lines (error envelopes of all four
// families), longest first, so an error can be matched against the provider's own words.
func wireProviderMessages(raw []byte) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok && k == "message" && strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			var v any
			if json.Unmarshal([]byte(strings.TrimSpace(data)), &v) == nil {
				walk(v)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// wireErrorText is an error's class and the provider message it carries: msg="<the fixture's message>" when the
// error text contains it (whitespace-normalised), else msg=none. The wrapping prose is not recorded, so rewording
// it does not churn the goldens, but a parser that loses the provider's message does.
func wireErrorText(err error, messages []string) string {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	text := norm(err.Error())
	msg := "none"
	for _, m := range messages {
		if strings.Contains(text, norm(m)) {
			msg = strconv.Quote(m)
			break
		}
	}
	return wireErrorClass(err) + " msg=" + msg
}

// renderWire is the golden text for one fixture: one line per emitted chunk with its non-zero fields in a fixed
// order, then the parser's return and a summary. An error is its class (what the chat acts on) and the provider
// message it carries, not its wrapping prose.
func renderWire(family, name, parser string, parse func(io.Reader, func(Chunk)) error, raw []byte) string {
	var chunks []Chunk
	ret := parse(bytes.NewReader(raw), func(c Chunk) { chunks = append(chunks, c) })

	toolID := wireToolIDMasker(raw)
	errText := func(err error) string { return wireErrorText(err, wireProviderMessages(raw)) }

	var b strings.Builder
	fmt.Fprintf(&b, "# %s/%s.sse through %s (go test ./internal/ai/ -run TestWireGolden -update-wire)\n", family, name, parser)
	b.WriteString("# one line per chunk; then the parser's return; errors by class (the sentinel) and provider message, not wrapping prose\n")
	var text, reasoning strings.Builder
	keepalives := 0
	for i, c := range chunks {
		text.WriteString(c.Text)
		reasoning.WriteString(c.Reasoning)
		line := wireChunkFields(c, toolID, errText)
		if line == "" {
			keepalives++
			line = "keepalive"
			if c.ServedModel != "" {
				line += " served=" + strconv.Quote(c.ServedModel)
			}
		}
		fmt.Fprintf(&b, "%02d %s\n", i+1, line)
	}
	if ret == nil {
		b.WriteString("return nil\n")
	} else {
		fmt.Fprintf(&b, "return error=%s\n", errText(ret))
	}
	fmt.Fprintf(&b, "summary chunks=%d keepalives=%d text=%q", len(chunks), keepalives, text.String())
	if reasoning.Len() > 0 {
		fmt.Fprintf(&b, " reasoning=%q", reasoning.String())
	}
	b.WriteString("\n")
	return b.String()
}

// wireChunkFields renders c's fields other than a lone served model; "" is a keepalive (nothing but the model).
func wireChunkFields(c Chunk, toolID func(string) string, errText func(error) string) string {
	var f []string
	add := func(s string) { f = append(f, s) }
	if c.Done {
		add("done")
	}
	if c.Error != nil {
		add("error=" + errText(c.Error))
	}
	if c.Text != "" {
		add("text=" + strconv.Quote(c.Text))
	}
	if c.Reasoning != "" {
		add("reasoning=" + strconv.Quote(c.Reasoning))
	}
	if c.FinishReason != "" {
		add("finish=" + strconv.Quote(c.FinishReason))
	}
	for _, kv := range []struct {
		k string
		v int
	}{{"in", c.TokensIn}, {"out", c.TokensOut}, {"cacheRead", c.CacheReadTokens}, {"cacheWrite", c.CacheWriteTokens}} {
		if kv.v != 0 {
			add(fmt.Sprintf("%s=%d", kv.k, kv.v))
		}
	}
	for _, tc := range c.ToolCalls {
		add(fmt.Sprintf("tool{id=%s name=%q input=%s}", toolID(tc.ID), tc.Name, wireJSON(tc.Input)))
	}
	if e := c.ToolEvent; e != nil {
		// DurationMs is wall-clock time: not recorded.
		add(fmt.Sprintf("toolEvent{name=%q label=%q done=%v ok=%v summary=%q}", e.Name, e.Label, e.Done, e.OK, e.Summary))
	}
	if n := c.Notice; n != nil {
		add(fmt.Sprintf("notice{name=%q label=%q}", n.Name, n.Label))
	}
	if c.ServerStep {
		add("serverStep")
	}
	if c.LocalKeepalive {
		add("localKeepalive")
	}
	if c.SilentStep != 0 {
		add(fmt.Sprintf("silentStep=%+d", c.SilentStep))
	}
	if c.attemptDiscarded {
		add("attemptDiscarded")
	}
	if len(c.ProviderItems) > 0 {
		add("items=" + wireJSON(c.ProviderItems))
	}
	if len(f) == 0 {
		return ""
	}
	if c.ServedModel != "" {
		add("served=" + strconv.Quote(c.ServedModel))
	}
	return strings.Join(f, " ")
}

// wireJSON is raw compacted when it is valid JSON, else quoted and marked invalid (a parser forwarding broken tool
// input is a finding in itself).
func wireJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return "invalid:" + strconv.Quote(string(raw))
}

// wireErrorClass names the sentinel err wraps (errorClass, the ai.error_class span attribute), plus a truncated
// body and a rate limit's retry hint.
func wireErrorClass(err error) string {
	class := errorClass(err)
	if class == "other" && errors.Is(err, io.ErrUnexpectedEOF) {
		class = "truncated"
	}
	var rl *RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		class += fmt.Sprintf(" retryAfter=%s", rl.RetryAfter)
	}
	return class
}

// TestWireGolden_MasksOnlyParserMintedIDs: only an id the parser invented (absent from the fixture) is masked. A
// provider-supplied id of the minted shape (Gemini 3's functionCall.id) is recorded as is, so a parser that stops
// honouring it shows in the golden.
func TestWireGolden_MasksOnlyParserMintedIDs(t *testing.T) {
	raw := []byte(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_7","name":"get_block","args":{}}}]}}]}` + "\n\n")
	parse := func(_ io.Reader, onChunk func(Chunk)) error {
		onChunk(Chunk{Done: true, ToolCalls: []ToolCall{
			{ID: "call_7", Name: "get_block", Input: json.RawMessage(`{}`)},
			{ID: "call_12", Name: "list_findings", Input: json.RawMessage(`{}`)},
		}})
		return nil
	}
	got := renderWire("gemini", "mask", "fake", parse, raw)
	for _, want := range []string{`tool{id="call_7" name="get_block"`, `tool{id=<minted 1> name="list_findings"`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in:\n%s", want, got)
		}
	}
}

// TestWireGolden_RecordsTheProviderMessage: an error line carries the provider's message found in the fixture (not
// the wrapping prose), or msg=none, so a parser that loses the message changes the golden.
func TestWireGolden_RecordsTheProviderMessage(t *testing.T) {
	raw := []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded  now\"}}\n\n")
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: claude stream error: Overloaded now", ErrProviderDown), `error=provider_down msg="Overloaded  now"`},
		{fmt.Errorf("%w: claude stream error", ErrProviderDown), `error=provider_down msg=none`},
	} {
		parse := func(_ io.Reader, onChunk func(Chunk)) error {
			onChunk(Chunk{Error: tc.err})
			return tc.err
		}
		got := renderWire("anthropic", "msg", "fake", parse, raw)
		if !strings.Contains(got, "01 "+tc.want) || !strings.Contains(got, "return "+tc.want) {
			t.Errorf("want %q on the chunk and the return in:\n%s", tc.want, got)
		}
	}
}

// TestWireGolden_DataWithoutSpaceParsesTheSame: every fixture, rewritten with "data:" and "event:" lines without
// the optional space after the colon, renders exactly as the fixture does (R-provider-33).
func TestWireGolden_DataWithoutSpaceParsesTheSame(t *testing.T) {
	for family, fam := range wireFamilies {
		fixtures, err := filepath.Glob(filepath.Join("testdata", "wire", family, "*.sse"))
		if err != nil {
			t.Fatal(err)
		}
		for _, fixture := range fixtures {
			raw, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(raw), "\n")
			for i, l := range lines {
				for _, field := range []string{"data: ", "event: "} {
					if strings.HasPrefix(l, field) {
						lines[i] = strings.TrimSuffix(field, " ") + l[len(field):]
					}
				}
			}
			tight := []byte(strings.Join(lines, "\n"))
			name := strings.TrimSuffix(filepath.Base(fixture), ".sse")
			if got, want := renderWire(family, name, fam.parser, fam.parse, tight), renderWire(family, name, fam.parser, fam.parse, raw); got != want {
				t.Errorf("%s without the space after the field colon:\n%s\nwith it:\n%s", fixture, got, want)
			}
		}
	}
}
