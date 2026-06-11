package api

import (
	"strings"
	"testing"
)

// TestValidateEmail locks the registration email policy: net/mail structural
// validation plus the 254-char RFC cap. The pre-fix check was only
// strings.Contains(email, "@"), which accepted "@", "a@", " @ ".
func TestValidateEmail(t *testing.T) {
	reject := []string{
		"",
		" ",
		"@",
		"a@",
		"not-an-email",
		"plainaddress",
		strings.Repeat("a", 250) + "@example.com", // > 254 chars
	}
	for _, e := range reject {
		if err := validateEmail(e); err == nil {
			t.Errorf("validateEmail(%q): expected rejection, got nil", e)
		}
	}

	accept := []string{
		"alice@example.com",
		"user.name+tag@sub.example.co",
	}
	for _, e := range accept {
		if err := validateEmail(e); err != nil {
			t.Errorf("validateEmail(%q): expected accept, got %v", e, err)
		}
	}
}

// TestValidatePasswordStrength locks the password policy: 12–72 chars
// (72 because bcrypt ignores bytes past 72) and at least 3 character classes.
func TestValidatePasswordStrength(t *testing.T) {
	reject := map[string]string{
		"too short":             "Aa1!",                    // 4 chars
		"11 chars":              "Password12!"[:11],        // boundary below min
		"only lowercase":        "aaaaaaaaaaaa",            // 12, 1 class
		"two classes":           "abcABCabcABC",            // 12, lower+upper only
		"over 72 bytes":         strings.Repeat("Aa1", 25), // 75 chars, 3 classes but too long
	}
	for name, pw := range reject {
		if err := validatePasswordStrength(pw); err == nil {
			t.Errorf("%s (%q): expected rejection, got nil", name, pw)
		}
	}

	accept := map[string]string{
		"4 classes":            "Password123!",            // 12 chars
		"3 classes":            "abcDEF123xyz",            // lower+upper+digit
		"exactly 72 bytes":     strings.Repeat("Aa1", 24), // 72 chars, 3 classes
	}
	for name, pw := range accept {
		if err := validatePasswordStrength(pw); err != nil {
			t.Errorf("%s (%q): expected accept, got %v", name, pw, err)
		}
	}
}
