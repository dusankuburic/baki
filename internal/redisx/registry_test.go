package redisx

import (
	"strings"
	"testing"
	"time"
)

func TestNewKey_AcceptsAWellFormedSpec(t *testing.T) {
	k, err := newKey(Spec{Name: "ws.room", Kind: KindPubSub, Pattern: "ws:room:{flow}", OnDown: OnDownOpen, Owner: "internal/websocket"})
	if err != nil {
		t.Fatal(err)
	}
	if got := k.Key("flow-1"); got != "ws:room:flow-1" {
		t.Errorf("Key = %q", got)
	}
	if got := k.Glob(); got != "ws:room:*" {
		t.Errorf("Glob = %q", got)
	}
	if got := k.Prefix(); got != "ws:room:" {
		t.Errorf("Prefix = %q", got)
	}
}

func TestNewKey_Refusals(t *testing.T) {
	ok := Spec{Name: "demo.hash", Kind: KindHash, Pattern: "demo:hash:{id}", TTL: time.Minute, OnDown: OnDownOpen, Owner: "internal/demo"}
	for name, tc := range map[string]struct {
		mutate func(*Spec)
		want   string
	}{
		"name without a domain":       {func(s *Spec) { s.Name = "demo" }, "dot-separated"},
		"name in capitals":            {func(s *Spec) { s.Name = "Demo.Hash" }, "dot-separated"},
		"no kind":                     {func(s *Spec) { s.Kind = 0 }, "kind"},
		"no on-down behaviour":        {func(s *Spec) { s.OnDown = 0 }, "on-down"},
		"no owner":                    {func(s *Spec) { s.Owner = "" }, "owner"},
		"a key without a TTL":         {func(s *Spec) { s.TTL = 0 }, "TTL"},
		"a channel with a TTL":        {func(s *Spec) { s.Kind = KindPubSub }, "no TTL"},
		"pattern starts with a part":  {func(s *Spec) { s.Pattern = "{id}:demo" }, "literal prefix"},
		"unbalanced brace":            {func(s *Spec) { s.Pattern = "demo:{id" }, "unbalanced"},
		"a glob character in pattern": {func(s *Spec) { s.Pattern = "demo:*:{id}" }, "glob"},
		"a space in pattern":          {func(s *Spec) { s.Pattern = "demo: {id}" }, "space"},
		"negative version":            {func(s *Spec) { s.Version = -1 }, "version"},
		"a channel name too short":    {func(s *Spec) { s.Kind, s.TTL, s.Pattern = KindPubSub, 0, "ab:{id}" }, "at least 8 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			s := ok
			tc.mutate(&s)
			if _, err := newKey(s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("newKey = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	if _, err := newKey(ok); err != nil {
		t.Fatalf("the baseline spec is refused: %v", err)
	}
}

// forget removes a test declaration so the registry the golden reads stays the production one.
func forget(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		registry.Lock()
		delete(registry.keys, name)
		registry.Unlock()
	})
}

func TestDeclare_PanicsOnADuplicate(t *testing.T) {
	spec := Spec{Name: "test.duplicate", Kind: KindString, Pattern: "test:duplicate:{id}", TTL: time.Minute, OnDown: OnDownOpen, Owner: "internal/redisx"}
	Declare(spec)
	forget(t, spec.Name)
	for name, dup := range map[string]Spec{
		"same name":    spec,
		"same pattern": {Name: "test.other", Kind: KindString, Pattern: spec.Pattern, TTL: time.Minute, OnDown: OnDownOpen, Owner: "internal/redisx"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Declare did not panic", name)
				}
			}()
			Declare(dup)
		}()
	}
}

func TestKey_PartCountMustMatch(t *testing.T) {
	k, err := newKey(Spec{Name: "demo.two", Kind: KindHash, Pattern: "demo:{a}:{b}", TTL: time.Minute, OnDown: OnDownOpen, Owner: "internal/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if got := k.Key("x", "y"); got != "demo:x:y" {
		t.Errorf("Key = %q", got)
	}
	if got := k.Glob(); got != "demo:*:*" {
		t.Errorf("Glob = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("Key with one part for two placeholders did not panic")
		}
	}()
	k.Key("x")
}

// A constant channel (no placeholders) builds to its own name.
func TestKey_ConstantPattern(t *testing.T) {
	if got := RevocationChannel.Key(); got != "baki:auth:revoked" {
		t.Errorf("RevocationChannel = %q", got)
	}
}

func TestMatchAndMechanismOf(t *testing.T) {
	for key, want := range map[string]string{
		"baki:auth:revoked": "auth_revoked",
		"unclaimed:thing":   "other",
		"":                  "other",
	} {
		if got := MechanismOf(key); got != want {
			t.Errorf("MechanismOf(%q) = %q, want %q", key, got, want)
		}
	}
	// The longest prefix wins when two families share a start.
	long := Declare(Spec{Name: "test.long", Kind: KindString, Pattern: "baki:auth:revoked:detail:{id}", TTL: time.Minute, OnDown: OnDownOpen, Owner: "internal/redisx"})
	forget(t, long.Name())
	if got := MechanismOf("baki:auth:revoked:detail:9"); got != "test_long" {
		t.Errorf("longest prefix: MechanismOf = %q, want test_long", got)
	}
	if got := MechanismOf("baki:auth:revoked"); got != "auth_revoked" {
		t.Errorf("shorter key: MechanismOf = %q, want auth_revoked", got)
	}
}

func TestDescribe(t *testing.T) {
	got := Describe(Spec{Name: "ws.presence", Kind: KindHash, Pattern: "ws:presence:{flow}", TTL: 216 * time.Second, Version: 1, OnDown: OnDownOpen, Owner: "internal/websocket"})
	want := "ws.presence | hash | ws:presence:{flow} | ttl: 3m36s | v1 | on-down: open | internal/websocket"
	if got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
	got = Describe(Spec{Name: "auth.revoked", Kind: KindPubSub, Pattern: "baki:auth:revoked", OnDown: OnDownClosed, Owner: "internal/auth"})
	if !strings.Contains(got, "ttl: none") || !strings.Contains(got, "on-down: closed") {
		t.Errorf("Describe of a channel = %q", got)
	}
}

// Specs lists the declarations sorted by name, so the golden is stable.
func TestSpecs_SortedByName(t *testing.T) {
	specs := Specs()
	for i := 1; i < len(specs); i++ {
		if specs[i-1].Name >= specs[i].Name {
			t.Errorf("Specs not sorted: %q before %q", specs[i-1].Name, specs[i].Name)
		}
	}
	if _, ok := Lookup("auth.revoked"); !ok {
		t.Error("Lookup(auth.revoked) found nothing")
	}
}

func TestNewKey_KindsAreForChannelsAndWellFormed(t *testing.T) {
	channel := Spec{Name: "demo.kinds", Kind: KindPubSub, Pattern: "demo:kinds:chan", Kinds: []string{"inval", "recheck"}, OnDown: OnDownOpen, Owner: "internal/demo"}
	if _, err := newKey(channel); err != nil {
		t.Fatalf("a channel with kinds refused: %v", err)
	}
	for name, mutate := range map[string]func(*Spec){
		"kinds on a hash":    func(s *Spec) { s.Kind, s.TTL = KindHash, time.Minute },
		"an empty kind":      func(s *Spec) { s.Kinds = []string{""} },
		"a kind in capitals": func(s *Spec) { s.Kinds = []string{"Inval"} },
		"a repeated kind":    func(s *Spec) { s.Kinds = []string{"inval", "inval"} },
	} {
		s := channel
		mutate(&s)
		if _, err := newKey(s); err == nil || !strings.Contains(err.Error(), "kind") {
			t.Errorf("%s: err = %v, want a refusal naming the kinds", name, err)
		}
	}
	got := Describe(channel)
	if !strings.Contains(got, "kinds: inval, recheck") {
		t.Errorf("Describe = %q, want the kinds listed", got)
	}
}
