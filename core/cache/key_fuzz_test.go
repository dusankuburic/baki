package cache

import "testing"

var fuzzSpec = func() *Spec {
	s, err := newSpec("x.fuzz", Keys(KindTenant, KindParam, KindContent), ReplicaLocal("test"))
	if err != nil {
		panic(err)
	}
	return s
}()

// fuzzParts builds a tuple of fuzzSpec's parts; tenant picks the tenant constructor.
func fuzzParts(tenant uint8, id, pname, pval, content string) []Part {
	var t Part
	switch tenant % 4 {
	case 0:
		t = Org(id)
	case 1:
		t = User(id)
	case 2:
		t = OrgUser(id, pval)
	default:
		t = Local()
	}
	return []Part{t, Param(pname, pval), Content(content)}
}

func anyZero(parts []Part) bool {
	for _, p := range parts {
		if p.IsZero() {
			return true
		}
	}
	return false
}

// FuzzKey_Injective: two part tuples give the same key exactly when they are the same tuple, and every key
// decodes back into its parts (which makes the encoding injective by construction).
func FuzzKey_Injective(f *testing.F) {
	f.Add(uint8(0), "o", "p", "v", "c", uint8(1), "o", "p", "v", "c")
	f.Add(uint8(0), "a:b", "c", "", "d", uint8(0), "a", "b:c", "", "d")
	f.Add(uint8(2), "1", "x", "2x", "c", uint8(2), "12", "x", "x", "c")
	f.Add(uint8(1), "u", "n1:", "v", "c", uint8(1), "u", "n", "1:v", "c")
	f.Add(uint8(3), "", "p", "", "c", uint8(3), "", "p", "", "c")
	f.Fuzz(func(t *testing.T, t1 uint8, id1, n1, v1, c1 string, t2 uint8, id2, n2, v2, c2 string) {
		a, b := fuzzParts(t1, id1, n1, v1, c1), fuzzParts(t2, id2, n2, v2, c2)
		if anyZero(a) || anyZero(b) {
			return
		}
		ka, err := fuzzSpec.Key(a...)
		if err != nil {
			t.Fatal(err)
		}
		kb, err := fuzzSpec.Key(b...)
		if err != nil {
			t.Fatal(err)
		}
		same := a[0] == b[0] && a[1] == b[1] && a[2] == b[2]
		if (ka == kb) != same {
			t.Fatalf("tuples %v and %v: keys %q and %q (same tuple: %v)", a, b, ka, kb, same)
		}
		for _, k := range []struct {
			key   Key
			parts []Part
		}{{ka, a}, {kb, b}} {
			got, ok := decodeKey(k.key.String())
			if !ok || len(got) != len(k.parts) {
				t.Fatalf("decode %q = %v, %v", k.key, got, ok)
			}
			for i := range got {
				if got[i] != k.parts[i] {
					t.Fatalf("decode %q part %d = %v, want %v", k.key, i, got[i], k.parts[i])
				}
			}
		}
	})
}
