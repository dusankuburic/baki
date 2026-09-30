package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ExecuteTool answers malformed argument JSON with the standard retryable
// error result (audit #9 F-11) instead of letting a handler choke on it: the
// model reads the message and can re-issue the call with valid JSON, and the
// loop keeps its bounded tool-result contract. Empty input is the providers'
// "no arguments" and still runs.
func TestExecuteTool_InvalidJSONArguments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"cut mid-object", `{"query":"xa`},
		{"not JSON at all", `not json`},
		{"trailing garbage", `{"query":"x"} trailing`},
	} {
		out := ExecuteTool("search_flow", json.RawMessage(tc.input), ToolContext{Ctx: context.Background()})
		if !strings.HasPrefix(out, "error:") || !strings.Contains(out, "arguments for search_flow were not valid JSON") {
			t.Errorf("%s: ExecuteTool = %q, want the retryable not-valid-JSON error result", tc.name, out)
		}
		if !strings.Contains(out, "call it again with valid JSON") {
			t.Errorf("%s: result %q does not tell the model to retry", tc.name, out)
		}
	}
	// Unknown tools keep their own message (checked before the JSON guard).
	if out := ExecuteTool("nope", json.RawMessage(`{`), ToolContext{}); !strings.Contains(out, "unknown tool") {
		t.Errorf("unknown tool with bad JSON = %q, want the unknown-tool message", out)
	}
}
