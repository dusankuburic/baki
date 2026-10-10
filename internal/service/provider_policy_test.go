package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// orgSettingsStub answers LoadOrgSettings from a map, or with err.
type orgSettingsStub struct {
	docs map[string]*storageif.AppSettings
	err  error
}

func (s orgSettingsStub) LoadOrgSettings(_ context.Context, orgID string) (*storageif.AppSettings, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.docs[orgID], nil
}

// ADR 0028: the deployment's Enabled flag refuses a provider for everyone (the one unambiguous disable, as the
// chat gate read it); an organisation's AllowedProviders refuses a provider for builds that name the org and
// nothing else; an empty list allows every provider; an unreadable org document is an error of its own, never
// "allowed" and never "not allowed".
func TestProviderPolicy_OrgAllowlistAndDeploymentDisable(t *testing.T) {
	ctx := context.Background()
	deployment := models.DefaultSettings().Clone()
	claude := deployment.AI.Providers["claude"]
	claude.Enabled = false
	deployment.AI.Providers["claude"] = claude
	orgs := orgSettingsStub{docs: map[string]*storageif.AppSettings{
		"org-list": {AI: storageif.AISettings{AllowedProviders: []string{"openai", "gemini"}}},
		"org-open": {AI: storageif.AISettings{}},
	}}
	policy := NewProviderPolicy(&staticSettings{s: *deployment}, orgs)

	cases := []struct {
		name, orgID, provider string
		refused               bool
	}{
		{"disabled in the deployment, personal", "", "claude", true},
		{"disabled in the deployment, org that lists it", "org-list", "claude", true},
		{"shipped-false default stays enabled", "", "openai", false},
		{"on the org list", "org-list", "openai", false},
		{"off the org list", "org-list", "xai", true},
		{"org without a list", "org-open", "xai", false},
		{"org without a document", "org-none", "xai", false},
		{"personal build ignores every list", "", "xai", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := policy.Allowed(ctx, "u1", c.orgID, c.provider)
			if errors.Is(err, ai.ErrProviderNotAllowed) != c.refused {
				t.Fatalf("Allowed(%q, %q) = %v, want refused=%v", c.orgID, c.provider, err, c.refused)
			}
			if err != nil && !strings.Contains(err.Error(), c.provider) {
				t.Errorf("the refusal does not name the provider: %q", err.Error())
			}
		})
	}

	// The org store unreadable: fail closed with the store's class, not the policy's.
	broken := NewProviderPolicy(&staticSettings{s: *deployment}, orgSettingsStub{err: errors.New("db down")})
	if err := broken.Allowed(ctx, "u1", "org-list", "openai"); err == nil || errors.Is(err, ai.ErrProviderNotAllowed) {
		t.Errorf("unreadable org settings: %v, want an error that is not ErrProviderNotAllowed", err)
	}
	// No settings, no org store (the desktop): everything allowed.
	if err := NewProviderPolicy(nil, nil).Allowed(ctx, "u1", "org-list", "claude"); err != nil {
		t.Errorf("bare policy: %v", err)
	}
	// The storage document round-trips the list (the org settings route saves models.AppSettings).
	backend := testutil.NewFakeBackend()
	backend.Orgs = map[string]*storageif.Organisation{"org-1": {ID: "org-1"}}
	_ = backend.SaveOrgSettings(ctx, "org-1", settingsFromModel(&models.AppSettings{AI: models.AISettings{AllowedProviders: []string{"glm"}}}))
	stored := NewProviderPolicy(nil, backend)
	if err := stored.Allowed(ctx, "u1", "org-1", "openai"); !errors.Is(err, ai.ErrProviderNotAllowed) {
		t.Errorf("list saved through the settings document: %v, want refused", err)
	}
	if err := stored.Allowed(ctx, "u1", "org-1", "glm"); err != nil {
		t.Errorf("listed provider: %v", err)
	}
}
