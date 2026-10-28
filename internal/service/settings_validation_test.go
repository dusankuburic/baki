package service

// R-config-2: the ai section of a member's settings write is checked before it is stored, and a value the write
// carries back unchanged is never refused (a document stored before a cap existed stays writable).

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

func TestValidateAISection(t *testing.T) {
	base := models.DefaultSettings().AI
	prompts := func(n int, text string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = text
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		mutate func(*models.AISettings)
		want   string // in the error, "" = accepted
	}{
		{"the defaults", func(*models.AISettings) {}, ""},
		{"a suffix at the cap", func(a *models.AISettings) { a.SystemPromptSuffix = strings.Repeat("é", MaxSystemPromptSuffixChars) }, ""},
		{"a 64 KiB suffix", func(a *models.AISettings) { a.SystemPromptSuffix = strings.Repeat("x", 64<<10) }, "ai.systemPromptSuffix"},
		{"a prompt list at the cap", func(a *models.AISettings) { a.Prompts.Flow = prompts(MaxPromptsPerList, "p") }, ""},
		{"a prompt list over the cap", func(a *models.AISettings) { a.Prompts.Block = prompts(MaxPromptsPerList+1, "p") }, "ai.prompts.block"},
		{"an oversized prompt", func(a *models.AISettings) { a.Prompts.Finding = []string{strings.Repeat("p", MaxPromptChars+1)} }, "ai.prompts.finding[0]"},
		{"an oversized prompt in the last list", func(a *models.AISettings) {
			a.Prompts.BlockWithFindings = []string{"ok", strings.Repeat("p", MaxPromptChars+1)}
		}, "ai.prompts.blockWithFindings[1]"},
		{"an unknown active provider", func(a *models.AISettings) { a.ActiveProvider = "nope" }, "ai.activeProvider"},
		{"an embedding provider that cannot embed", func(a *models.AISettings) { a.EmbeddingProvider = "claude" }, "ai.embeddingProvider"},
		{"an oversized embedding model", func(a *models.AISettings) { a.EmbeddingModel = strings.Repeat("m", maxEmbeddingModelChars+1) }, "ai.embeddingModel"},
		{"a temperature above 2", func(a *models.AISettings) {
			setProvider(a, "claude", func(c *models.AIProviderConfig) { c.Temperature = 2.5 })
		}, "ai.providers.claude.temperature"},
		{"a negative temperature", func(a *models.AISettings) {
			setProvider(a, "openai", func(c *models.AIProviderConfig) { c.Temperature = -0.1 })
		}, "ai.providers.openai.temperature"},
		{"a NaN temperature", func(a *models.AISettings) {
			setProvider(a, "openai", func(c *models.AIProviderConfig) { c.Temperature = math.NaN() })
		}, "ai.providers.openai.temperature"},
		{"negative max tokens", func(a *models.AISettings) {
			setProvider(a, "claude", func(c *models.AIProviderConfig) { c.MaxTokens = -1 })
		}, "ai.providers.claude.maxTokens"},
		{"max tokens over the cap", func(a *models.AISettings) {
			setProvider(a, "claude", func(c *models.AIProviderConfig) { c.MaxTokens = MaxOutputTokensSetting + 1 })
		}, "ai.providers.claude.maxTokens"},
		{"a context budget over the cap", func(a *models.AISettings) {
			setProvider(a, "claude", func(c *models.AIProviderConfig) { c.ContextTokenBudget = MaxContextTokenBudget + 1 })
		}, "ai.providers.claude.contextTokenBudget"},
		{"a negative context budget", func(a *models.AISettings) {
			setProvider(a, "claude", func(c *models.AIProviderConfig) { c.ContextTokenBudget = -5 })
		}, "ai.providers.claude.contextTokenBudget"},
		{"an unknown reasoning effort", func(a *models.AISettings) {
			setProvider(a, "claude", func(c *models.AIProviderConfig) { c.ReasoningEffort = "max" })
		}, "ai.providers.claude.reasoningEffort"},
		{"a changed provider entry within bounds", func(a *models.AISettings) {
			setProvider(a, "xai", func(c *models.AIProviderConfig) { c.Temperature, c.MaxTokens, c.ReasoningEffort = 0.7, 8000, "high" })
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := cloneAI(base)
			tc.mutate(&next)
			err := validateAISection(next, base)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrSettingsInvalid) {
				t.Fatalf("err = %v, want ErrSettingsInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// A value already stored is carried back by every write of the whole section (the client posts the section it
// holds), so a cap added after it was stored must not refuse the next write that changes something else.
func TestValidateAISection_StoredValuesAreKept(t *testing.T) {
	stored := models.DefaultSettings().AI
	stored.SystemPromptSuffix = strings.Repeat("x", MaxSystemPromptSuffixChars+10)
	stored.Prompts.Flow = make([]string, MaxPromptsPerList+3)
	stored.ActiveProvider = "retired-provider"
	setProvider(&stored, "claude", func(c *models.AIProviderConfig) { c.ContextTokenBudget = 200_000 })

	next := cloneAI(stored)
	next.SaveConversationHistory = !stored.SaveConversationHistory
	if err := validateAISection(next, stored); err != nil {
		t.Fatalf("a write that changes only saveConversationHistory was refused: %v", err)
	}

	next = cloneAI(stored)
	next.SystemPromptSuffix += "y"
	if err := validateAISection(next, stored); !errors.Is(err, ErrSettingsInvalid) {
		t.Errorf("an oversized suffix that changed: err = %v, want ErrSettingsInvalid", err)
	}
}

// Done when: POST /api/system/settings with a 64 KiB systemPromptSuffix answers ErrSettingsInvalid naming the field,
// and the stored value is unchanged (the deployment's document on the desktop, the member's own in cloud).
func TestUpdateUserSettings_RefusesAnOversizedSuffix(t *testing.T) {
	ctx := context.Background()
	settings := storage.NewInMemorySettingsStore()
	sys := NewSystemService(settings, nil, NilNotifier{}, nil, "local", nil, nil)

	in := *models.DefaultSettings()
	in.AI.SystemPromptSuffix = strings.Repeat("x", 64<<10)
	err := sys.UpdateUserSettings(ctx, "", in, SettingsSections{"ai": true})
	if !errors.Is(err, ErrSettingsInvalid) || !strings.Contains(err.Error(), "ai.systemPromptSuffix") {
		t.Fatalf("err = %v, want ErrSettingsInvalid naming ai.systemPromptSuffix", err)
	}
	if got := settings.Get().AI.SystemPromptSuffix; got != "" {
		t.Errorf("stored suffix = %d bytes, want it unchanged", len(got))
	}

	// A write that does not name the ai section is not refused for an ai value it does not carry.
	if err := sys.UpdateUserSettings(ctx, "", in, SettingsSections{"profile": true}); err != nil {
		t.Errorf("a profile-only write was refused for the ai section it leaves out: %v", err)
	}
}

func setProvider(a *models.AISettings, id string, edit func(*models.AIProviderConfig)) {
	c := a.Providers[id]
	edit(&c)
	a.Providers[id] = c
}

// cloneAI copies the section deeply enough for a test to edit the copy's maps and lists.
func cloneAI(a models.AISettings) models.AISettings {
	out := a
	out.Providers = make(map[string]models.AIProviderConfig, len(a.Providers))
	for k, v := range a.Providers {
		out.Providers[k] = v
	}
	out.Prompts.Block = append([]string(nil), a.Prompts.Block...)
	out.Prompts.Flow = append([]string(nil), a.Prompts.Flow...)
	out.Prompts.Finding = append([]string(nil), a.Prompts.Finding...)
	out.Prompts.BlockWithFindings = append([]string(nil), a.Prompts.BlockWithFindings...)
	return out
}

func TestValidateAISection_ProviderEntries(t *testing.T) {
	stored := models.DefaultSettings().AI
	next := cloneAI(stored)
	next.Providers["junk-1"] = models.AIProviderConfig{}
	if err := validateAISection(next, stored); !errors.Is(err, ErrSettingsInvalid) || !strings.Contains(err.Error(), "ai.providers.junk-1") {
		t.Errorf("a new unknown provider entry: err = %v, want it refused by name", err)
	}
	stored.Providers["retired"] = models.AIProviderConfig{Temperature: 0.5}
	next = cloneAI(stored)
	if err := validateAISection(next, stored); err != nil {
		t.Errorf("a stored entry for a retired provider was refused: %v", err)
	}
	next = cloneAI(stored)
	setProvider(&next, "claude", func(c *models.AIProviderConfig) { c.DefaultModel = strings.Repeat("m", maxModelIDChars+1) })
	if err := validateAISection(next, stored); !errors.Is(err, ErrSettingsInvalid) || !strings.Contains(err.Error(), "ai.providers.claude.defaultModel") {
		t.Errorf("an oversized default model: err = %v", err)
	}
}

// Review: the org route checks the ai section too, on the desktop (where it writes the deployment's document) and
// against the org's stored document in cloud; and the member route's cloud branch refuses like the desktop's.
func TestSettingsWrites_EveryWriterChecksTheAISection(t *testing.T) {
	ctx := context.Background()
	big := *models.DefaultSettings()
	big.AI.SystemPromptSuffix = strings.Repeat("x", 64<<10)

	desktop := NewSystemService(storage.NewInMemorySettingsStore(), nil, NilNotifier{}, nil, "local", nil, nil)
	if err := desktop.UpdateOrgSettings(ctx, "org-1", big, OrgSettingsWrite{Sections: SettingsSections{"ai": true}}); !errors.Is(err, ErrSettingsInvalid) {
		t.Errorf("desktop org route: err = %v, want ErrSettingsInvalid", err)
	}

	backend := testutil.NewFakeBackend()
	backend.SeedOrgs("org-1")
	cloud := NewSystemService(storage.NewInMemorySettingsStore(), nil, NilNotifier{}, backend, "cloud", nil, nil)
	if err := cloud.UpdateOrgSettings(ctx, "org-1", big, OrgSettingsWrite{Sections: SettingsSections{"ai": true}}); !errors.Is(err, ErrSettingsInvalid) {
		t.Errorf("cloud org route: err = %v, want ErrSettingsInvalid", err)
	}
	if err := cloud.UpdateUserSettings(ctx, "user-1", big, SettingsSections{"ai": true}); !errors.Is(err, ErrSettingsInvalid) {
		t.Errorf("cloud member route: err = %v, want ErrSettingsInvalid", err)
	}
	if stored, err := cloud.GetUserSettings(ctx, "user-1"); err == nil && stored.AI.SystemPromptSuffix != "" {
		t.Errorf("cloud member route stored the refused suffix (%d bytes)", len(stored.AI.SystemPromptSuffix))
	}
}
