package config

import (
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/platform/secret"
)

const (
	strongEncKey  = "a-sufficiently-long-random-enc-key-123456"
	strongPrevKey = "the-replaced-encryption-key-0123456789abcdef"
)

// TestApplyEnvVars_EncryptionKeyPrevious: PAD_ENCRYPTION_KEY_PREVIOUS is a comma-separated list, newest first, with
// blanks around and between the entries dropped.
func TestApplyEnvVars_EncryptionKeyPrevious(t *testing.T) {
	t.Setenv("PAD_ENCRYPTION_KEY_PREVIOUS", " "+strongPrevKey+" , ,older-key-0123456789abcdefghijklmnop ")
	cfg := Default()
	if err := applyEnvVars(cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{strongPrevKey, "older-key-0123456789abcdefghijklmnop"}
	if got := secret.RevealAll(cfg.Auth.EncryptionKeyPrevious); !slices.Equal(got, want) {
		t.Errorf("EncryptionKeyPrevious = %q, want %q", got, want)
	}
}

// TestValidate_EncryptionKeyPrevious: the previous keys meet the current key's cloud strength rule, need a dedicated
// current key, and repeat neither it nor each other; no error message carries a key.
func TestValidate_EncryptionKeyPrevious(t *testing.T) {
	cfg := cloudCfg()
	cfg.Auth.EncryptionKey = secret.New(strongEncKey)
	cfg.Auth.EncryptionKeyPrevious = []secret.Value{secret.New(strongPrevKey)}
	if err := Validate(cfg); err != nil {
		t.Fatalf("a strong previous key was refused: %v", err)
	}

	const weak = "short-old-key"
	for name, tc := range map[string]struct {
		mode     DeploymentMode
		current  string
		previous []string
		wantIn   string
	}{
		"without a dedicated key":   {ModeCloud, "", []string{strongPrevKey}, "PAD_ENCRYPTION_KEY"},
		"local without a key":       {ModeLocal, "", []string{strongPrevKey}, "PAD_ENCRYPTION_KEY"},
		"repeats the current key":   {ModeCloud, strongEncKey, []string{strongEncKey}, "entry 1"},
		"repeats an earlier entry":  {ModeCloud, strongEncKey, []string{strongPrevKey, strongPrevKey}, "entry 2"},
		"local repeats current key": {ModeLocal, "local-key", []string{"local-key"}, "entry 1"},
	} {
		cfg := cloudCfg()
		if tc.mode == ModeLocal {
			cfg = Default()
		}
		cfg.Auth.EncryptionKey = secret.New(tc.current)
		cfg.Auth.EncryptionKeyPrevious = make([]secret.Value, len(tc.previous))
		for i, k := range tc.previous {
			cfg.Auth.EncryptionKeyPrevious[i] = secret.New(k)
		}
		err := Validate(cfg)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "PAD_ENCRYPTION_KEY_PREVIOUS") || !strings.Contains(msg, tc.wantIn) {
			t.Errorf("%s: error %q should name PAD_ENCRYPTION_KEY_PREVIOUS and %q", name, msg, tc.wantIn)
		}
		for _, k := range append([]string{tc.current}, tc.previous...) {
			if k != "" && strings.Contains(msg, k) {
				t.Errorf("%s: error %q carries a key", name, msg)
			}
		}
	}

	// The strength rule is the current key's only: a weak or placeholder previous key is how a deployment moves off
	// it (TestValidate_AWeakKeyCanBeRotatedAway), in every mode.
	for _, prev := range [][]string{{strongPrevKey, weak}, {"please-change-this-secret-in-production"}} {
		cfg := cloudCfg()
		cfg.Auth.EncryptionKey = secret.New(strongEncKey)
		cfg.Auth.EncryptionKeyPrevious = []secret.Value{}
		for _, k := range prev {
			cfg.Auth.EncryptionKeyPrevious = append(cfg.Auth.EncryptionKeyPrevious, secret.New(k))
		}
		if err := Validate(cfg); err != nil {
			t.Errorf("cloud mode refused a weak previous key %d: %v", len(prev), err)
		}
	}
	local := Default()
	local.Auth.EncryptionKey = secret.New("local-key")
	local.Auth.EncryptionKeyPrevious = []secret.Value{secret.New(weak)}
	if err := Validate(local); err != nil {
		t.Errorf("local mode refused a short previous key: %v", err)
	}
}
