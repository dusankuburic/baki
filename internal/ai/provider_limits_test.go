package ai

import (
	"context"
	"errors"
	"testing"
)

// limitsStub is a Provider whose catalog and limits are test-configurable.
type limitsStub struct {
	stubProvider
	models []ModelInfo
	err    error
	limit  int
}

func (s limitsStub) Models(_ context.Context) ([]ModelInfo, error) { return s.models, s.err }
func (s limitsStub) ContextLimit() int                             { return s.limit }

func TestContextLimitFor(t *testing.T) {
	cases := []struct {
		name     string
		stub     limitsStub
		modelID  string
		expected int
	}{
		{
			name: "catalog hit overrides provider default",
			stub: limitsStub{
				limit:  200_000,
				models: []ModelInfo{{ID: "big", ContextLimit: 1_000_000}},
			},
			modelID:  "big",
			expected: 1_000_000,
		},
		{
			name: "catalog entry without window falls back to provider limit",
			stub: limitsStub{
				limit:  200_000,
				models: []ModelInfo{{ID: "unknown-window"}},
			},
			modelID:  "unknown-window",
			expected: 200_000,
		},
		{
			name:     "model not in catalog falls back to provider limit",
			stub:     limitsStub{limit: 128_000, models: []ModelInfo{{ID: "other", ContextLimit: 8_192}}},
			modelID:  "missing",
			expected: 128_000,
		},
		{
			name:     "Models error falls back to provider limit",
			stub:     limitsStub{limit: 128_000, err: errors.New("boom")},
			modelID:  "any",
			expected: 128_000,
		},
		{
			name:     "empty model ID uses provider limit without catalog scan",
			stub:     limitsStub{limit: 131_072, models: []ModelInfo{{ID: "x", ContextLimit: 8}}},
			modelID:  "",
			expected: 131_072,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextLimitFor(context.Background(), tc.stub, tc.modelID); got != tc.expected {
				t.Errorf("ContextLimitFor(%q) = %d, want %d", tc.modelID, got, tc.expected)
			}
		})
	}
}

// TestContextLimitFor_RealCatalog spot-checks the motivating real-world case:
// Claude's provider-wide default (200k) under-reports its catalogued
// Sonnet/Opus 4.6 window (1M).
func TestContextLimitFor_RealCatalog(t *testing.T) {
	p := &ClaudeProvider{}
	if got := ContextLimitFor(context.Background(), p, "claude-sonnet-4-6"); got != 1_000_000 {
		t.Errorf("claude-sonnet-4-6 effective limit = %d, want 1000000 (catalog) not %d (provider default)", got, p.ContextLimit())
	}
	if got := ContextLimitFor(context.Background(), p, "claude-haiku-4-5"); got != 200_000 {
		t.Errorf("claude-haiku-4-5 effective limit = %d, want 200000", got)
	}
}
