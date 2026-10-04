package service

import (
	"testing"

	"pad-core/models"
)

// TestProviderDisabledByPolicy pins the AIProviderConfig.Enabled enforcement
// semantics. The flag is a plain bool that no UI writes, and both
// DefaultSettings and the frontend's store ship it false for every provider
// but the default active one (claude) — so "stored false" is indistinguishable
// from "unset" and must read as ENABLED for those providers. Only an explicit
// disable of a provider the defaults enable may refuse the turn.
func TestProviderDisabledByPolicy(t *testing.T) {
	// claude is the one provider DefaultSettings ships enabled; openai is the
	// representative every-other-provider (shipped false).
	claudeEnabled := *models.DefaultSettings()

	settingsWith := func(id string, enabled bool) *models.AppSettings {
		s := models.DefaultSettings().Clone()
		pc := s.AI.Providers[id]
		pc.Enabled = enabled
		s.AI.Providers[id] = pc
		return s
	}

	cases := []struct {
		name     string
		settings *models.AppSettings
		provider string
		want     bool
	}{
		{"no settings wired", nil, "claude", false},
		{"default settings, claude", &claudeEnabled, "claude", false},
		{"default settings, openai", &claudeEnabled, "openai", false},
		{"absent entry", &models.AppSettings{}, "openai", false},
		{"explicitly enabled", settingsWith("openai", true), "openai", false},
		// The one unambiguous disable: the defaults enable the provider and
		// the stored document says false.
		{"claude disabled by policy", settingsWith("claude", false), "claude", true},
		// A stored false on a provider whose default is false is the shipped
		// default, not a policy decision.
		{"openai stored false stays enabled", settingsWith("openai", false), "openai", false},
		// A provider the defaults do not enumerate: its entry exists only
		// because something wrote it, but false is still the value every
		// shipped default carries for it.
		{"unenumerated provider stored false", settingsWith("made-up", false), "made-up", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := providerDisabledByPolicy(c.settings, c.provider); got != c.want {
				t.Errorf("providerDisabledByPolicy(%q) = %v, want %v", c.provider, got, c.want)
			}
		})
	}
}
