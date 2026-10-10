package ai

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// keyedStub records the key it was constructed with so a test can tell which
// credential the factory resolved.
type keyedStub struct {
	Provider
	key string
}

func (s *keyedStub) ID() string { return "org-stub" }

var registerOrgStubOnce sync.Once

func registerOrgStub() {
	registerOrgStubOnce.Do(func() {
		RegisterProvider("org-stub", func(k string) Provider { return &keyedStub{key: k} })
	})
}

// orgTestFactory resolves member keys from members and org keys from orgs
// (both keyed by scope/org id → key; missing = "").
func orgTestFactory(members, orgs map[string]string) *ProviderFactory {
	f := NewProviderFactory(func(scope, _ string) (string, error) { return members[scope], nil }, nil, nil, nil)
	if orgs != nil {
		f.SetOrgKeyLookup(func(orgID, _ string) (string, error) { return orgs[orgID], nil })
	}
	return f
}

func resolvedKey(t *testing.T, p Provider) (key, memoScope string) {
	t.Helper()
	ap, ok := p.(*auditedProvider)
	if !ok {
		t.Fatalf("provider is %T, want the audited chain", p)
	}
	raw, ok := unwrapProvider(ap).(*keyedStub)
	if !ok {
		t.Fatalf("raw provider is %T, want *keyedStub", unwrapProvider(ap))
	}
	return raw.key, ap.memoScope
}

// unwrapProvider walks the audited → traced → retrying → breaker chain down
// to the raw provider.
func unwrapProvider(p Provider) Provider {
	for {
		switch w := p.(type) {
		case *auditedProvider:
			p = w.inner
		case *TracedProvider:
			p = w.Provider
		case *RetryingProvider:
			p = w.Provider
		case *CircuitBreakerProvider:
			p = w.Provider
		default:
			return p
		}
	}
}

// G7: ForOrg is member-first — the member's own key wins, the org's is the
// fallback and only it runs under OrgScope; nothing at all is
// ErrKeyNotConfigured, and an empty org id or no org store is plain For.
func TestProviderFactory_ForOrg_MemberFirst(t *testing.T) {
	registerOrgStub()
	f := orgTestFactory(map[string]string{"u-own": "member-key"}, map[string]string{"org-1": "org-key"})

	p, err := f.ForOrg(context.Background(), "u-own", "org-1", "org-stub")
	if err != nil {
		t.Fatalf("ForOrg member: %v", err)
	}
	if key, memo := resolvedKey(t, p); key != "member-key" || memo != "u-own" {
		t.Errorf("member with own key: key=%q memo=%q, want member-key under the member scope", key, memo)
	}

	p, err = f.ForOrg(context.Background(), "u-none", "org-1", "org-stub")
	if err != nil {
		t.Fatalf("ForOrg org fallback: %v", err)
	}
	if key, memo := resolvedKey(t, p); key != "org-key" || memo != OrgScope("org-1") {
		t.Errorf("member without key: key=%q memo=%q, want org-key under %q", key, memo, OrgScope("org-1"))
	}
	if ap := p.(*auditedProvider); ap.scope != "u-none" {
		t.Errorf("usage scope = %q, want the member's (usage is attributed to the member, not the org scope)", ap.scope)
	}

	_, err = f.ForOrg(context.Background(), "u-none", "org-2", "org-stub")
	if !errors.Is(err, ErrKeyNotConfigured) {
		t.Errorf("no member key, no org key: err = %v, want ErrKeyNotConfigured", err)
	}
	_, err = f.ForOrg(context.Background(), "u-none", "", "org-stub")
	if !errors.Is(err, ErrKeyNotConfigured) {
		t.Errorf("personal flow without a key: err = %v, want ErrKeyNotConfigured", err)
	}

	noStore := orgTestFactory(map[string]string{}, nil)
	if _, err := noStore.ForOrg(context.Background(), "u-none", "org-1", "org-stub"); !errors.Is(err, ErrKeyNotConfigured) {
		t.Errorf("no org store: err = %v, want ErrKeyNotConfigured", err)
	}
}

// A member key-store failure is reported, never papered over by the org's
// credential; an org key-store failure is reported too.
func TestProviderFactory_ForOrg_LookupErrorsSurface(t *testing.T) {
	registerOrgStub()
	memberErr := errors.New("keystore: open: message authentication failed")
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", memberErr }, nil, nil, nil)
	f.SetOrgKeyLookup(func(_, _ string) (string, error) { return "org-key", nil })
	if _, err := f.ForOrg(context.Background(), "u1", "org-1", "org-stub"); !errors.Is(err, memberErr) {
		t.Errorf("member lookup error must surface, got %v", err)
	}

	orgErr := errors.New("db down")
	f = orgTestFactory(map[string]string{}, nil)
	f.SetOrgKeyLookup(func(_, _ string) (string, error) { return "", orgErr })
	if _, err := f.ForOrg(context.Background(), "u1", "org-1", "org-stub"); !errors.Is(err, orgErr) || !strings.Contains(err.Error(), "org key") {
		t.Errorf("org lookup error must surface and say so, got %v", err)
	}
}

// Copilot is always the member's own; the org never has one.
func TestProviderFactory_ForOrg_CopilotIgnoresOrg(t *testing.T) {
	orgCalls := 0
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", nil }, nil, nil, nil)
	f.SetOrgKeyLookup(func(_, _ string) (string, error) { orgCalls++; return "org-pat", nil })
	if _, err := f.ForOrg(context.Background(), "u1", "org-1", "copilot"); err == nil {
		t.Error("copilot without a member credential must fail even with an org key")
	}
	if orgCalls != 0 {
		t.Errorf("org store consulted %d times for copilot, want 0", orgCalls)
	}
}

// ForEmbeddingOrg is org-first: the corpus is embedded with the org's
// resource whenever it has one, else the member's.
func TestProviderFactory_ForEmbeddingOrg_OrgFirst(t *testing.T) {
	registerOrgStub()
	f := orgTestFactory(map[string]string{"u-own": "member-key"}, map[string]string{"org-1": "org-key"})

	p, err := f.ForEmbeddingOrg(context.Background(), "u-own", "org-1", "org-stub", "")
	if err != nil {
		t.Fatalf("ForEmbeddingOrg: %v", err)
	}
	if key, memo := resolvedKey(t, p); key != "org-key" || memo != OrgScope("org-1") {
		t.Errorf("org credential present: key=%q memo=%q, want the org's", key, memo)
	}

	p, err = f.ForEmbeddingOrg(context.Background(), "u-own", "org-2", "org-stub", "")
	if err != nil {
		t.Fatalf("ForEmbeddingOrg fallback: %v", err)
	}
	if key, memo := resolvedKey(t, p); key != "member-key" || memo != "u-own" {
		t.Errorf("no org credential: key=%q memo=%q, want the member's", key, memo)
	}

	if _, err := f.ForEmbeddingOrg(context.Background(), "u-none", "org-2", "org-stub", ""); !errors.Is(err, ErrKeyNotConfigured) {
		t.Errorf("nothing configured: err = %v, want ErrKeyNotConfigured", err)
	}
}

// The org's Azure document memoises under OrgScope + resource, so served
// models learned by one member are seen by the others; a member's own
// document stays under their scope.
func TestProviderFactory_ForOrg_AzureMemoScope(t *testing.T) {
	doc := `{"endpoint":"https://shared-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"c"}`
	f := orgTestFactory(map[string]string{}, map[string]string{"org-1": doc})
	p, err := f.ForOrg(context.Background(), "u1", "org-1", azureFoundryProviderID)
	if err != nil {
		t.Fatalf("ForOrg azure: %v", err)
	}
	ap := p.(*auditedProvider)
	if !strings.HasPrefix(ap.memoScope, OrgScope("org-1")+"\x00") || !strings.Contains(ap.memoScope, "shared-res") {
		t.Errorf("memoScope = %q, want org scope + resource", ap.memoScope)
	}
	if ap.scope != "u1" {
		t.Errorf("usage scope = %q, want the member's", ap.scope)
	}
}
