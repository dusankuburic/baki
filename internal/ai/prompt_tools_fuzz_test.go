package ai

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// promptToolFixtureFiles are the tests whose inline model texts seed FuzzParsePromptToolCalls.
var promptToolFixtureFiles = []string{
	"prompt_tools_test.go",
	"prompt_tools_marker_test.go",
	"frame_test.go",
}

// FuzzParsePromptToolCalls fuzzes the prompt-mode marker parser over model-authored text, with the two helpers the
// chat service runs on the same text (StripPromptToolCalls, HasUnterminatedToolCall). Invariants:
//   - all three return within fuzzBudget, without panicking;
//   - every parsed call has a name, valid UTF-8 (it is decoded from JSON), and a valid JSON input — the parser
//     decodes each block and defaults a missing input to {};
//   - there are never more calls than opening markers;
//   - stripping never grows the text, and text without a "<tool" marker is left alone: no calls, nothing
//     stripped, nothing unterminated.
//
// The input bytes are not promised to be UTF-8: they are the model's own JSON, passed to the tool verbatim.
func FuzzParsePromptToolCalls(f *testing.F) {
	n := 0
	for _, strs := range fixtureLiterals(f, promptToolFixtureFiles...) {
		for _, decl := range strs {
			for _, s := range decl {
				if strings.Contains(s, "<tool") || strings.Contains(s, "tool_call") {
					f.Add(s)
					n++
				}
			}
		}
	}
	if n == 0 {
		f.Fatal("no <tool_call> fixture found to seed from: update promptToolFixtureFiles")
	}
	f.Fuzz(func(t *testing.T, text string) {
		var (
			calls        []PromptToolCall
			stripped     string
			unterminated bool
		)
		runWithin(t, fuzzBudget, func() {
			calls = ParsePromptToolCalls(text)
			stripped = StripPromptToolCalls(text)
			unterminated = HasUnterminatedToolCall(text)
		})
		for i, c := range calls {
			if c.Name == "" || !utf8.ValidString(c.Name) {
				t.Errorf("call %d: name %q is empty or not valid UTF-8", i, c.Name)
			}
			if !json.Valid(c.Input) {
				t.Errorf("call %d (%q): input is not valid JSON: %q", i, c.Name, c.Input)
			}
		}
		if open := strings.Count(text, toolCallOpen); len(calls) > open {
			t.Errorf("%d calls from %d opening markers", len(calls), open)
		}
		if len(stripped) > len(text) {
			t.Errorf("stripping grew the text from %d to %d bytes", len(text), len(stripped))
		}
		if !strings.Contains(text, "<tool") && (len(calls) > 0 || stripped != text || unterminated) {
			t.Errorf("marker-free text changed: calls=%d stripped=%q unterminated=%v", len(calls), stripped, unterminated)
		}
		if t.Failed() {
			t.Logf("text: %q", text)
		}
	})
}
