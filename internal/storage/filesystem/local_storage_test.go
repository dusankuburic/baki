package filesystem

import (
	"context"
	"testing"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
	testutil "pad-analyzer/internal/testutil"
)

// TestLocalStorageBackend_Contract runs the cross-backend contract suite
// against the filesystem backend. The same suite runs against Postgres in
// `database/postgres_storage_test.go::TestPostgres_Contract` so the two
// implementations cannot diverge on return-shape semantics.
func TestLocalStorageBackend_Contract(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	contract.RunSuite(t, b)
}

// createTestFlow creates a test flow document
func createTestFlow(id string) *interfaces.FlowDocument {
	return &interfaces.FlowDocument{
		ID:          id,
		Name:        "Test Flow",
		Description: "A test flow document",
		Content:     []byte(`{"test": "content"}`),
		Metadata: interfaces.FlowMetadata{
			BlockCount:   10,
			SubflowCount: 2,
			MaxDepth:     3,
		},
	}
}

// TestLocalStorageBackend_SaveAndLoadFlow tests saving and loading flow documents
func TestLocalStorageBackend_SaveAndLoadFlow(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Create test flow
	testFlow := createTestFlow("test-flow-1")

	// Save flow
	err := storage.SaveFlow(ctx, testFlow)
	testutil.AssertNoError(t, err, "Failed to save flow")

	// Load flow
	loadedFlow, err := storage.LoadFlow(ctx, "test-flow-1")
	testutil.AssertNoError(t, err, "Failed to load flow")

	// Verify flow data
	testutil.AssertEqual(t, testFlow.ID, loadedFlow.ID, "Flow ID mismatch")
	testutil.AssertEqual(t, testFlow.Name, loadedFlow.Name, "Flow name mismatch")
	testutil.AssertEqual(t, testFlow.Description, loadedFlow.Description, "Flow description mismatch")
}

// TestLocalStorageBackend_ListFlows tests listing flow documents
func TestLocalStorageBackend_ListFlows(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Create multiple test flows
	flows := []*interfaces.FlowDocument{
		createTestFlow("flow-1"),
		createTestFlow("flow-2"),
		createTestFlow("flow-3"),
	}

	// Save flows
	for _, flow := range flows {
		err := storage.SaveFlow(ctx, flow)
		testutil.AssertNoError(t, err, "Failed to save flow")
	}

	// List all flows
	listedFlows, err := storage.ListFlows(ctx, interfaces.FlowFilter{})
	testutil.AssertNoError(t, err, "Failed to list flows")
	testutil.AssertEqual(t, len(flows), len(listedFlows), "Flow count mismatch")

	// List with limit
	limitedFlows, err := storage.ListFlows(ctx, interfaces.FlowFilter{Limit: 2})
	testutil.AssertNoError(t, err, "Failed to list flows with limit")
	testutil.AssertEqual(t, 2, len(limitedFlows), "Limited flow count mismatch")
}

// TestLocalStorageBackend_DeleteFlow tests deleting flow documents
func TestLocalStorageBackend_DeleteFlow(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Create and save test flow
	testFlow := createTestFlow("flow-to-delete")
	err := storage.SaveFlow(ctx, testFlow)
	testutil.AssertNoError(t, err, "Failed to save flow")

	// Delete flow
	err = storage.DeleteFlow(ctx, "flow-to-delete")
	testutil.AssertNoError(t, err, "Failed to delete flow")

	// Verify flow is deleted
	_, err = storage.LoadFlow(ctx, "flow-to-delete")
	if err == nil {
		t.Fatal("Expected error when loading deleted flow")
	}
}

// TestLocalStorageBackend_SaveAndLoadSettings tests saving and loading settings
func TestLocalStorageBackend_SaveAndLoadSettings(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Create test settings
	testSettings := &interfaces.AppSettings{
		Version: 1,
		General: interfaces.GeneralSettings{
			FirstRunCompleted: true,
			LastUsedVersion:   "1.0.0",
			CheckForUpdates:    "weekly",
		},
		Appearance: interfaces.AppearanceSettings{
			Theme:   "dark",
			Density: "comfortable",
		},
	}

	// Save settings
	err := storage.SaveSettings(ctx, testSettings)
	testutil.AssertNoError(t, err, "Failed to save settings")

	// Load settings
	loadedSettings, err := storage.LoadSettings(ctx)
	testutil.AssertNoError(t, err, "Failed to load settings")

	// Verify settings
	testutil.AssertEqual(t, testSettings.Version, loadedSettings.Version, "Settings version mismatch")
	testutil.AssertEqual(t, testSettings.General.FirstRunCompleted, loadedSettings.General.FirstRunCompleted, "First run completed mismatch")
}

// TestLocalStorageBackend_SaveAndLoadConversation tests saving and loading conversations
func TestLocalStorageBackend_SaveAndLoadConversation(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Create test conversation
	messages := []interfaces.ChatMessage{
		{
			ID:        "msg-1",
			Role:      "user",
			Content:   "Hello",
			Timestamp: "2024-01-01T00:00:00Z",
		},
		{
			ID:        "msg-2",
			Role:      "assistant",
			Content:   "Hi there!",
			Timestamp: "2024-01-01T00:00:01Z",
		},
	}

	// Save conversation
	err := storage.SaveConversation(ctx, "flow-1", "test-scope", messages)
	testutil.AssertNoError(t, err, "Failed to save conversation")

	// Load conversation
	loadedMessages, err := storage.LoadConversation(ctx, "flow-1", "test-scope")
	testutil.AssertNoError(t, err, "Failed to load conversation")

	// Verify messages
	testutil.AssertEqual(t, len(messages), len(loadedMessages), "Message count mismatch")
	testutil.AssertEqual(t, messages[0].ID, loadedMessages[0].ID, "First message ID mismatch")
}

// TestLocalStorageBackend_Ping tests the ping method
func TestLocalStorageBackend_Ping(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Ping should succeed
	err := storage.Ping(ctx)
	testutil.AssertNoError(t, err, "Ping failed")
}