package ai

import "testing"

func TestReasoningOutputFloor(t *testing.T) {
	cases := []struct {
		names []string
		want  int
	}{
		{[]string{"o1"}, reasoningOutputFloor},
		{[]string{"o3-mini"}, reasoningOutputFloor},
		{[]string{"o4-mini-2025-04-16"}, reasoningOutputFloor},
		{[]string{"gpt-5"}, reasoningOutputFloor},
		{[]string{"gpt-5-mini"}, reasoningOutputFloor},
		{[]string{"gpt-5.6"}, reasoningOutputFloor},
		{[]string{"GPT-5-Nano"}, reasoningOutputFloor},
		{[]string{"gpt-oss-120b"}, reasoningOutputFloor},
		{[]string{"DeepSeek-R1"}, reasoningOutputFloor},
		{[]string{"grok-4"}, reasoningOutputFloor},
		{[]string{"Phi-4-reasoning"}, reasoningOutputFloor},
		{[]string{"claude:claude-opus-5-5"}, reasoningOutputFloor},
		{[]string{"claude-sonnet-5"}, reasoningOutputFloor},
		{[]string{"claude-fable-5-1"}, reasoningOutputFloor},
		{[]string{"claude-mythos-preview"}, reasoningOutputFloor},
		// A custom deployment name is recognised through its served model.
		{[]string{"my-deployment", "gpt-5-mini-2025-08-07"}, reasoningOutputFloor},
		// Not reasoning models.
		{[]string{"gpt-5-chat"}, 0},
		{[]string{"gpt-4o"}, 0},
		{[]string{"gpt-4.1-mini"}, 0},
		{[]string{"claude-sonnet-4-6"}, 0},
		{[]string{"claude-opus-4-8"}, 0},
		{[]string{"model-router"}, 0},
		{[]string{"proto1"}, 0},
		{[]string{"embedding-o3x"}, 0},
		{[]string{"", "  "}, 0},
		{nil, 0},
	}
	for _, c := range cases {
		if got := ReasoningOutputFloor(c.names...); got != c.want {
			t.Errorf("ReasoningOutputFloor(%q) = %d, want %d", c.names, got, c.want)
		}
	}
}
