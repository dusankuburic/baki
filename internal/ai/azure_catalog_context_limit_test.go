package ai

import "testing"

// TestCatalog_AzureContextLimitIsTheInputCap (D-37): ContextLimit is the
// INPUT cap the chat service truncates the prompt against; the gpt-5 family
// carried the total 400k window (272k in + 128k out), so a 300k-token
// history passed the guard and every turn failed with CONTEXT_LIMIT.
func TestCatalog_AzureContextLimitIsTheInputCap(t *testing.T) {
	for _, id := range []string{"gpt-5", "gpt-5-mini", "gpt-5-nano"} {
		entry, ok := catalogEntryFor(azureFoundryProviderID, id)
		if !ok {
			t.Fatalf("%s missing from the azure-foundry catalogue", id)
		}
		if entry.ContextLimit != 272000 || entry.ContextLimit+entry.MaxOutputTokens != 400000 {
			t.Errorf("%s: ContextLimit %d + MaxOutputTokens %d, want the 272000 input cap of the 400000 window", id, entry.ContextLimit, entry.MaxOutputTokens)
		}
	}
	// A served-model lookup (versioned name) resolves to the same entry.
	if entry, ok := catalogEntryFor(azureFoundryProviderID, "gpt-5-2025-08-07"); !ok || entry.ContextLimit != 272000 {
		t.Errorf("versioned gpt-5 = %+v %v", entry, ok)
	}
}
