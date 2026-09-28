package ai

import (
	"context"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// usageOrgStub is a raw provider that answers Chat and Embed with fixed
// usage, so the audited chain records a row the tests can inspect.
type usageOrgStub struct {
	Provider
	key string
}

func (s *usageOrgStub) ID() string                  { return "usage-org-stub" }
func (s *usageOrgStub) Name() string                { return "usage-org-stub" }
func (s *usageOrgStub) ContextLimit() int           { return 8192 }
func (s *usageOrgStub) SupportsTools() bool         { return false }
func (s *usageOrgStub) DefaultModel() string        { return "m1" }
func (s *usageOrgStub) FreeModel() string           { return "" }
func (s *usageOrgStub) EstimateTokens(t string) int { return len(t) }
func (s *usageOrgStub) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 1, OutputCostPerM: 1}
}
func (s *usageOrgStub) Models(context.Context) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m1", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}}, nil
}
func (s *usageOrgStub) Chat(context.Context, Request) (*Response, error) {
	return &Response{Content: "ok", TokensIn: 10, TokensOut: 2}, nil
}
func (s *usageOrgStub) Embed(_ context.Context, text []string) ([][]float32, error) {
	out := make([][]float32, len(text))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

var registerUsageOrgStubOnce sync.Once

func registerUsageOrgStub() {
	registerUsageOrgStubOnce.Do(func() {
		RegisterProvider("usage-org-stub", func(k string) Provider { return &usageOrgStub{key: k} })
	})
}

// usageOrgFactory is orgTestFactory with a recorder that forwards every
// usage row to the returned channel.
func usageOrgFactory(members, orgs map[string]string) (*ProviderFactory, <-chan *interfaces.UsageMetric) {
	ch := make(chan *interfaces.UsageMetric, 8)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error { ch <- m; return nil }
	f := NewProviderFactory(func(scope, _ string) (string, error) { return members[scope], nil }, nil, rec, nil)
	f.SetOrgKeyLookup(func(orgID, _ string) (string, error) { return orgs[orgID], nil })
	return f, ch
}

func nextUsage(t *testing.T, ch <-chan *interfaces.UsageMetric) *interfaces.UsageMetric {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no usage row was recorded")
		return nil
	}
}

// R-4: a chain built from an org's shared credential attributes every usage
// row that carries no org of its own — embeddings (Embed has no Request) and
// the admin's connection test (a Request without OrgID) — to that org, so the
// spend is capped by the org's budget; a chain on the member's own key keeps
// attributing embeddings to the member alone.
func TestFactory_OrgCredentialUsageIsOrgAttributed(t *testing.T) {
	registerUsageOrgStub()
	f, ch := usageOrgFactory(map[string]string{"u-own": "member-key"}, map[string]string{"org-1": "org-key"})

	t.Run("embedding on the org credential", func(t *testing.T) {
		p, err := f.ForEmbeddingOrg("u-none", "org-1", "usage-org-stub", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Embed(context.Background(), []string{"chunk text"}); err != nil {
			t.Fatal(err)
		}
		if m := nextUsage(t, ch); m.OrgID != "org-1" || m.UserID != "u-none" {
			t.Errorf("usage row = org %q user %q, want org-1 / u-none", m.OrgID, m.UserID)
		}
	})

	t.Run("chat fallback on the org credential without a request org", func(t *testing.T) {
		p, err := f.ForOrg("u-none", "org-1", "usage-org-stub")
		if err != nil {
			t.Fatal(err)
		}
		if !UsedOrgCredential(p) {
			t.Error("UsedOrgCredential = false for a chain on the org's credential")
		}
		if _, err := p.Chat(context.Background(), Request{Model: "m1"}); err != nil {
			t.Fatal(err)
		}
		if m := nextUsage(t, ch); m.OrgID != "org-1" {
			t.Errorf("usage org = %q, want org-1", m.OrgID)
		}
	})

	t.Run("admin connection test", func(t *testing.T) {
		p, err := f.ForOrgCredential("u-own", "org-1", "usage-org-stub")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Chat(context.Background(), Request{Model: "m1"}); err != nil {
			t.Fatal(err)
		}
		if m := nextUsage(t, ch); m.OrgID != "org-1" || m.UserID != "u-own" {
			t.Errorf("usage row = org %q user %q, want org-1 / u-own", m.OrgID, m.UserID)
		}
	})

	t.Run("a request org still wins", func(t *testing.T) {
		p, err := f.ForOrg("u-none", "org-1", "usage-org-stub")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Chat(context.Background(), Request{Model: "m1", OrgID: "org-1"}); err != nil {
			t.Fatal(err)
		}
		if m := nextUsage(t, ch); m.OrgID != "org-1" {
			t.Errorf("usage org = %q, want org-1", m.OrgID)
		}
	})

	t.Run("member's own key stays personal", func(t *testing.T) {
		p, err := f.ForEmbeddingOrg("u-own", "org-2", "usage-org-stub", "")
		if err != nil {
			t.Fatal(err)
		}
		if UsedOrgCredential(p) {
			t.Error("UsedOrgCredential = true for the member's own key")
		}
		if _, err := p.Embed(context.Background(), []string{"chunk"}); err != nil {
			t.Fatal(err)
		}
		if m := nextUsage(t, ch); m.OrgID != "" {
			t.Errorf("usage org = %q, want personal (no org credential)", m.OrgID)
		}
		own, err := f.ForOrg("u-own", "org-1", "usage-org-stub")
		if err != nil {
			t.Fatal(err)
		}
		if UsedOrgCredential(own) {
			t.Error("member-first: the member's own key must not count as the org's")
		}
	})

	if UsedOrgCredential(&usageOrgStub{}) {
		t.Error("a bare provider never used an org credential")
	}
}
