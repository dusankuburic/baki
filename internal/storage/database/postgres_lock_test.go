package database_test

import (
	"context"
	"testing"
)

// TestPostgres_TryGlobalLock exercises the advisory-lock capability against a
// real Postgres: mutual exclusion between two acquisitions of the same key,
// independence of distinct keys, and re-acquirability after release. Skipped
// unless DATABASE_URL is set (same harness as the other integration tests).
func TestPostgres_TryGlobalLock(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	const key = int64(0x746573746c6f636b) // "testlock"

	rel1, ok, err := b.TryGlobalLock(ctx, key)
	if err != nil || !ok {
		t.Fatalf("first acquisition: acquired=%v err=%v, want true/nil", ok, err)
	}

	// Same key while held → must be refused (this is the cross-replica guard;
	// advisory locks are session-scoped and each call pins its own pooled
	// connection, so the two calls genuinely contend like two replicas would).
	rel2, ok, err := b.TryGlobalLock(ctx, key)
	if err != nil {
		t.Fatalf("second acquisition errored: %v", err)
	}
	if ok {
		rel2()
		t.Fatal("second acquisition of a held key succeeded — no mutual exclusion")
	}

	// A different key must be independent.
	relOther, ok, err := b.TryGlobalLock(ctx, key+1)
	if err != nil || !ok {
		t.Fatalf("distinct-key acquisition: acquired=%v err=%v, want true/nil", ok, err)
	}
	relOther()

	// After release the key must be immediately re-acquirable (release unlocks
	// explicitly rather than waiting for the pooled session to die).
	rel1()
	rel3, ok, err := b.TryGlobalLock(ctx, key)
	if err != nil || !ok {
		t.Fatalf("re-acquisition after release: acquired=%v err=%v, want true/nil", ok, err)
	}
	rel3()
}
