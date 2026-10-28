package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// R-testing-2: fuzz targets for the hand-written parsers that read untrusted bytes — the four provider SSE state
// machines here and in responses_fuzz_test.go, the prompt-mode marker parser and the tool handlers' input
// decoding. Each body asserts the parser's contract, not only "no panic". Run one locally with
//
//	go test ./internal/ai/ -run '^$' -fuzz '^FuzzParseClaudeSSE$' -fuzztime 60s
//
// The nightly matrix in .github/workflows/bench-fuzz.yml runs all six; a crasher lands in testdata/fuzz/<target>/
// and must become a regression test (keep the corpus file).

// fuzzBudget bounds one fuzz input: every input is small and every parser is linear, so a run that takes seconds
// is a hang or a quadratic path, not a slow machine.
const fuzzBudget = 5 * time.Second

// runWithin runs fn on its own goroutine and fails the input when it panics or does not return within budget —
// a synchronous elapsed-time check would never fire on an infinite loop.
func runWithin(t *testing.T, budget time.Duration, fn func()) {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Sprintf("panic: %v\n%s", r, debug.Stack())
				return
			}
			done <- ""
		}()
		fn()
	}()
	select {
	case msg := <-done:
		if msg != "" {
			t.Fatal(msg)
		}
	case <-time.After(budget):
		t.Fatalf("did not return within %s", budget)
	}
}

// fixtureLiterals returns, per top-level declaration of each file, the constant string expressions it contains
// (a literal or a + concatenation of literals), in source order. The fuzz targets seed from the inline fixtures
// of the parser tests this way, so a fixture added to those tests seeds the fuzzer without being copied here.
// A file that is missing or does not parse fails the seeding: the list below names files that must exist.
func fixtureLiterals(tb testing.TB, files ...string) map[string][][]string {
	tb.Helper()
	out := map[string][][]string{}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			tb.Fatalf("seed fixture file %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			var strs []string
			ast.Inspect(decl, func(n ast.Node) bool {
				e, ok := n.(ast.Expr)
				if !ok {
					return true
				}
				if s, ok := constString(e); ok {
					strs = append(strs, s)
					return false
				}
				return true
			})
			if len(strs) > 0 {
				out[name] = append(out[name], strs)
			}
		}
	}
	return out
}

// constString evaluates a string literal or a + concatenation of them; anything else (a variable, a call) is not constant here.
func constString(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return constString(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok := constString(e.X)
		if !ok {
			return "", false
		}
		r, ok := constString(e.Y)
		return l + r, ok
	}
	return "", false
}

// sseFixtureFiles are the tests whose inline SSE fixtures seed the four SSE targets. Every target gets all of
// them: a Claude fixture fed to the OpenAI parser is still a useful starting point for the mutator.
var sseFixtureFiles = []string{
	"stream_test.go",
	"claude_replay_test.go",
	"claude_route_test.go",
	"claude_request_test.go",
	"stream_cache_test.go",
	"stream_tools_test.go",
	"openai_sse_test.go",
	"openai_compat_repairs_test.go",
	"reasoning_models_test.go",
	"gemini_classify_test.go",
	"gemini_errors_test.go",
	"responses_test.go",
	"responses_replay_test.go",
	"responses_replay_fast_test.go",
	"responses_foundry_test.go",
	"audited_cache_tokens_test.go",
	"spend_integrity_test.go",
}

// sseSeeds builds the SSE seed corpus from the fixture files:
//   - every constant string with a `data:` line, as it is (a trailing newline added to a lone line);
//   - per declaration, its single `data:` lines joined into one stream (the Gemini tests list lines, not bodies);
//   - per declaration, its bare JSON events (`{"type":…}`, the Responses tests' sseEvents arguments) framed as
//     `event:`/`data:` lines into one stream;
//   - the short terminalAfterTerminal bodies.
func sseSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	seen := map[string]bool{}
	var seeds []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			seeds = append(seeds, s)
		}
	}
	for _, tc := range terminalAfterTerminal {
		if len(tc.body) < 4<<10 {
			add(tc.body)
		}
	}
	lits := fixtureLiterals(tb, sseFixtureFiles...)
	for _, name := range sseFixtureFiles {
		before := len(seeds)
		for _, strs := range lits[name] {
			var lines, events []string
			for _, s := range strs {
				switch {
				case strings.Contains(s, "data:"):
					if !strings.HasSuffix(s, "\n") {
						s += "\n\n"
					}
					add(s)
					if strings.HasPrefix(s, "data:") && strings.Count(strings.TrimRight(s, "\n"), "\n") == 0 {
						lines = append(lines, s)
					}
				case strings.HasPrefix(strings.TrimSpace(s), `{"type":`) && json.Valid([]byte(s)):
					var head struct {
						Type string `json:"type"`
					}
					_ = json.Unmarshal([]byte(s), &head)
					events = append(events, "event: "+head.Type+"\ndata: "+s+"\n\n")
				}
			}
			if len(lines) > 1 {
				add(strings.Join(lines, ""))
			}
			if len(events) > 0 {
				add(strings.Join(events, ""))
			}
		}
		if len(seeds) == before {
			tb.Fatalf("seed fixture file %s yields no SSE seed: update sseFixtureFiles", name)
		}
	}
	sort.Strings(seeds)
	out := make([][]byte, len(seeds))
	for i, s := range seeds {
		out[i] = []byte(s)
	}
	return out
}

// sseContract is what a parser promises about the chunks it emits, beyond not panicking.
type sseContract struct {
	// toolInputJSON: every tool-call input is valid JSON. Only Gemini promises it — it marshals the decoded args
	// itself. Claude, OpenAI-compatible and Responses pass the model's argument fragments through verbatim, so a
	// call cut mid-argument or the malformed JSON open models emit reaches ExecuteTool, which answers the model
	// with a retryable "not valid JSON" error, and the chat service records it as {} (invalidToolInput). Those
	// parsers promise a non-blank input instead: a blank one passed both checks and broke the replayed request.
	toolInputJSON bool
}

// checkSSE runs parse over data and asserts the shared SSE parser contract:
//   - it returns within fuzzBudget, without panicking;
//   - at most one terminal chunk (Done, or an Error chunk): a second terminal re-records usage in the audited
//     wrapper and fails a turn whose answer was already delivered;
//   - a nil return delivered exactly one terminal chunk, an error return none — the stream ended either way;
//   - tool calls ride only on the Done chunk, each with a non-blank input (valid JSON where promised);
//   - every string decoded from JSON (text, reasoning, names, ids, labels, errors) is valid UTF-8, since
//     encoding/json replaces invalid bytes with U+FFFD; ProviderItems, when present, is valid JSON;
//   - the emitted text is bounded by the input (at most 3× — each invalid byte may decode to a 3-byte U+FFFD).
func checkSSE(t *testing.T, parse func([]byte, func(Chunk)) error, data []byte, c sseContract) {
	t.Helper()
	var (
		chunks []Chunk
		err    error
	)
	runWithin(t, fuzzBudget, func() {
		err = parse(data, func(ch Chunk) { chunks = append(chunks, ch) })
	})

	terminals, textBytes := 0, 0
	for i, ch := range chunks {
		if ch.Done || ch.Error != nil {
			terminals++
		}
		if len(ch.ToolCalls) > 0 && !ch.Done {
			t.Errorf("chunk %d carries %d tool call(s) without Done", i, len(ch.ToolCalls))
		}
		textBytes += len(ch.Text) + len(ch.Reasoning)
		strs := map[string]string{"text": ch.Text, "reasoning": ch.Reasoning, "finishReason": ch.FinishReason, "servedModel": ch.ServedModel}
		if ch.Error != nil {
			strs["error"] = ch.Error.Error()
		}
		if ev := ch.ToolEvent; ev != nil {
			strs["toolEvent.name"], strs["toolEvent.label"], strs["toolEvent.summary"] = ev.Name, ev.Label, ev.Summary
		}
		for j, tc := range ch.ToolCalls {
			strs[fmt.Sprintf("toolCalls[%d].id", j)] = tc.ID
			strs[fmt.Sprintf("toolCalls[%d].name", j)] = tc.Name
			if len(bytes.TrimSpace(tc.Input)) == 0 {
				t.Errorf("chunk %d tool call %d (%q) has a blank input %q", i, j, tc.Name, tc.Input)
			} else if c.toolInputJSON && !json.Valid(tc.Input) {
				t.Errorf("chunk %d tool call %d (%q) input is not valid JSON: %q", i, j, tc.Name, tc.Input)
			}
		}
		for field, s := range strs {
			if !utf8.ValidString(s) {
				t.Errorf("chunk %d %s is not valid UTF-8: %q", i, field, s)
			}
		}
		if len(ch.ProviderItems) > 0 && !json.Valid(ch.ProviderItems) {
			t.Errorf("chunk %d ProviderItems is not valid JSON: %q", i, ch.ProviderItems)
		}
	}
	switch {
	case terminals > 1:
		t.Errorf("%d terminal chunks, want at most 1 (err=%v)", terminals, err)
	case err == nil && terminals != 1:
		t.Errorf("nil return with %d terminal chunks, want exactly 1", terminals)
	case err != nil && terminals != 0:
		t.Errorf("error return %v after a terminal chunk", err)
	}
	if textBytes > 3*len(data) {
		t.Errorf("emitted %d bytes of text from %d bytes of input", textBytes, len(data))
	}
	if t.Failed() {
		t.Logf("input: %q", data)
		for i, ch := range chunks {
			t.Logf("chunk %d: %+v", i, ch)
		}
	}
}

func readerParse(p func(r *bytes.Reader, on func(Chunk)) error) func([]byte, func(Chunk)) error {
	return func(data []byte, on func(Chunk)) error { return p(bytes.NewReader(data), on) }
}

func FuzzParseClaudeSSE(f *testing.F) {
	for _, s := range sseSeeds(f) {
		f.Add(s)
	}
	parse := readerParse(func(r *bytes.Reader, on func(Chunk)) error { return parseClaudeSSE(r, on) })
	f.Fuzz(func(t *testing.T, data []byte) {
		checkSSE(t, parse, data, sseContract{})
	})
}

func FuzzParseOpenAISSE(f *testing.F) {
	for _, s := range sseSeeds(f) {
		f.Add(s)
	}
	parse := readerParse(func(r *bytes.Reader, on func(Chunk)) error { return parseOpenAISSE(r, on) })
	f.Fuzz(func(t *testing.T, data []byte) {
		checkSSE(t, parse, data, sseContract{})
	})
}

func FuzzParseGeminiSSE(f *testing.F) {
	for _, s := range sseSeeds(f) {
		f.Add(s)
	}
	parse := readerParse(func(r *bytes.Reader, on func(Chunk)) error { return parseGeminiSSE(r, on) })
	f.Fuzz(func(t *testing.T, data []byte) {
		checkSSE(t, parse, data, sseContract{toolInputJSON: true})
	})
}

// Found by FuzzParseClaudeSSE (testdata/fuzz/FuzzParseClaudeSSE/79f792d523c8235a): a tool call whose streamed
// arguments are whitespace only ("\f", " ") was returned verbatim. It is not JSON, yet both guards read it as "no
// arguments" (the chat service's invalidToolInput and ExecuteTool trim it to empty), so it was recorded as is and
// the replayed assistant turn could not be marshalled (json.RawMessage rejects it). Blank arguments are "{}", as the
// Responses parser already makes them.
func TestStreamedToolCall_BlankArgumentsAreAnEmptyObject(t *testing.T) {
	claude := "event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"list_findings\"}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\" \\f\"}}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"
	openai := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"list_findings","arguments":" \n"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	for name, tc := range map[string]struct {
		parse func(io.Reader, func(Chunk)) error
		body  string
	}{"claude": {parseClaudeSSE, claude}, "openai": {parseOpenAISSE, openai}} {
		var done Chunk
		if err := tc.parse(strings.NewReader(tc.body), func(c Chunk) {
			if c.Done {
				done = c
			}
		}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(done.ToolCalls) != 1 || string(done.ToolCalls[0].Input) != "{}" {
			t.Errorf("%s: tool calls %+v, want one with input {}", name, done.ToolCalls)
		}
		if _, err := json.Marshal(done.ToolCalls); err != nil {
			t.Errorf("%s: the tool calls cannot be replayed: %v", name, err)
		}
	}
}

// terminalAfterTerminal are SSE bodies whose stream goes on after its terminal event: a second stop, a refusal or
// an error event after the Done, a line over the scanner cap after it. The byte mutator rarely duplicates a whole
// event, so they seed the SSE targets and pin the fix below.
var terminalAfterTerminal = map[string]struct {
	parse func(io.Reader, func(Chunk)) error
	body  string
}{
	"claude/second stop": {parseClaudeSSE, claudeStop + claudeStop},
	"claude/refusal after the stop": {parseClaudeSSE, claudeStop +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"refusal\"}}\n\n"},
	"claude/error after the stop": {parseClaudeSSE, claudeStop +
		"event: error\ndata: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"},
	"gemini/error after the finish": {parseGeminiSSE, geminiStop +
		"data: {\"error\":{\"code\":503,\"status\":\"UNAVAILABLE\",\"message\":\"The model is overloaded.\"}}\n\n"},
	"gemini/overlong line after the finish": {parseGeminiSSE, geminiStop + "data: " + strings.Repeat("x", 1<<20) + "\n\n"},
}

const (
	claudeStop = "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"
	geminiStop = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":1}}\n\n"
)

// Found by the FuzzParse*SSE terminal invariant (seeded by hand, see terminalAfterTerminal): the Claude parser
// emitted a second Done for a second stop_reason, and an error chunk for a refusal or error event after its Done;
// the Gemini parser an error chunk for an error event after its finish, and a read error after it. Every stream
// must end once: the audited wrapper records usage on each Done (billed twice), and the chat loop fails a turn
// whose answer was already delivered on an error after it. As the Claude parser already did for a read error,
// whatever follows the terminal chunk is ignored.
func TestSSEParsers_NothingTerminalAfterTheTerminalChunk(t *testing.T) {
	for name, tc := range terminalAfterTerminal {
		t.Run(name, func(t *testing.T) {
			checkSSE(t, func(data []byte, on func(Chunk)) error { return tc.parse(bytes.NewReader(data), on) }, []byte(tc.body), sseContract{})
		})
	}
}
