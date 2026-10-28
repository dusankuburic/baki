package config

import (
	"testing"
	"time"
)

// PAD_RAG_DEADLINE_MS: how long a chat turn waits for its knowledge-base guidelines before it answers without them
// (R-observability-5). Unset is the 800 ms default; a number of milliseconds is taken as is; a negative one, or one
// past the ceiling (the turn's first chunk waits on it), is refused at boot.
func TestRuntime_RAGDeadlineConfig(t *testing.T) {
	if got := Default().Runtime.EffectiveRAGDeadline(); got != 800*time.Millisecond {
		t.Errorf("default deadline = %v, want 800ms", got)
	}
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_RAG_DEADLINE_MS", "1500")
	cfg, err := LoadFromEnvRaw()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Runtime.EffectiveRAGDeadline(); got != 1500*time.Millisecond {
		t.Errorf("PAD_RAG_DEADLINE_MS=1500 gives %v", got)
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("1500 must validate: %v", err)
	}

	t.Setenv("PAD_RAG_DEADLINE_MS", "0")
	if cfg, err = LoadFromEnvRaw(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Runtime.EffectiveRAGDeadline(); got != DefaultRAGDeadline {
		t.Errorf("PAD_RAG_DEADLINE_MS=0 gives %v, want the default %v", got, DefaultRAGDeadline)
	}

	for _, bad := range []string{"-1", "10001"} {
		t.Setenv("PAD_RAG_DEADLINE_MS", bad)
		if cfg, err = LoadFromEnvRaw(); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := Validate(cfg); err == nil {
			t.Errorf("PAD_RAG_DEADLINE_MS=%s validated; it must be refused at boot", bad)
		}
	}
	t.Setenv("PAD_RAG_DEADLINE_MS", "soon")
	if _, err := LoadFromEnvRaw(); err == nil {
		t.Error("PAD_RAG_DEADLINE_MS=soon loaded; a value that is not a number must be refused")
	}
}
