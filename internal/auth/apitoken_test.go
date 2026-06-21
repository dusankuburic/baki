package auth

import (
	"strings"
	"testing"
)

func TestGenerateAPIToken(t *testing.T) {
	raw1, hash1, err := GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	if !strings.HasPrefix(raw1, APITokenPrefix) {
		t.Errorf("raw token %q missing prefix %q", raw1, APITokenPrefix)
	}
	if !IsAPIToken(raw1) {
		t.Error("IsAPIToken should recognize a generated token")
	}
	if hash1 != HashAPIToken(raw1) {
		t.Error("returned hash must equal HashAPIToken(raw)")
	}
	if hash1 == raw1 {
		t.Error("hash must not equal the raw token")
	}

	// Tokens must be unique across calls.
	raw2, _, _ := GenerateAPIToken()
	if raw1 == raw2 {
		t.Error("two generated tokens should differ")
	}
}

func TestHashAPIToken_Deterministic(t *testing.T) {
	h1, h2 := HashAPIToken("pad_pat_abc"), HashAPIToken("pad_pat_abc")
	if h1 != h2 {
		t.Error("HashAPIToken must be deterministic")
	}
	if h1 == HashAPIToken("pad_pat_xyz") {
		t.Error("different tokens must hash differently")
	}
}

func TestIsAPIToken(t *testing.T) {
	if IsAPIToken("eyJhbGciOi...") { // a JWT
		t.Error("a JWT must not be treated as an API token")
	}
	if IsAPIToken("") {
		t.Error("empty string is not an API token")
	}
	if !IsAPIToken(APITokenPrefix + "deadbeef") {
		t.Error("prefixed value should be an API token")
	}
}
