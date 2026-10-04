package ai

import "testing"

// TestLearnedWindows_RepointedAliasForgetsItsWindow (D-43): a stable alias's
// first served-model change drops its learned window (the rejection that
// taught it belonged to the previous model); a date bump, a model router
// and an alias already known to vary keep theirs.
func TestLearnedWindows_RepointedAliasForgetsItsWindow(t *testing.T) {
	resetServedModels()
	resetLearnedWindows()
	t.Cleanup(resetServedModels)
	t.Cleanup(resetLearnedWindows)
	az := azureFoundryProviderID

	// Re-pointed: Phi-4 → gpt-4o.
	recordContextWindow("u", az, "chat", 16384)
	recordServedModel("u", az, "chat", "Phi-4")
	if lim, ok := lookupContextWindow("u", az, "chat"); !ok || lim != 16384 {
		t.Fatalf("first observation must keep the window: %d %v", lim, ok)
	}
	recordServedModel("u", az, "chat", "gpt-4o-2024-11-20")
	if lim, ok := lookupContextWindow("u", az, "chat"); ok {
		t.Errorf("re-pointed alias kept its old window %d", lim)
	}

	// A version-date bump of the same model is not a re-point.
	recordContextWindow("u", az, "dated", 16384)
	recordServedModel("u", az, "dated", "gpt-4o-2024-05-13")
	recordServedModel("u", az, "dated", "gpt-4o-2024-11-20")
	if lim, ok := lookupContextWindow("u", az, "dated"); !ok || lim != 16384 {
		t.Errorf("date bump dropped the window: %d %v", lim, ok)
	}

	// A model router (by name) keeps its learned minimum across routed models.
	recordContextWindow("u", az, "model-router", 16384)
	recordServedModel("u", az, "model-router", "gpt-5-nano-2025-08-07")
	recordServedModel("u", az, "model-router", "gpt-4.1-2025-04-14")
	if lim, ok := lookupContextWindow("u", az, "model-router"); !ok || lim != 16384 {
		t.Errorf("model-router lost its learned minimum: %d %v", lim, ok)
	}

	// An alias already observed varying keeps its window on a third model.
	recordServedModel("u", az, "smart", "gpt-5-nano-2025-08-07")
	recordServedModel("u", az, "smart", "gpt-4.1-2025-04-14") // varied from now on
	recordContextWindow("u", az, "smart", 8192)
	recordServedModel("u", az, "smart", "o4-mini-2025-04-16")
	if lim, ok := lookupContextWindow("u", az, "smart"); !ok || lim != 8192 {
		t.Errorf("a varied alias lost its window: %d %v", lim, ok)
	}

	// Another user's same-named deployment is untouched.
	recordContextWindow("v", az, "chat", 4096)
	recordServedModel("u", az, "chat", "gpt-4.1")
	if lim, ok := lookupContextWindow("v", az, "chat"); !ok || lim != 4096 {
		t.Errorf("another scope's window changed: %d %v", lim, ok)
	}
}
