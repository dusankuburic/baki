package filesystem

import "testing"

// The read side of path safety (LoadFlow and LoadConversation refusing traversal-bearing IDs and scopes) is the
// contract's Path_traversal_is_refused scenario, run by TestLegacySource_Contract.
func TestSafePathSegment(t *testing.T) {
	bad := []string{"", ".", "..", "../etc", "a/b", `a\b`, "..\\x", "foo/../bar", "a..b"}
	for _, s := range bad {
		if safePathSegment(s) {
			t.Errorf("safePathSegment(%q) = true, want false", s)
		}
	}
	good := []string{"flow-123", "uuid_abc-DEF", "abc123"}
	for _, s := range good {
		if !safePathSegment(s) {
			t.Errorf("safePathSegment(%q) = false, want true", s)
		}
	}
}
