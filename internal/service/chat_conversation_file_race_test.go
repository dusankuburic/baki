package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"pad-core/models"
)

// TestConversationFile_SaveRacesReads: local and desktop store a conversation as a file replaced by rename. On
// Windows the rename fails while a reader has the file open, and a read fails while the rename replaces it, so a
// polling GET conversation lost the stream's finished answer or got a 500. Saves and reads now retry the transient
// conflict; neither may fail while reads poll.
func TestConversationFile_SaveRacesReads(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir(),
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{SaveConversationHistory: true}}}}
	ctx := context.Background()
	const flow, conv = "f1", "11111111-2222-4333-8444-555555555555"
	if err := svc.SaveConversation(ctx, flow, conv, []models.ChatMessage{{ID: "m0", Role: "user", Content: "q"}}); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var readErrs sync.Map
	var wg sync.WaitGroup
	for r := range 2 { // each polls about every millisecond (the client polls every 10 ms)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := svc.GetConversation(ctx, flow, conv); err != nil {
					readErrs.Store(r, err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		msgs := []models.ChatMessage{{ID: "m0", Role: "user", Content: "q"},
			{ID: fmt.Sprintf("a%d", i), Role: "assistant", Content: fmt.Sprintf("answer %d", i)}}
		if err := svc.SaveConversation(ctx, flow, conv, msgs); err != nil {
			t.Errorf("save %d racing reads: %v", i, err)
			break
		}
	}
	close(stop)
	wg.Wait()
	readErrs.Range(func(k, v any) bool {
		t.Errorf("reader %v: %v", k, v)
		return true
	})
}
