package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/ai"
)

// S5: registry text shows every character it holds. Beyond control and format
// characters, the other default-ignorable code points (Hangul fillers, U+034F), variation
// selectors, U+2800 (a blank braille cell), private-use and unassigned code points are
// refused — they render as nothing, or differently everywhere, so "Admin" plus one of
// them could pass for "Admin" — and a name (or non-empty description) needs at least one
// visible letter, number, punctuation mark or symbol.
func TestAzureModels_TextRefusesInvisibleCharacters(t *testing.T) {
	refused := []string{
		"\u3164",                 // HANGUL FILLER
		"\u115F\u1160",           // HANGUL CHOSEONG / JUNGSEONG FILLER
		"\uFFA0",                 // HALFWIDTH HANGUL FILLER
		"\u2800",                 // BRAILLE PATTERN BLANK
		"Admin\u2800",            // …next to visible text
		"A\u034FB",               // COMBINING GRAPHEME JOINER
		"x\uFE0F",                // VARIATION SELECTOR-16
		"\U000E0100y",            // VARIATION SELECTOR-17
		"a\uFFF0",                // unassigned, default-ignorable
		"a\u0378",                // unassigned
		"a\uE000",                // private use
		"a\U0010FFFD",            // supplementary private use
		"a\u200Bb", "evil\u202E", // zero-width space, bidi override (as before)
	}
	for _, s := range refused {
		if _, err := validateConnectionName(s); azmCode(err) != AzureCodeInvalidConnectionName {
			t.Errorf("resource name %+q: %v, want INVALID_CONNECTION_NAME", s, err)
		}
		if _, _, err := normalizeAzurePurpose("custom", s); azmCode(err) != AzureCodeInvalidPurpose {
			t.Errorf("custom purpose %+q: %v, want INVALID_PURPOSE", s, err)
		}
		f := azureModelFields{ConnectionID: "default", Kind: "chat", Deployment: "gpt-4o", DisplayName: s}
		if err := validateModelFields(&f); azmCode(err) != AzureCodeInvalidDisplayName {
			t.Errorf("display name %+q: %v, want INVALID_DISPLAY_NAME", s, err)
		}
		f = azureModelFields{ConnectionID: "default", Kind: "chat", Deployment: "gpt-4o", Description: s}
		if err := validateModelFields(&f); azmCode(err) != AzureCodeInvalidDescription {
			t.Errorf("description %+q: %v, want INVALID_DESCRIPTION", s, err)
		}
		if ai.ValidAzureAgentName("agent" + s) {
			t.Errorf("agent name %+q accepted", "agent"+s)
		}
	}
	// Blank once cleaned: no visible character at all.
	for _, s := range []string{"\u3000", " \u00A0 "} {
		f := azureModelFields{ConnectionID: "default", Kind: "chat", Deployment: "gpt-4o", Description: s}
		if err := validateModelFields(&f); err != nil || f.Description != "" {
			t.Errorf("a whitespace-only description %+q = %q, %v; want cleared", s, f.Description, err)
		}
	}
	accepted := []string{"Café ☕", "日本語モデル", "Review · bot", "e\u0301clair", "R2-D2 (v1.0)", "∑ math"}
	for _, s := range accepted {
		if _, err := validateConnectionName(s); err != nil {
			t.Errorf("resource name %+q: %v", s, err)
		}
		f := azureModelFields{ConnectionID: "default", Kind: "chat", Deployment: "gpt-4o", DisplayName: s, Description: s}
		if err := validateModelFields(&f); err != nil {
			t.Errorf("display name / description %+q: %v", s, err)
		}
		if p, c, err := normalizeAzurePurpose("custom", s); err != nil || p != "custom" || c == "" {
			t.Errorf("custom purpose %+q = %q/%q, %v", s, p, c, err)
		}
	}
	if !ai.ValidAzureAgentName("Review bot") || !ai.ValidAzureAgentName("日本語") || ai.ValidAzureAgentName("\u3164") {
		t.Error("agent names: a visible name must pass, a filler must not")
	}

	// Through the API's create path, the same refusals.
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	if _, err := e.svc.CreateConnection(ctx, azmUser, AzureConnectionInput{Name: "\u3164", Config: azmKeyConfig("k")}); azmCode(err) != AzureCodeInvalidConnectionName {
		t.Errorf("create a resource named with a filler: %v", err)
	}
	c := e.mustConn(t, azmUser, "ok", azmKeyConfig("k"))
	if _, err := e.svc.CreateModel(ctx, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "agent", Deployment: "bot\u3164"}); azmCode(err) != ai.AzureCfgErrInvalidDeploy {
		t.Errorf("an agent named with a filler: %v, want INVALID_DEPLOYMENT", err)
	}
}
