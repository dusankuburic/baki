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
// output: OpenAI o-series (codex-mini, a fine-tuned o4-mini, included), the
// GPT-5 family (not gpt-5-chat, which does not reason) and GPT-6 family,
// gpt-chat-latest (a fixed, nonzero reasoning level), gpt-oss, DeepSeek R1 /
// MAI-DS-R1, DeepSeek-V3.2-Speciale (reasoning only) and DeepSeek-V4-Pro,
// MAI-Thinking-1, Grok 4 and grok-code-fast-1, Phi-4 reasoning, and the
// Claude families that think by default — each one a reasoning model by
// Microsoft Learn or its vendor's documentation. Matched case-insensitively
// against every name the caller knows for the model.
var reasoningModelRe = regexp.MustCompile(`(^|[^a-z0-9])(o1|o3|o4)([^a-z0-9]|$)|gpt-5(?:[.-]\d+)?(?:-(?:mini|nano|pro|codex))*(?:$|[^a-z])|gpt-6(?:$|[^0-9])|gpt-chat-latest|codex-mini|gpt-oss|deepseek-r1|mai-ds-r1|deepseek-v3[.-]2-speciale|deepseek-v4-pro|mai-thinking|grok-4|grok-code-fast|phi-4-(?:mini-)?reasoning|claude-(?:opus|sonnet)-5|claude-(?:fable|mythos)`)

// ReasoningOutputFloor returns the minimum max-tokens budget for a turn whose
// model is known by any of names (the request model id or deployment name,
// the served model), or 0 when none of them is a reasoning model. A
// "gpt-5-chat" name is never a reasoning model.
//
// A model-router deployment always gets the floor: the router may route any
// call to a reasoning model (its 2025-11-18 pool is mostly gpt-5.x / o4-mini
// / grok-4), and it forwards the caller's reasoning_effort to whichever
// model it picks — what served the LAST call says nothing about this one.
// The caller still caps the floor at the model's output ceiling and window.
//
// Claude deployments (a "claude:<name>" id, or a served "claude-…" model)
// are budgeted by what the name reveals: a family/version whose turns carry
// thinking (Opus 4.7+, every 5.x, Fable, Mythos — the same predicate
// buildBody uses to drop sampling and request adaptive thinking,
// claudeRemovesSampling) gets the floor; a name that reveals a
// non-thinking model (Haiku/Sonnet 4.5, Opus 4.6) does not; a Claude
// deployment whose model nobody knows yet (a custom name before its first
// answer is memoised) always gets the floor — thinking models (Opus 5 /
// 5.5, Sonnet 5) think by default, and the first turn at the 4096 default
// frequently ends max_tokens with no answer. Over-flooring a custom-named
// Haiku/Sonnet 4.5 deployment is harmless: the floor only raises the
// request cap and stays under their 64K ceiling.
func ReasoningOutputFloor(names ...string) int {
	claude, known := false, false
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || strings.Contains(n, "gpt-5-chat") {
			continue
		}
		if isModelRouterAlias(n) || reasoningModelRe.MatchString(n) {
			return reasoningOutputFloor
		}
		if IsClaudeModel(n) {
			claude = true
			n = claudeDeploymentName(n)
		}
		// A Claude name that reveals its family/version: adaptive thinking
		// is sent (Opus 4.7+, on every path since F-17) or on by default
		// (5.x, Fable, Mythos) exactly when buildBody drops sampling —
		// claudeRemovesSampling is that predicate.
		if claudeModelVersionRe.MatchString(n) || claudeAlwaysThinkingFamilyRe.MatchString(n) {
			known = true
			if claudeRemovesSampling(n) {
				return reasoningOutputFloor
			}
		}
	}
	if claude && !known {
		return reasoningOutputFloor // a Claude deployment whose model nobody knows yet
	}
	return 0
}
