package contract

import (
	"context"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runCIDeliveryScenarios holds the CI webhook's replay record (ADR 0019): a key's delivery id is recorded once, and
// of concurrent records of one pair exactly one wins, so a captured call cannot run twice even when its copies
// race. The pair is the whole key: another caller's record of the same id is its own.
func runCIDeliveryScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("CIDeliveries_record_once_per_caller", func(t *testing.T) {
		callerA, callerB := "credential:contract-a-"+runID, "token:contract-b-"+runID
		id := "contract-delivery-" + runID
		record := func(caller, id string) bool {
			t.Helper()
			fresh, err := b.RecordCIDelivery(ctx, caller, id)
			if err != nil {
				t.Fatalf("RecordCIDelivery(%s, %s): %v", caller, id, err)
			}
			return fresh
		}
		if !record(callerA, id) {
			t.Fatal("a new pair is not fresh")
		}
		if record(callerA, id) {
			t.Error("the same pair recorded twice")
		}
		if !record(callerB, id) {
			t.Error("another caller's record of the id is a replay")
		}
		if !record(callerA, id+"-2") {
			t.Error("the caller's next id is a replay")
		}
		// The bounds the webhook allows are stored; an id outside them is refused and recorded nowhere.
		if !record(callerA, strings.Repeat("a", 16)) || !record(callerA, strings.Repeat("b", 128)) {
			t.Error("an id at the bounds was not recorded")
		}
		if fresh, err := b.RecordCIDelivery(ctx, callerA, strings.Repeat("c", 15)); err == nil || fresh {
			t.Errorf("a 15-character id = %v, %v; want an error", fresh, err)
		}

		const racers = 16
		raced := "contract-race-" + runID
		var wg sync.WaitGroup
		results := make(chan bool, racers)
		errs := make(chan error, racers)
		for range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fresh, err := b.RecordCIDelivery(ctx, callerA, raced)
				if err != nil {
					errs <- err
					return
				}
				results <- fresh
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Errorf("a concurrent record failed: %v", err)
		}
		n := 0
		for fresh := range results {
			if fresh {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d of %d concurrent records of one pair were fresh, want 1", n, racers)
		}
	})
}
