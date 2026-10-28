package ai

import (
	"regexp"
	"strings"
)

// reasoningOutputFloor is the smallest max-tokens budget a turn on a reasoning model should get. Both vendors
// count hidden reasoning against the same cap as the answer (Azure: "reserve at least 25,000 tokens for
// reasoning and output"; Anthropic: max_tokens covers thinking plus text) — with the app's 4096 default such
// a turn often spends the whole budget thinking and ends billed with no answer (PROVIDER_OUTPUT_LIMIT). The
// floor only raises a smaller request; the caller still caps it at the ceiling and window.
const reasoningOutputFloor = 16384

// reasoningModelRe matches model ids (or deployment names that follow them) whose hidden reasoning is billed
// as output: OpenAI o-series, GPT-5 (not gpt-5-chat) and GPT-6, gpt-chat-latest, gpt-oss, codex-mini,
// DeepSeek R1 / MAI-DS-R1 / Speciale / V4-Pro, MAI-Thinking, Grok 4, grok-3-mini and grok-code-fast, Phi-4 reasoning, the
// Claude families that think by default, and the Gemini models that think by default (2.5, 3 and the
// "-thinking" ids), whose thoughts Gemini bills as output.
var reasoningModelRe = regexp.MustCompile(`(^|[^a-z0-9])(o1|o3|o4)([^a-z0-9]|$)|gpt-5(?:[.-]\d+)?(?:-(?:mini|nano|pro|codex))*(?:$|[^a-z])|gpt-6(?:$|[^0-9])|gpt-chat-latest|codex-mini|gpt-oss|deepseek-r1|mai-ds-r1|deepseek-v3[.-]2-speciale|deepseek-v4-pro|mai-thinking|grok-4|grok-3-mini|grok-code-fast|phi-4-(?:mini-)?reasoning|claude-(?:opus|sonnet)-5|claude-(?:fable|mythos)|gemini-(?:2\.5|3)(?:[.-]|$)|gemini-[\w.-]*thinking`)

// ReasoningOutputFloor returns the minimum max-tokens budget for a turn whose model is known by any of names,
// 0 when none is a reasoning model ("gpt-5-chat" never is). A model-router deployment always gets the floor —
// the router may route any call to a reasoning model. A Claude deployment is budgeted by what its name
// reveals (claudeRemovesSampling's predicate); an unknown Claude deployment always gets the floor, since
// thinking models think by default and the 4096 default frequently ends max_tokens with no answer.
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
		// A Claude name that reveals its family/version: thinking is sent or on by
		// default exactly when buildBody drops sampling (claudeRemovesSampling).
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
