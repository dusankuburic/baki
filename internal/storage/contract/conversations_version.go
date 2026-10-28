package contract

import (
	"context"
	"errors"
	"sync"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runConversationVersionScenarios holds the conversation store's optimistic-version contract (ADR 0031): the
// compare-and-swap save and delete, and the rule that every change to the messages gives a new version, whoever
// made it.
func runConversationVersionScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	msg := func(id, content string) interfaces.ChatMessage {
		return interfaces.ChatMessage{ID: id, Role: "user", Content: content, Timestamp: "2026-10-13T10:00:00Z"}
	}
	ids := func(msgs []interfaces.ChatMessage) []string {
		out := make([]string, len(msgs))
		for i, m := range msgs {
			out[i] = m.ID
		}
		return out
	}
	mustLoad := func(t *testing.T, flowID string) ([]interfaces.ChatMessage, int64) {
		t.Helper()
		msgs, v, err := b.LoadConversationVersioned(ctx, flowID, "scope", "")
		if err != nil {
			t.Fatalf("LoadConversationVersioned: %v", err)
		}
		return msgs, v
	}
	mustSaveIf := func(t *testing.T, flowID string, expected int64, msgs ...interfaces.ChatMessage) int64 {
		t.Helper()
		v, err := b.SaveConversationIf(ctx, flowID, "scope", "", expected, msgs)
		if err != nil {
			t.Fatalf("SaveConversationIf(expected %d): %v", expected, err)
		}
		return v
	}
	wantConflict := func(t *testing.T, what string, err error) {
		t.Helper()
		if !errors.Is(err, interfaces.ErrConversationConflict) {
			t.Fatalf("%s: err = %v, want ErrConversationConflict", what, err)
		}
	}

	run("LoadConversationVersioned_missing_returns_empty_and_zero", func(t *testing.T) {
		msgs, v := mustLoad(t, "contract-cv-missing-"+runID)
		if msgs == nil || len(msgs) != 0 || v != 0 {
			t.Errorf("missing conversation = (%v, %d), want a non-nil empty list at version 0", msgs, v)
		}
	})

	run("SaveConversationIf_create_expects_zero", func(t *testing.T) {
		flowID := "contract-cv-create-" + runID
		// Nothing to compare against: a version the caller could only have read from a conversation that is gone.
		_, err := b.SaveConversationIf(ctx, flowID, "scope", "", 7, []interfaces.ChatMessage{msg("m0", "x")})
		wantConflict(t, "create expecting 7", err)
		if got, v := mustLoad(t, flowID); len(got) != 0 || v != 0 {
			t.Fatalf("a refused create stored something: (%v, %d)", ids(got), v)
		}

		v1 := mustSaveIf(t, flowID, 0, msg("m1", "first"))
		if v1 <= 0 {
			t.Errorf("created conversation has version %d, want a positive one", v1)
		}
		got, v := mustLoad(t, flowID)
		if v != v1 || len(got) != 1 || got[0].ID != "m1" {
			t.Errorf("after create: (%v, %d), want (m1, %d)", ids(got), v, v1)
		}
		// A second create from the same empty read lost the race.
		_, err = b.SaveConversationIf(ctx, flowID, "scope", "", 0, []interfaces.ChatMessage{msg("m2", "second")})
		wantConflict(t, "second create with expected 0", err)
		if got, v := mustLoad(t, flowID); v != v1 || len(got) != 1 || got[0].ID != "m1" {
			t.Errorf("a refused create changed the conversation: (%v, %d)", ids(got), v)
		}
	})

	run("SaveConversationIf_stale_expected_conflicts_and_writes_nothing", func(t *testing.T) {
		flowID := "contract-cv-stale-" + runID
		v1 := mustSaveIf(t, flowID, 0, msg("m1", "one"))
		v2 := mustSaveIf(t, flowID, v1, msg("m1", "one"), msg("m2", "two"))
		if v2 <= v1 {
			t.Fatalf("version after a change = %d, want more than %d", v2, v1)
		}
		current, err := b.SaveConversationIf(ctx, flowID, "scope", "", v1, []interfaces.ChatMessage{msg("m1", "one"), msg("m3", "stale")})
		wantConflict(t, "save at the old version", err)
		if current != v2 {
			t.Errorf("the conflict reported version %d, want the current %d", current, v2)
		}
		got, v := mustLoad(t, flowID)
		if v != v2 || len(got) != 2 || got[1].ID != "m2" {
			t.Errorf("a refused save changed the conversation: (%v, %d), want (m1 m2, %d)", ids(got), v, v2)
		}
		// Unchanged messages keep the version: a retry that stores what is already there is not a change.
		if same := mustSaveIf(t, flowID, v2, msg("m1", "one"), msg("m2", "two")); same != v2 {
			t.Errorf("saving the stored list again moved the version %d to %d", v2, same)
		}
	})

	run("SaveConversationIf_concurrent_same_base_exactly_one_wins", func(t *testing.T) {
		flowID := "contract-cv-race-" + runID
		base := mustSaveIf(t, flowID, 0, msg("m0", "base"))
		const writers = 8
		var (
			wg        sync.WaitGroup
			mu        sync.Mutex
			winners   []string
			conflicts int
			others    []error
		)
		start := make(chan struct{})
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := "w" + string(rune('a'+i))
				<-start
				_, err := b.SaveConversationIf(ctx, flowID, "scope", "", base, []interfaces.ChatMessage{msg("m0", "base"), msg(id, "from "+id)})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					winners = append(winners, id)
				case errors.Is(err, interfaces.ErrConversationConflict):
					conflicts++
				default:
					others = append(others, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(others) > 0 {
			t.Fatalf("unexpected errors: %v", others)
		}
		if len(winners) != 1 || conflicts != writers-1 {
			t.Fatalf("winners = %v, conflicts = %d; want exactly one winner and %d conflicts", winners, conflicts, writers-1)
		}
		got, _ := mustLoad(t, flowID)
		if len(got) != 2 || got[1].ID != winners[0] {
			t.Errorf("stored %v, want the winner's list (m0 %s)", ids(got), winners[0])
		}
	})

	run("Version_strictly_increases_and_survives_no_ABA_after_delete_recreate", func(t *testing.T) {
		flowID := "contract-cv-aba-" + runID
		v1 := mustSaveIf(t, flowID, 0, msg("m1", "old life"))
		v2 := mustSaveIf(t, flowID, v1, msg("m1", "old life"), msg("m2", "more"))
		if v2 <= v1 {
			t.Fatalf("versions %d then %d, want them to increase", v1, v2)
		}
		// Cleared and started again: the new conversation may not take a version the old one had, or a holder of
		// the old read could write over it as if nothing happened.
		if err := b.DeleteConversation(ctx, flowID, "scope"); err != nil {
			t.Fatalf("DeleteConversation: %v", err)
		}
		v3 := mustSaveIf(t, flowID, 0, msg("n1", "new life"))
		if v3 <= v2 {
			t.Errorf("version of the re-created conversation = %d, want more than %d", v3, v2)
		}
		for _, stale := range []int64{v1, v2} {
			_, err := b.SaveConversationIf(ctx, flowID, "scope", "", stale, []interfaces.ChatMessage{msg("m9", "ghost")})
			wantConflict(t, "write at a version of the deleted conversation", err)
		}
		if got, v := mustLoad(t, flowID); v != v3 || len(got) != 1 || got[0].ID != "n1" {
			t.Errorf("stored (%v, %d), want (n1, %d)", ids(got), v, v3)
		}
	})

	run("Blind_SaveConversation_invalidates_a_held_version", func(t *testing.T) {
		flowID := "contract-cv-blind-" + runID
		held := mustSaveIf(t, flowID, 0, msg("m1", "one"))
		if err := b.SaveConversation(ctx, flowID, "scope", []interfaces.ChatMessage{msg("m1", "one"), msg("m2", "blind")}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		got, v := mustLoad(t, flowID)
		if v <= held {
			t.Errorf("a blind change left the version at %d (was %d), want a greater one", v, held)
		}
		if len(got) != 2 {
			t.Fatalf("blind save stored %v", ids(got))
		}
		_, err := b.SaveConversationIf(ctx, flowID, "scope", "", held, []interfaces.ChatMessage{msg("m1", "one"), msg("m3", "late")})
		wantConflict(t, "save at the version read before the blind save", err)
	})

	run("Blind_SaveConversation_of_the_same_list_keeps_the_version", func(t *testing.T) {
		flowID := "contract-cv-blindsame-" + runID
		held := mustSaveIf(t, flowID, 0, msg("m1", "one"), msg("m2", "two"))
		if err := b.SaveConversation(ctx, flowID, "scope", []interfaces.ChatMessage{msg("m1", "one"), msg("m2", "two")}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		if _, v := mustLoad(t, flowID); v != held {
			t.Errorf("an unchanged blind save moved the version %d to %d", held, v)
		}
		mustSaveIf(t, flowID, held, msg("m1", "one"), msg("m2", "two"), msg("m3", "three"))
	})

	run("Blind_SaveConversation_creates_with_a_version", func(t *testing.T) {
		flowID := "contract-cv-blindnew-" + runID
		if err := b.SaveConversation(ctx, flowID, "scope", []interfaces.ChatMessage{msg("m1", "one")}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		got, v := mustLoad(t, flowID)
		if len(got) != 1 || v <= 0 {
			t.Fatalf("(%v, %d), want one message at a positive version", ids(got), v)
		}
		mustSaveIf(t, flowID, v, msg("m1", "one"), msg("m2", "two"))
	})

	run("DeleteConversationIf_stale_conflicts_current_deletes", func(t *testing.T) {
		flowID := "contract-cv-del-" + runID
		v1 := mustSaveIf(t, flowID, 0, msg("m1", "one"))
		v2 := mustSaveIf(t, flowID, v1, msg("m1", "one"), msg("m2", "two"))
		wantConflict(t, "delete at the old version", b.DeleteConversationIf(ctx, flowID, "scope", "", v1, false))
		if got, v := mustLoad(t, flowID); v != v2 || len(got) != 2 {
			t.Fatalf("a refused delete changed the conversation: (%v, %d)", ids(got), v)
		}
		if err := b.DeleteConversationIf(ctx, flowID, "scope", "", v2, false); err != nil {
			t.Fatalf("DeleteConversationIf at the current version: %v", err)
		}
		if got, v := mustLoad(t, flowID); len(got) != 0 || v != 0 {
			t.Errorf("after delete: (%v, %d), want an empty conversation at version 0", ids(got), v)
		}
		// Gone already: clearing from nothing is the idempotent no-op; clearing a version that was held is a
		// conflict, because the conversation changed since the caller read it.
		if err := b.DeleteConversationIf(ctx, flowID, "scope", "", 0, false); err != nil {
			t.Errorf("DeleteConversationIf(0) of a missing conversation = %v, want nil", err)
		}
		wantConflict(t, "delete of a missing conversation at a held version", b.DeleteConversationIf(ctx, flowID, "scope", "", v2, false))
	})

	run("Conflict_matches_ErrVersionConflict", func(t *testing.T) {
		flowID := "contract-cv-is-" + runID
		v := mustSaveIf(t, flowID, 0, msg("m1", "one"))
		_, err := b.SaveConversationIf(ctx, flowID, "scope", "", v+1000, nil)
		if !errors.Is(err, interfaces.ErrConversationConflict) || !errors.Is(err, interfaces.ErrVersionConflict) {
			t.Errorf("err = %v, want it to match ErrConversationConflict and ErrVersionConflict", err)
		}
	})
}
