package ai

import (
	"regexp"
	"strings"
)

// ReasoningOutputFloor is the smallest max-tokens budget a turn on a model
// that spends its output budget on hidden reasoning should get. Both vendors
// count the reasoning against the same cap as the answer: Azure's reasoning
// guide recommends reserving tens of thousands of tokens for o-series/GPT-5
// ("reserve at least 25,000 tokens for reasoning and output"), and Anthropic's
// migration guides say max_tokens is a hard limit on thinking plus text for
// the models that think by default (Opus 5 / 5.5, Sonnet 5 / 5.5, Fable,
// Mythos). With the app's 4096-token default such a turn often spends the
// whole budget thinking and ends with no answer (PROVIDER_OUTPUT_LIMIT) while
// still being billed.
//
// The floor only raises a smaller request; the caller still caps it at the
// model's own output ceiling and the context window.
const reasoningOutputFloor = 16384

// reasoningModelRe matches model ids (or deployment names that follow them,
// the Foundry portal default) of models whose hidden reasoning is billed as
// output: OpenAI o-series and GPT-5 family (not gpt-5-chat, which does not
// reason), gpt-oss, DeepSeek R1 / MAI-DS-R1, Grok 4, Phi-4 reasoning, and the
// Claude families that think by default. Matched case-insensitively against
// every name the caller knows for the model.
var reasoningModelRe = regexp.MustCompile(`(^|[^a-z0-9])(o1|o3|o4)([^a-z0-9]|$)|gpt-5(?:[.-]\d+)?(?:-(?:mini|nano|pro|codex))*(?:$|[^a-z])|gpt-oss|deepseek-r1|mai-ds-r1|grok-4|phi-4-(?:mini-)?reasoning|claude-(?:opus|sonnet)-5|claude-(?:fable|mythos)`)

// ReasoningOutputFloor returns the minimum max-tokens budget for a turn whose
// model is known by any of names (the request model id or deployment name,
// the served model), or 0 when none of them is a reasoning model. A
// "gpt-5-chat" name is never a reasoning model.
func ReasoningOutputFloor(names ...string) int {
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || strings.Contains(n, "gpt-5-chat") {
			continue
		}
		if reasoningModelRe.MatchString(n) {
			return reasoningOutputFloor
		}
	}
	return 0
}
