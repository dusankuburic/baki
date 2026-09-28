package ai

import (
	"strings"
	"testing"
)

// R-9: only alias providers record a served model on the usage row; a
// dated answer that normalises to the requested id is blank (it split
// analytics into "gpt-4o" and "gpt-4o · gpt-4o-2024-08-06"), and an
// upstream's arbitrarily long "model" value is capped.
func TestUsageServedModel_Gating(t *testing.T) {
	long := strings.Repeat("x", 200)
	multi := strings.Repeat("é", 100) // 200 bytes, 2 per rune
	cases := []struct {
		name, provider, model, served, want string
	}{
		{"non-alias provider never records", "openai", "gpt-4o", "gpt-4o-2024-08-06", ""},
		{"non-alias provider, other model", "github-models", "gpt-4o", "gpt-4o-mini", ""},
		{"alias, dated answer of the same model", azureFoundryProviderID, "gpt-4o", "gpt-4o-2024-08-06", ""},
		{"alias, same model other case", azureFoundryProviderID, "GPT-4o", " gpt-4o ", ""},
		{"alias, deployment name differs", azureFoundryProviderID, "my-deploy", "gpt-4o-2024-08-06", "gpt-4o-2024-08-06"},
		{"alias, nothing reported", azureFoundryProviderID, "my-deploy", "", ""},
		{"alias, capped", azureFoundryProviderID, "my-deploy", long, long[:maxUsageServedModel]},
	}
	for _, tc := range cases {
		if got := usageServedModel(tc.provider, tc.model, tc.served); got != tc.want {
			t.Errorf("%s: usageServedModel(%q, %q, %q) = %q, want %q", tc.name, tc.provider, tc.model, tc.served, got, tc.want)
		}
	}
	got := usageServedModel(azureFoundryProviderID, "my-deploy", multi)
	if len(got) > maxUsageServedModel || !strings.HasPrefix(multi, got) || len(got)%2 != 0 {
		t.Errorf("multi-byte cap = %d bytes (%q), want ≤ %d on a rune boundary", len(got), got, maxUsageServedModel)
	}
}

// The gate reaches the recorded row: a plain provider's dated answer leaves
// ServedModel empty.
func TestAudited_NonAliasProviderRecordsNoServedModel(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "known", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		resp:   &Response{TokensIn: 10, TokensOut: 5, ServedModel: "known-2024-08-06"},
	}
	if m := awaitMetric(t, stub, Request{Model: "known"}); m.ServedModel != "" {
		t.Errorf("ServedModel = %q, want empty for a provider whose ids are model names", m.ServedModel)
	}
}
