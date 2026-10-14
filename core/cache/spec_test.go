package cache

import (
	"errors"
	"strings"
	"testing"
)

// Declared once per test binary: the registry is process-wide.
var (
	registeredTenant = Declare("cachetest.registered-tenant",
		Keys(KindTenant, KindContent), ReplicaLocal("test"))
	registeredGlobal = Declare("cachetest.registered-global",
		Keys(KindResource), Global("test: ids are unique"), ReplicaLocal("test"))
)

// testSpec validates a declaration without registering it, so a test gets fresh statistics on every run.
func testSpec(t *testing.T, name string, opts ...SpecOption) *Spec {
	t.Helper()
	s, err := newSpec(name, opts...)
	if err != nil {
		t.Fatalf("newSpec(%q): %v", name, err)
	}
	return s
}

// mustPanic runs fn and returns what it panicked with, failing when it did not.
func mustPanic(t *testing.T, what string, fn func()) (v any) {
	t.Helper()
	defer func() {
		v = recover()
		if v == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	fn()
	return nil
}

// TestDeclare_RejectsBadDeclarations: a cache must name its tenant or say why it has none, live somewhere, and
// carry a well-formed name; each mistake is a declaration error (an init panic through Declare).
func TestDeclare_RejectsBadDeclarations(t *testing.T) {
	local := ReplicaLocal("recomputed on a miss")
	cases := []struct {
		name, spec string
		opts       []SpecOption
		want       string
	}{
		{"no tenant, no Global", "x.y", []SpecOption{Keys(KindContent), local}, "no KindTenant"},
		{"no keys, no Global", "x.y", []SpecOption{local}, "no KindTenant"},
		{"tenant and Global", "x.y", []SpecOption{Keys(KindTenant), Global("why"), local}, "Global at once"},
		{"Global without reason", "x.y", []SpecOption{Keys(KindContent), Global(" "), local}, "needs a reason"},
		{"Global twice", "x.y", []SpecOption{Global("a"), Global("b"), local}, "Global given twice"},
		{"tenant twice", "x.y", []SpecOption{Keys(KindTenant, KindTenant), local}, "tenant part is declared 2 times"},
		{"invalid kind", "x.y", []SpecOption{Keys(KindTenant, PartKind(0)), local}, "invalid part kind"},
		{"invalid kind high", "x.y", []SpecOption{Keys(KindTenant, maxKind+1), local}, "invalid part kind"},
		{"Keys twice", "x.y", []SpecOption{Keys(KindTenant), Keys(KindTenant), local}, "Keys given twice"},
		{"too many parts", "x.y", []SpecOption{Keys(append([]PartKind{KindTenant}, sixteenParams()...)...), local}, "at most 16"},
		{"no locality", "x.y", []SpecOption{Keys(KindTenant)}, "no locality"},
		{"two localities", "x.y", []SpecOption{Keys(KindTenant), local, Shared("redis", "shared by a server")}, "more than one locality"},
		{"locality without reason", "x.y", []SpecOption{Keys(KindTenant), ReplicaLocal("")}, "needs a reason"},
		{"name without domain", "reports", []SpecOption{Keys(KindTenant), local}, "dot-separated"},
		{"name with capitals", "Analyzer.reports", []SpecOption{Keys(KindTenant), local}, "dot-separated"},
		{"name with spaces", "analyzer. reports", []SpecOption{Keys(KindTenant), local}, "dot-separated"},
		{"empty name", "", []SpecOption{Keys(KindTenant), local}, "dot-separated"},
		{"long name", "a." + strings.Repeat("b", 63), []SpecOption{Keys(KindTenant), local}, "at most 64"},
	}
	for _, tc := range cases {
		_, err := newSpec(tc.spec, tc.opts...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
	ok := [][]SpecOption{
		{Keys(KindTenant, KindRuleProfile, KindContent), local},
		{Keys(KindTenant, KindParam, KindParam, KindContent, KindContent), local},
		{Global("one deployment row"), local}, // a singleton: no parts at all
		{Keys(KindResource), Global("ids are unique"), Shared("redis", "shared by a server")},
	}
	for i, opts := range ok {
		if _, err := newSpec("x.ok-spec", opts...); err != nil {
			t.Errorf("valid declaration %d refused: %v", i, err)
		}
	}
	mustPanic(t, "Declare with an invalid declaration", func() { Declare("x.y", Keys(KindContent), local) })
}

func sixteenParams() []PartKind {
	out := make([]PartKind, 16)
	for i := range out {
		out[i] = KindParam
	}
	return out
}

// TestDeclare_DuplicateNamePanics: one name, one meaning.
func TestDeclare_DuplicateNamePanics(t *testing.T) {
	v := mustPanic(t, "a second Declare of a name", func() {
		Declare(registeredTenant.Name(), Keys(KindTenant), ReplicaLocal("again"))
	})
	if err, ok := v.(error); !ok || !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("panic = %v, want a duplicate-declaration error", v)
	}
}

// TestSpecs_ListsDeclarationsSorted: the registry (the source of the caches golden and of the metrics) lists every
// declaration by name with what it declared.
func TestSpecs_ListsDeclarationsSorted(t *testing.T) {
	specs := Specs()
	byName := map[string]SpecInfo{}
	for i, s := range specs {
		if i > 0 && specs[i-1].Name >= s.Name {
			t.Errorf("Specs not sorted: %q before %q", specs[i-1].Name, s.Name)
		}
		byName[s.Name] = s
	}
	tenant, ok := byName["cachetest.registered-tenant"]
	if !ok {
		t.Fatal("declared spec missing from Specs()")
	}
	if len(tenant.Kinds) != 2 || tenant.Kinds[0] != KindTenant || tenant.Kinds[1] != KindContent ||
		tenant.Global != "" || tenant.Locality != LocalityReplica || tenant.Where != "test" {
		t.Errorf("tenant spec info = %+v", tenant)
	}
	global := byName[registeredGlobal.Name()]
	if global.Global != "test: ids are unique" || len(global.Kinds) != 1 || global.Kinds[0] != KindResource {
		t.Errorf("global spec info = %+v", global)
	}
	if len(global.Stats.Evictions) != len(EvictReasons()) {
		t.Errorf("Evictions has %d reasons, want every one of %d", len(global.Stats.Evictions), len(EvictReasons()))
	}
	// SpecInfo is a copy: changing it changes no declaration.
	tenant.Kinds[0] = KindParam
	if registeredTenant.Info().Kinds[0] != KindTenant {
		t.Error("SpecInfo.Kinds aliases the spec")
	}
}

// TestKey_ExactlyTheDeclaredParts: any argument order gives the same key; a missing, extra or zero part is refused
// (a panic in a test binary, an error in production) and counted as a rejected key.
func TestKey_ExactlyTheDeclaredParts(t *testing.T) {
	s := testSpec(t, "x.key", Keys(KindTenant, KindRuleProfile, KindContent), ReplicaLocal("test"))
	a, err := s.Key(Org("o"), RuleProfile("p"), Content("c"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Key(Content("c"), Org("o"), RuleProfile("p"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a.IsZero() || a.Spec() != s {
		t.Errorf("argument order changed the key: %q vs %q", a, b)
	}
	if got := a.Parts(); len(got) != 3 || got[0] != Org("o") || got[1] != RuleProfile("p") || got[2] != Content("c") {
		t.Errorf("Parts() = %v, want the parts in declared order", got)
	}

	bad := map[string][]Part{
		"missing tenant": {RuleProfile("p"), Content("c")},
		"missing all":    nil,
		"extra part":     {Org("o"), RuleProfile("p"), Content("c"), Version("1")},
		"duplicate kind": {Org("o"), User("u"), RuleProfile("p"), Content("c")},
		"zero tenant":    {Org(""), RuleProfile("p"), Content("c")},
		"zero content":   {Org("o"), RuleProfile("p"), Content("")},
		"wrong kind":     {Org("o"), RuleProfile("p"), Resource("c")},
		"17 parts":       seventeenParts(),
	}
	for name, parts := range bad {
		mustPanic(t, "Key with "+name, func() { _, _ = s.Key(parts...) })
	}
	restore := SetKeyErrorPanicForTesting(false)
	defer restore()
	for name, parts := range bad {
		k, err := s.Key(parts...)
		var ke *KeyError
		if err == nil || !errors.Is(err, ErrInvalidKey) || !errors.As(err, &ke) || ke.Spec != "x.key" || !k.IsZero() {
			t.Errorf("%s: Key = %q, %v; want the zero Key and a *KeyError", name, k, err)
		}
	}
	if got := s.Info().Stats.Rejected; got != uint64(2*len(bad)) {
		t.Errorf("Rejected = %d, want %d (every refusal counted, panicking or not)", got, 2*len(bad))
	}
}

func seventeenParts() []Part {
	out := []Part{Org("o")}
	for i := range maxParts {
		out = append(out, Param("p", string(rune('a'+i))))
	}
	return out
}

// TestKey_RepeatedKindsKeepArgumentOrder: two parts of one kind fill the declared slots in argument order, and the
// Param names keep them apart.
func TestKey_RepeatedKindsKeepArgumentOrder(t *testing.T) {
	s := testSpec(t, "x.params", Keys(KindTenant, KindParam, KindParam), ReplicaLocal("test"))
	k1, _ := s.Key(User("u"), Param("provider", "p"), Param("model", "m"))
	k2, _ := s.Key(Param("provider", "p"), User("u"), Param("model", "m"))
	k3, _ := s.Key(User("u"), Param("model", "m"), Param("provider", "p"))
	if k1 != k2 {
		t.Errorf("moving the tenant changed the key: %q vs %q", k1, k2)
	}
	if k1 == k3 {
		t.Error("swapping two params kept the key; they fill the slots in argument order")
	}
	if p := k1.Parts(); p[1] != Param("provider", "p") || p[2] != Param("model", "m") {
		t.Errorf("Parts() = %v", p)
	}
	// An empty Param value is an input like any other.
	e1, _ := s.Key(User("u"), Param("a", ""), Param("b", "x"))
	e2, _ := s.Key(User("u"), Param("a", "x"), Param("b", ""))
	if e1 == e2 {
		t.Error("empty param values collided")
	}
}

// TestKey_SingletonGlobal: a Global spec without parts has exactly one key.
func TestKey_SingletonGlobal(t *testing.T) {
	s := testSpec(t, "x.singleton", Global("one row"), ReplicaLocal("test"))
	k, err := s.Key()
	if err != nil || k.IsZero() || k.String() != "" {
		t.Errorf("Key() = %q, %v; want the one valid empty key", k, err)
	}
	mustPanic(t, "Key with a part on a partless spec", func() { _, _ = s.Key(Version("1")) })
}

// TestKey_EncodingIsReadable pins the encoding's shape (the keys only live in memory; this is for log readers).
func TestKey_EncodingIsReadable(t *testing.T) {
	s := testSpec(t, "x.enc", Keys(KindTenant, KindVersion, KindParam), ReplicaLocal("test"))
	k, _ := s.Key(Org("acme"), Version("3"), Param("q", "a:b"))
	if want := "t3:org4:acmev0:1:3a1:q3:a:b"; k.String() != want {
		t.Errorf("encoding = %q, want %q", k.String(), want)
	}
	if _, ok := decodeKey("t3:org4:acm"); ok {
		t.Error("decodeKey accepted a truncated encoding")
	}
	for _, bad := range []string{"z0:1:x", "t03:org4:acme", "t-1:", "tx:", "t3"} {
		if _, ok := decodeKey(bad); ok {
			t.Errorf("decodeKey accepted %q", bad)
		}
	}
}

// TestShared_NeedsAMechanismAndAReason: Shared names how the entries are shared and why that is enough, and the
// golden prints both.
func TestShared_NeedsAMechanismAndAReason(t *testing.T) {
	for _, tc := range []struct{ mechanism, reason, want string }{
		{"", "r", "needs a mechanism"},
		{" ", "r", "needs a mechanism"},
		{"redis-inval", "", "needs a reason"},
		{"redis-inval", " ", "needs a reason"},
	} {
		_, err := newSpec("x.shared", Keys(KindTenant), Shared(tc.mechanism, tc.reason))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Shared(%q, %q): err = %v, want it to mention %q", tc.mechanism, tc.reason, err, tc.want)
		}
	}
	info := testSpec(t, "x.shared", Keys(KindTenant), Shared(MechanismInvalidation, "30 s TTL backstop")).Info()
	if info.Locality != LocalityShared || info.Mechanism != "redis-inval" || info.Where != "redis-inval: 30 s TTL backstop" {
		t.Errorf("info = %+v, want shared / redis-inval / %q", info, "redis-inval: 30 s TTL backstop")
	}
	if got := testSpec(t, "x.local", Keys(KindTenant), ReplicaLocal("why")).Info(); got.Mechanism != "" || got.Where != "why" {
		t.Errorf("replica-local info = %+v, want no mechanism and the reason as Where", got)
	}
}

func TestLocality_String(t *testing.T) {
	if LocalityReplica.String() != "replica-local" || LocalityShared.String() != "shared" || Locality(0).String() != "invalid" {
		t.Error("Locality names changed (they are in the caches golden)")
	}
}

func TestKeyError_Messages(t *testing.T) {
	for _, tc := range []struct {
		err  KeyError
		want string
	}{
		{KeyError{Spec: "a.b", Problem: "zero"}, `cache "a.b": a key part is zero (built from empty input)`},
		{KeyError{Spec: "a.b", Problem: "foreign"}, `cache "a.b": the key was built by another cache's spec`},
		{KeyError{Spec: "a.b", Problem: "extra"}, `cache "a.b": extra key parts`},
		{KeyError{Spec: "a.b", Kind: KindTenant, Problem: "missing"}, `cache "a.b": missing tenant part`},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}
