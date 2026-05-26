package ai

import (
	"testing"
)

func TestIsPotentialSecret(t *testing.T) {
	tests := []struct {
		value    string
		expected bool
	}{
		{"", false},
		{"short", false},
		{"password=MySecret123", true},
		{"api_key=sk-1234567890abcdef", true},
		{"normal text value", false},
		{"Bearer eyJhbGciOiJIUzI1NiJ9.test.sig", true},
		{"sk-proj-abcdefghijklmnopqrstuvwxyz", true},
		{"just a regular property value", false},
		{"my-password-is-hidden", true},
	}

	for _, tt := range tests {
		got := isPotentialSecret(tt.value)
		if got != tt.expected {
			t.Errorf("isPotentialSecret(%q) = %v, want %v", tt.value, got, tt.expected)
		}
	}
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		value    string
		expected string
	}{
		{"abcd", "****"},
		{"sk-1234567890abcdef", "sk***************ef"},
		{"ab", "****"},
		{"abcdefghij", "ab******ij"},
	}
	for _, tt := range tests {
		got := maskSecret(tt.value)
		if got != tt.expected {
			t.Errorf("maskSecret(%q) = %q, want %q", tt.value, got, tt.expected)
		}
	}
}

func TestShannonEntropy(t *testing.T) {
	if shannonEntropy("") != 0 {
		t.Error("empty string should have 0 entropy")
	}
	if shannonEntropy("aaaa") >= 1.0 {
		t.Error("repeated chars should have low entropy")
	}
	if shannonEntropy("aAbBcCdD") < 2.0 {
		t.Error("diverse chars should have higher entropy")
	}
}
