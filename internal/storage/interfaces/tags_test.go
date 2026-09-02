package interfaces

import (
	"strings"
	"testing"
)

// TestNormalizeFlowTags pins the shared wire contract: lowercase, whitelist,
// length/count caps, dedupe, empty-drop. Every backend enforces exactly this.
func TestNormalizeFlowTags(t *testing.T) {
	got, err := NormalizeFlowTags([]string{"  PROD ", "prod", "", "Business-Unit", "env_test"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{"prod", "business-unit", "env_test"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}

	bad := []struct {
		name string
		in   []string
	}{
		{"space in tag", []string{"business unit"}},
		{"illegal char", []string{"prod!"}},
		{"too long", []string{strings.Repeat("a", MaxFlowTagLen+1)}},
		{"too many", makeMany(MaxFlowTags + 1)},
	}
	for _, tc := range bad {
		if _, err := NormalizeFlowTags(tc.in); err == nil {
			t.Errorf("%s: want error for %v", tc.name, tc.in)
		}
	}
}

func makeMany(n int) []string {
	out := make([]string, n)
	for i := range out {
		// Unique per index: two letters + zero-padded index.
		out[i] = "t" + strings.Repeat("0", 3) + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	return out
}

// TestSplitFlowTags: CSV round trip; empty ⇒ nil.
func TestSplitFlowTags(t *testing.T) {
	if got := SplitFlowTags(""); got != nil {
		t.Errorf("empty CSV = %v, want nil", got)
	}
	if got := SplitFlowTags("prod,finance"); len(got) != 2 || got[0] != "prod" || got[1] != "finance" {
		t.Errorf("split = %v", got)
	}
}
