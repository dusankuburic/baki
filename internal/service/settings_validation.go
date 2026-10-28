package service

import (
	"fmt"
	"math"
	"slices"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-core/models"
)

// The ai section's caps, mirrored by the frontend's constraints (frontend/src/lib/constraints.ts; a test compares
// them). The suffix is concatenated onto every system prompt, and the prompts are the chat's suggestion chips.
const (
	MaxSystemPromptSuffixChars = 4000
	MaxPromptsPerList          = 20
	MaxPromptChars             = 500
	// MaxOutputTokensSetting is the per-provider Max tokens field's ceiling.
	MaxOutputTokensSetting = 32000
)

// maxModelIDChars bounds a provider's default model id, as maxEmbeddingModelChars does the embedding model's.
const maxModelIDChars = maxEmbeddingModelChars

// maxTemperature bounds the per-provider temperature: every wire the providers speak accepts 0..2.
const maxTemperature = 2.0

// validateAISection checks the ai section of a member's settings write against stored, the section as it is
// stored now. A value equal to the stored one is not checked: the client posts the whole section it holds, so a
// value stored before a cap existed (or a provider id since retired) must not refuse the next write that changes
// something else. The deployment-wide embedding pair is checked like the admin writer's
// (validateDeploymentAISettings), apart from the operator-pin rule: on the desktop this route is that pair's
// writer, and a member's own document in cloud never reaches the indexer.
func validateAISection(next, stored models.AISettings) error {
	if next.SystemPromptSuffix != stored.SystemPromptSuffix {
		if n := utf8.RuneCountInString(next.SystemPromptSuffix); n > MaxSystemPromptSuffixChars {
			return fmt.Errorf("%w: ai.systemPromptSuffix is %d characters, at most %d", ErrSettingsInvalid, n, MaxSystemPromptSuffixChars)
		}
	}
	for _, list := range []struct {
		name         string
		next, stored []string
	}{
		{"block", next.Prompts.Block, stored.Prompts.Block},
		{"flow", next.Prompts.Flow, stored.Prompts.Flow},
		{"finding", next.Prompts.Finding, stored.Prompts.Finding},
		{"blockWithFindings", next.Prompts.BlockWithFindings, stored.Prompts.BlockWithFindings},
	} {
		if slices.Equal(list.next, list.stored) {
			continue
		}
		if len(list.next) > MaxPromptsPerList {
			return fmt.Errorf("%w: ai.prompts.%s has %d prompts, at most %d", ErrSettingsInvalid, list.name, len(list.next), MaxPromptsPerList)
		}
		for i, p := range list.next {
			if n := utf8.RuneCountInString(p); n > MaxPromptChars {
				return fmt.Errorf("%w: ai.prompts.%s[%d] is %d characters, at most %d", ErrSettingsInvalid, list.name, i, n, MaxPromptChars)
			}
		}
	}
	if next.ActiveProvider != stored.ActiveProvider && next.ActiveProvider != "" && !ai.KnownProvider(next.ActiveProvider) {
		return fmt.Errorf("%w: ai.activeProvider %q is not a provider this server knows", ErrSettingsInvalid, next.ActiveProvider)
	}
	if next.EmbeddingProvider != stored.EmbeddingProvider && next.EmbeddingProvider != "" && !ai.SupportsEmbeddings(next.EmbeddingProvider) {
		return fmt.Errorf("%w: ai.embeddingProvider %q is not a provider that can embed", ErrSettingsInvalid, next.EmbeddingProvider)
	}
	if next.EmbeddingModel != stored.EmbeddingModel {
		if n := utf8.RuneCountInString(next.EmbeddingModel); n > maxEmbeddingModelChars {
			return fmt.Errorf("%w: ai.embeddingModel is %d characters, at most %d", ErrSettingsInvalid, n, maxEmbeddingModelChars)
		}
	}
	ids := make([]string, 0, len(next.Providers))
	for id := range next.Providers {
		ids = append(ids, id)
	}
	slices.Sort(ids) // the first refusal named is the same on every write
	for _, id := range ids {
		if _, kept := stored.Providers[id]; !kept && !ai.KnownProvider(id) {
			return fmt.Errorf("%w: ai.providers.%s is not a provider this server knows", ErrSettingsInvalid, id)
		}
		if err := validateProviderConfig(id, next.Providers[id], stored.Providers[id]); err != nil {
			return err
		}
	}
	return nil
}

// validateProviderConfig checks one provider's per-turn parameters; a field equal to the stored one is kept.
// Use clamps or refuses these too (temperature and max tokens are sent as stored, the context budget is clamped
// to MaxContextTokenBudget, an unknown reasoning effort fails the turn): refused here, the bad value is named
// where it is entered instead.
func validateProviderConfig(id string, next, stored models.AIProviderConfig) error {
	field := func(name string) string { return "ai.providers." + id + "." + name }
	if next.Temperature != stored.Temperature { // NaN never equals the stored value
		if math.IsNaN(next.Temperature) || next.Temperature < 0 || next.Temperature > maxTemperature {
			return fmt.Errorf("%w: %s must be between 0 and %g", ErrSettingsInvalid, field("temperature"), maxTemperature)
		}
	}
	if next.MaxTokens != stored.MaxTokens && (next.MaxTokens < 0 || next.MaxTokens > MaxOutputTokensSetting) {
		return fmt.Errorf("%w: %s must be between 0 (the default) and %d", ErrSettingsInvalid, field("maxTokens"), MaxOutputTokensSetting)
	}
	if next.ContextTokenBudget != stored.ContextTokenBudget && (next.ContextTokenBudget < 0 || next.ContextTokenBudget > MaxContextTokenBudget) {
		return fmt.Errorf("%w: %s must be between 0 (derived from the model) and %d", ErrSettingsInvalid, field("contextTokenBudget"), MaxContextTokenBudget)
	}
	if next.DefaultModel != stored.DefaultModel {
		if n := utf8.RuneCountInString(next.DefaultModel); n > maxModelIDChars {
			return fmt.Errorf("%w: %s is %d characters, at most %d", ErrSettingsInvalid, field("defaultModel"), n, maxModelIDChars)
		}
	}
	if next.ReasoningEffort != stored.ReasoningEffort && !validReasoningEffort(next.ReasoningEffort) {
		return fmt.Errorf("%w: %s must be low, medium or high (empty for the provider default)", ErrSettingsInvalid, field("reasoningEffort"))
	}
	return nil
}
