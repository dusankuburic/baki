package ai

import (
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/egress"
)

func TestLocalConfig_TrailingV1RootIsAccepted(t *testing.T) {
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	for in, want := range map[string]string{
		"http://127.0.0.1:11434/v1":  "http://127.0.0.1:11434",
		"http://127.0.0.1:11434/V1/": "http://127.0.0.1:11434",
		"http://127.0.0.1:11434/":    "http://127.0.0.1:11434",
		"http://[::1]:1234":          "http://[::1]:1234",
	} {
		cfg, err := NewLocalConfigForSave(in, 0)
		if err != nil || cfg.BaseURL != want {
			t.Errorf("%s: cfg = %+v, err = %v, want %s", in, cfg, err, want)
		}
	}
	if _, err := NewLocalConfigForSave("http://127.0.0.1:11434/v1/extra", 0); err == nil {
		t.Error("a deeper path must stay refused")
	}
}

func TestNewLocalConfigForSave_ErrorCodes(t *testing.T) {
	saveLocalModelsPolicy(t, egress.BlockInternal)
	_, err := NewLocalConfigForSave("http://127.0.0.1:11434", 0)
	var ce *LocalConfigError
	if !errors.As(err, &ce) || ce.Code != LocalCfgErrUnavailable || LocalModelsAvailable() {
		t.Errorf("cloud policy: err = %v, want LOCAL_MODELS_UNAVAILABLE", err)
	}
	// The read route's refusal is the same error.
	if err := CheckLocalModelsAvailable(); !errors.As(err, &ce) || ce.Code != LocalCfgErrUnavailable || !strings.Contains(ce.Message, "desktop app only") {
		t.Errorf("cloud policy: CheckLocalModelsAvailable = %v, want LOCAL_MODELS_UNAVAILABLE", err)
	}
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	if !LocalModelsAvailable() || CheckLocalModelsAvailable() != nil {
		t.Error("AllowLoopback must make local models available")
	}
	_, err = NewLocalConfigForSave("https://example.com", 0)
	if !errors.As(err, &ce) || ce.Code != LocalCfgErrInvalidBaseURL {
		t.Errorf("remote host: err = %v, want INVALID_BASE_URL", err)
	}
}

// The optional context window: 0 is the default (not stored), 1024..2000000
// is stored as given, anything else is INVALID_CONTEXT_WINDOW.
func TestNewLocalConfigForSave_ContextWindow(t *testing.T) {
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	for _, n := range []int{0, 1024, 8192, 32768, 2000000} {
		cfg, err := NewLocalConfigForSave("http://127.0.0.1:11434", n)
		if err != nil || cfg.ContextWindow != n {
			t.Errorf("%d: cfg = %+v, err = %v", n, cfg, err)
			continue
		}
		raw, err := cfg.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 && strings.Contains(raw, "contextWindow") {
			t.Errorf("an unset window must not be stored: %s", raw)
		}
		back, err := ParseLocalConfig(raw)
		if err != nil || back.ContextWindow != n || back.Public().ContextWindow != n {
			t.Errorf("%d: round trip = %+v (%v)", n, back, err)
		}
		want := n
		if n == 0 {
			want = LocalDefaultContextWindow
		}
		if back.EffectiveContextWindow() != want {
			t.Errorf("%d: EffectiveContextWindow = %d, want %d", n, back.EffectiveContextWindow(), want)
		}
	}
	for _, n := range []int{-1, 1, 1023, 2000001} {
		_, err := NewLocalConfigForSave("http://127.0.0.1:11434", n)
		var ce *LocalConfigError
		if !errors.As(err, &ce) || ce.Code != LocalCfgErrInvalidContextWindow {
			t.Errorf("%d: err = %v, want INVALID_CONTEXT_WINDOW", n, err)
		}
	}
	// A bad URL is still named first.
	_, err := NewLocalConfigForSave("https://example.com", 5)
	var ce *LocalConfigError
	if !errors.As(err, &ce) || ce.Code != LocalCfgErrInvalidBaseURL {
		t.Errorf("bad URL + bad window: err = %v, want INVALID_BASE_URL", err)
	}
}

// A stored window out of range reads as unset (the default applies) instead
// of making the provider unusable; the model keys of the document's first
// draft stay accepted and are dropped on the next save.
func TestParseLocalConfig_TolerantOfStoredOddities(t *testing.T) {
	cfg, err := ParseLocalConfig(`{"baseUrl":"http://127.0.0.1:11434","contextWindow":100}`)
	if err != nil || cfg.ContextWindow != 0 || cfg.EffectiveContextWindow() != LocalDefaultContextWindow {
		t.Errorf("out-of-range window: cfg = %+v, err = %v", cfg, err)
	}
	cfg, err = ParseLocalConfig(`{"baseUrl":"http://127.0.0.1:11434","model":"qwen2.5:7b","embeddingModel":"nomic-embed-text"}`)
	if err != nil || cfg.BaseURL != "http://127.0.0.1:11434" {
		t.Fatalf("legacy model keys: cfg = %+v, err = %v", cfg, err)
	}
	raw, err := cfg.Encode()
	if err != nil || strings.Contains(raw, "model") || strings.Contains(raw, "embedding") {
		t.Errorf("re-encoded document = %s (%v), want the legacy keys gone", raw, err)
	}
	// Anything else unknown is still refused.
	if _, err := ParseLocalConfig(`{"baseUrl":"http://127.0.0.1:11434","apiKey":"x"}`); err == nil {
		t.Error("an unknown field must stay refused")
	}
}
