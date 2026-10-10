package cache

import "testing"

// TestParts_EmptyInputIsZero: every constructor turns empty input into the zero Part, which Key refuses, so an
// unset tenant or digest can never widen a key to "everyone".
func TestParts_EmptyInputIsZero(t *testing.T) {
	zero := map[string]Part{
		"Org":               Org(""),
		"User":              User(""),
		"OrgUser no org":    OrgUser("", "u"),
		"OrgUser no user":   OrgUser("o", ""),
		"Tenant both empty": Tenant("", ""),
		"RuleProfile":       RuleProfile(""),
		"Content":           Content(""),
		"Resource":          Resource(""),
		"Version":           Version(""),
		"Param no name":     Param("", "v"),
	}
	for name, p := range zero {
		if !p.IsZero() {
			t.Errorf("%s = %v, want the zero Part", name, p)
		}
	}
	set := map[string]Part{
		"Org":             Org("o"),
		"User":            User("u"),
		"OrgUser":         OrgUser("o", "u"),
		"Local":           Local(),
		"Param no value":  Param("flag", ""),
		"Param":           Param("model", "m"),
		"Version":         Version("3"),
		"Tenant org only": Tenant("o", ""),
	}
	for name, p := range set {
		if p.IsZero() {
			t.Errorf("%s is zero", name)
		}
	}
}

// TestParts_TenantKinds: Tenant prefers the organisation; the tenant kinds stay distinct for equal ids.
func TestParts_TenantKinds(t *testing.T) {
	if got := Tenant("o", "u"); got != Org("o") {
		t.Errorf("Tenant(o, u) = %v, want Org(o)", got)
	}
	if got := Tenant("", "u"); got != User("u") {
		t.Errorf("Tenant(\"\", u) = %v, want User(u)", got)
	}
	distinct := []Part{Org("x"), User("x"), Local(), OrgUser("x", "x")}
	for i := range distinct {
		if distinct[i].Kind() != KindTenant {
			t.Errorf("%v: kind %v, want tenant", distinct[i], distinct[i].Kind())
		}
		for j := i + 1; j < len(distinct); j++ {
			if distinct[i] == distinct[j] {
				t.Errorf("%v == %v", distinct[i], distinct[j])
			}
		}
	}
	// The pair is encoded unambiguously: ("a:b", "c") and ("a", "b:c") differ.
	if OrgUser("a:b", "c") == OrgUser("a", "b:c") || OrgUser("1", "2x") == OrgUser("12", "x") {
		t.Error("OrgUser pairs collide")
	}
}

func TestPartKind_String(t *testing.T) {
	want := map[PartKind]string{
		KindTenant: "tenant", KindRuleProfile: "rule-profile", KindContent: "content",
		KindResource: "resource", KindVersion: "version", KindParam: "param", 0: "invalid", maxKind + 1: "invalid",
	}
	for k, s := range want {
		if k.String() != s {
			t.Errorf("PartKind(%d) = %q, want %q", k, k.String(), s)
		}
	}
}
