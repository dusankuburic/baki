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
		// The rows README.md's reasoning-floor sentence enumerates: keep
		// them pinned next to that wording.
		{[]string{"mai-ds-r1"}, reasoningOutputFloor},
		{[]string{"Phi-4-mini-reasoning"}, reasoningOutputFloor},
		{[]string{"gpt-5-pro"}, reasoningOutputFloor},
		{[]string{"gpt-5-codex"}, reasoningOutputFloor},
		// Each catalogued reasoning model is one by Microsoft Learn (the GPT-6/o-series tables, MAI-Thinking-1,
		// the Foundry reasoning guides, gpt-chat-latest's fixed level) or its vendor (DeepSeek's Speciale card;
		// grok-code-fast-1 "a speedy and economical reasoning model").
		{[]string{"gpt-6-astra"}, reasoningOutputFloor},
		{[]string{"gpt-6-sol"}, reasoningOutputFloor},
		{[]string{"gpt-6-luna"}, reasoningOutputFloor},
		{[]string{"gpt-6.1-sol"}, reasoningOutputFloor},
		{[]string{"gpt-chat-latest"}, reasoningOutputFloor},
		{[]string{"codex-mini"}, reasoningOutputFloor},
		{[]string{"codex-mini-2025-05-16"}, reasoningOutputFloor},
		{[]string{"MAI-Thinking-1"}, reasoningOutputFloor},
		{[]string{"DeepSeek-V3.2-Speciale"}, reasoningOutputFloor},
		{[]string{"DeepSeek-V4-Pro"}, reasoningOutputFloor},
		{[]string{"grok-code-fast-1"}, reasoningOutputFloor},
		// grok-3-mini reasons by default (xAI: "a lightweight model that thinks before responding"); grok-3 does not.
		{[]string{"grok-3-mini"}, reasoningOutputFloor},
		{[]string{"grok-3-mini-fast"}, reasoningOutputFloor},
		{[]string{"my-deployment", "MAI-Thinking-1"}, reasoningOutputFloor},
		{[]string{"claude:claude-opus-5-5"}, reasoningOutputFloor},
		{[]string{"claude-sonnet-5"}, reasoningOutputFloor},
		{[]string{"claude-fable-5-1"}, reasoningOutputFloor},
		{[]string{"claude-mythos-preview"}, reasoningOutputFloor},
		// Opus 4.7/4.8 are sent adaptive thinking: floored.
		{[]string{"claude-opus-4-8"}, reasoningOutputFloor},
		{[]string{"claude-opus-4-7"}, reasoningOutputFloor},
		// A model-router deployment always gets the floor: any call may be
		// routed to a reasoning model, whatever served the last one.
		{[]string{"model-router"}, reasoningOutputFloor},
		{[]string{"my-router", "model-router"}, reasoningOutputFloor}, // project-listing shape
		{[]string{"model-router-deployment", "gpt-4.1"}, reasoningOutputFloor},
		{[]string{"prod-model-router", "gpt-4.1-2025-04-14"}, reasoningOutputFloor}, // memo shape: last routed model was not a reasoner
		// A Claude deployment whose model nobody knows yet.
		{[]string{"claude:prod-opus"}, reasoningOutputFloor},
		{[]string{"claude:prod-opus", ""}, reasoningOutputFloor},
		{[]string{"claude:prod", "claude-opus-5-5"}, reasoningOutputFloor},
		// A custom deployment name is recognised through its served model.
		{[]string{"my-deployment", "gpt-5-mini-2025-08-07"}, reasoningOutputFloor},
		// Not reasoning models.
		{[]string{"gpt-5-chat"}, 0},
		{[]string{"gpt-4o"}, 0},
		{[]string{"gpt-4.1-mini"}, 0},
		{[]string{"grok-3"}, 0},
		{[]string{"Phi-4"}, 0},
		{[]string{"DeepSeek-V3-0324"}, 0},
		{[]string{"gpt-60-preview"}, 0},
		{[]string{"claude-sonnet-4-6"}, 0},
		{[]string{"claude-opus-4-6"}, 0},
		{[]string{"claude:prod-opus", "claude-haiku-4-5"}, 0}, // served model known and does not think
		{[]string{"claude:claude-sonnet-4-5"}, 0},
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
