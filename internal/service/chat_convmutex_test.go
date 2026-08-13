package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// panicOnSaveStore embeds FakeBackend (so it satisfies the full chatStore
// interface) but panics on SaveConversation to simulate a storage failure inside
// the reconstruct+persist critical section.
type panicOnSaveStore struct {
	*testutil.FakeBackend
}

func (p *panicOnSaveStore) SaveConversation(context.Context, string, string, []storageif.ChatMessage) error {
	panic("boom: simulated storage failure during persist")
}

// TestReconstructAndPersistUserTurn_ReleasesMutexOnPanic is the regression test
// for the per-conversation mutex leak. A panic inside the locked critical
// section (here from a storage failure during persist) must still release the
// conversation mutex via defer-unlock. Before the fix the section used a manual
// Lock/Unlock with no defer, so a panic — swallowed by the stream goroutine's
// top-level recover() — left the persistent convMu-map mutex held forever,
// deadlocking every future stream on that (flowID, convKey).
func TestReconstructAndPersistUserTurn_ReleasesMutexOnPanic(t *testing.T) {
	svc := &ChatService{
		mode:    config.ModeCloud,
		backend: &panicOnSaveStore{FakeBackend: testutil.NewFakeBackend()},
	}
	doc := &models.FlowDocument{ID: "flow-1"}
	req := models.ChatRequest{UserMessage: "hi"}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected the injected persist panic to propagate")
			}
		}()
		svc.reconstructAndPersistUserTurn(context.Background(), doc, &req, "flow")
	}()

	// The mutex must be re-acquirable: TryLock succeeds only if the panic path
	// released it. A leaked lock fails here (and, in production, would hang every
	// subsequent stream on this conversation).
	mu := svc.convMutexFor(doc.ID, "flow")
	if !mu.TryLock() {
		t.Fatal("conversation mutex was not released after panic — leak regression")
	}
	mu.Unlock()
}
