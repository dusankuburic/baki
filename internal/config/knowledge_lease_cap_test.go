package config

import "testing"

// PAD_KNOWLEDGE_LEASE_CAP: the org knowledge-base leases one replica holds at once (each pins a pooled database
// connection). Unset is the default; a number is taken as is; a negative one is refused at boot.
func TestStorage_KnowledgeLeaseCapConfig(t *testing.T) {
	if got := Default().Storage.EffectiveKnowledgeLeaseCap(); got != 4 {
		t.Errorf("default cap = %d, want 4", got)
	}
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_KNOWLEDGE_LEASE_CAP", "7")
	cfg, err := LoadFromEnvRaw()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Storage.EffectiveKnowledgeLeaseCap(); got != 7 {
		t.Errorf("PAD_KNOWLEDGE_LEASE_CAP=7 gives cap %d", got)
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("7 must validate: %v", err)
	}

	t.Setenv("PAD_KNOWLEDGE_LEASE_CAP", "0")
	cfg, err = LoadFromEnvRaw()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Storage.EffectiveKnowledgeLeaseCap(); got != DefaultKnowledgeLeaseCap {
		t.Errorf("PAD_KNOWLEDGE_LEASE_CAP=0 gives cap %d, want the default %d", got, DefaultKnowledgeLeaseCap)
	}

	t.Setenv("PAD_KNOWLEDGE_LEASE_CAP", "-1")
	cfg, err = LoadFromEnvRaw()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Validate(cfg); err == nil {
		t.Error("a negative cap validated; it must be refused at boot")
	}
}
