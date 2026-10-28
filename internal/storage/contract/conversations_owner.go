package contract

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/storage/interfaces"
)

// runConversationOwnerScenarios holds the conversation store's owner rule (R-security-3, migration 0052): a
// conversation belongs to the user whose write created it; another user reads it as missing, cannot write it and
// cannot delete it; an unowned (legacy) conversation is shared by the flow's audience, deleted only by a caller that
// may delete the flow, and listed for nobody. Also the thread index (R-frontend-c26 (c)): the list, the title, and
// what erasure and the export do with conversations on other people's flows.
func runConversationOwnerScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	msg := func(id, content string) interfaces.ChatMessage {
		return interfaces.ChatMessage{ID: id, Role: "user", Content: content, Timestamp: "2026-10-27T10:00:00Z"}
	}
	ownerA, ownerB := "contract-co-a-"+runID, "contract-co-b-"+runID
	load := func(t *testing.T, flowID, scope, owner string) ([]interfaces.ChatMessage, int64) {
		t.Helper()
		msgs, v, err := b.LoadConversationVersioned(ctx, flowID, scope, owner)
		if err != nil {
			t.Fatalf("LoadConversationVersioned(%s): %v", owner, err)
		}
		return msgs, v
	}
	create := func(t *testing.T, flowID, scope, owner string, msgs ...interfaces.ChatMessage) int64 {
		t.Helper()
		v, err := b.SaveConversationIf(ctx, flowID, scope, owner, 0, msgs)
		if err != nil {
			t.Fatalf("SaveConversationIf(create, %s): %v", owner, err)
		}
		t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), flowID, scope) })
		return v
	}
	list := func(t *testing.T, flowID, owner string) []interfaces.ConversationSummary {
		t.Helper()
		got, err := b.ListConversations(ctx, flowID, owner, nil, 100)
		if err != nil {
			t.Fatalf("ListConversations(%s): %v", owner, err)
		}
		return got
	}
	scopes := func(s []interfaces.ConversationSummary) []string {
		out := make([]string, len(s))
		for i, c := range s {
			out[i] = c.Scope
		}
		return out
	}

	run("ConversationOwner_another_users_conversation_reads_as_missing", func(t *testing.T) {
		flowID := "contract-co-read-" + runID
		// Every kind of key: a thread uuid, the legacy "flow" key and a block id.
		for _, scope := range []string{"0b7c4c1e-6a65-4cf4-9a49-6f7d3b1f2a10", "flow", "block-" + runID} {
			v := create(t, flowID, scope, ownerA, msg("a1", "A's secret question"))
			if got, gv := load(t, flowID, scope, ownerB); len(got) != 0 || gv != 0 {
				t.Errorf("%s: B read A's conversation: (%d messages, version %d), want (0, 0)", scope, len(got), gv)
			}
			if got, gv := load(t, flowID, scope, ownerA); len(got) != 1 || gv != v {
				t.Errorf("%s: A's own read = (%d messages, version %d), want (1, %d)", scope, len(got), gv, v)
			}
			// The system (no owner) sees every conversation.
			if got, _ := load(t, flowID, scope, ""); len(got) != 1 {
				t.Errorf("%s: the system read %d messages, want 1", scope, len(got))
			}
		}
	})

	run("ConversationOwner_another_users_key_refuses_writes", func(t *testing.T) {
		flowID := "contract-co-write-" + runID
		v := create(t, flowID, "k", ownerA, msg("a1", "mine"))
		for _, expected := range []int64{0, v} {
			_, err := b.SaveConversationIf(ctx, flowID, "k", ownerB, expected, []interfaces.ChatMessage{msg("b1", "B's turn")})
			if !errors.Is(err, interfaces.ErrConversationKeyTaken) {
				t.Errorf("B's save at %d over A's conversation: err = %v, want ErrConversationKeyTaken", expected, err)
			}
			if errors.Is(err, interfaces.ErrConversationConflict) {
				t.Errorf("a taken key must not read as a conflict (a client would reload and retry forever): %v", err)
			}
		}
		if _, err := b.SetConversationTitle(ctx, flowID, "k", ownerB, "B's name"); !errors.Is(err, interfaces.ErrConversationKeyTaken) {
			t.Errorf("B naming A's conversation: err = %v, want ErrConversationKeyTaken", err)
		}
		got, gv := load(t, flowID, "k", ownerA)
		if gv != v || len(got) != 1 || got[0].ID != "a1" {
			t.Errorf("A's conversation changed under B's refused writes: (%v, %d)", got, gv)
		}
	})

	run("ConversationOwner_another_users_conversation_cannot_be_deleted", func(t *testing.T) {
		flowID := "contract-co-del-" + runID
		v := create(t, flowID, "k", ownerA, msg("a1", "mine"))
		// Missing to B: expected 0 is the idempotent no-op, a held version the conflict of a gone conversation.
		if err := b.DeleteConversationIf(ctx, flowID, "k", ownerB, 0, true); err != nil {
			t.Errorf("B's delete at 0 = %v, want the no-op of a missing conversation", err)
		}
		if err := b.DeleteConversationIf(ctx, flowID, "k", ownerB, v, true); !errors.Is(err, interfaces.ErrConversationConflict) {
			t.Errorf("B's delete at A's version = %v, want ErrConversationConflict (missing to B)", err)
		}
		if _, gv := load(t, flowID, "k", ownerA); gv != v {
			t.Fatalf("B deleted A's conversation (version now %d)", gv)
		}
		if err := b.DeleteConversationIf(ctx, flowID, "k", ownerA, v, false); err != nil {
			t.Fatalf("A's delete of A's conversation: %v", err)
		}
		if got, gv := load(t, flowID, "k", ""); len(got) != 0 || gv != 0 {
			t.Errorf("after A's delete: (%d, %d), want gone", len(got), gv)
		}
	})

	run("ConversationOwner_unowned_conversation_is_shared_and_deleted_only_with_the_flow_right", func(t *testing.T) {
		flowID := "contract-co-legacy-" + runID
		// A blind system write: the shape every row stored before owners has.
		if err := b.SaveConversation(ctx, flowID, "flow", []interfaces.ChatMessage{msg("l1", "legacy")}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), flowID, "flow") })
		_, v := load(t, flowID, "flow", ownerA)
		if v == 0 {
			t.Fatal("the unowned conversation is not readable by A")
		}
		// B appends to it; it stays unowned, so A still reads B's turn.
		v2, err := b.SaveConversationIf(ctx, flowID, "flow", ownerB, v, []interfaces.ChatMessage{msg("l1", "legacy"), msg("b1", "B")})
		if err != nil {
			t.Fatalf("B's append to the unowned conversation: %v", err)
		}
		if got, gv := load(t, flowID, "flow", ownerA); gv != v2 || len(got) != 2 {
			t.Errorf("after B's append A reads (%d, %d), want (2, %d): the write must not take the conversation", len(got), gv, v2)
		}
		if l := list(t, flowID, ownerA); len(l) != 0 {
			t.Errorf("an unowned conversation is in A's list: %v", scopes(l))
		}
		if l := list(t, flowID, ownerB); len(l) != 0 {
			t.Errorf("an unowned conversation is in B's list: %v", scopes(l))
		}
		if _, err := b.SetConversationTitle(ctx, flowID, "flow", ownerA, "named"); !errors.Is(err, interfaces.ErrConversationUnowned) {
			t.Errorf("naming an unowned conversation: err = %v, want ErrConversationUnowned", err)
		}
		if err := b.DeleteConversationIf(ctx, flowID, "flow", ownerA, v2, false); !errors.Is(err, interfaces.ErrConversationUnowned) {
			t.Errorf("A's delete without the flow right: err = %v, want ErrConversationUnowned", err)
		}
		if _, gv := load(t, flowID, "flow", ownerA); gv != v2 {
			t.Fatalf("the refused delete removed the conversation (version %d)", gv)
		}
		if err := b.DeleteConversationIf(ctx, flowID, "flow", ownerA, v2, true); err != nil {
			t.Fatalf("A's delete with the flow right: %v", err)
		}
		if got, gv := load(t, flowID, "flow", ""); len(got) != 0 || gv != 0 {
			t.Errorf("after the delete: (%d, %d), want gone", len(got), gv)
		}
	})

	// systemThread stores an unowned conversation under key, as every row before v52 and the release before's writes
	// during a rolling deploy are; contextBlockID is its messages' context block.
	systemThread := func(t *testing.T, flowID, key, contextBlockID string) int64 {
		t.Helper()
		m := msg("l-"+key, "legacy")
		m.ContextBlockID = contextBlockID
		if err := b.SaveConversation(ctx, flowID, key, []interfaces.ChatMessage{m}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), flowID, key) })
		_, v := load(t, flowID, key, "")
		return v
	}

	// An unowned thread under a client-generated thread id (a uuid no message names as its context block) is claimed
	// by the first write that names an owner: whoever holds that unguessable key is its creator. From then on it is
	// that user's: in their list, refused to anyone else.
	run("ConversationOwner_unowned_thread_is_claimed_by_its_first_owned_write", func(t *testing.T) {
		flowID, key := "contract-co-claim-"+runID, uuid.NewString()
		v := systemThread(t, flowID, key, "some-block")
		if shared, err := b.IsSharedConversation(ctx, flowID, key); err != nil || shared {
			t.Errorf("an unowned thread under a thread id: IsSharedConversation = (%v, %v), want false (claimable)", shared, err)
		}
		if shared, err := b.IsSharedConversation(ctx, flowID, "missing-"+key); err != nil || shared {
			t.Errorf("a missing conversation: IsSharedConversation = (%v, %v), want false", shared, err)
		}
		v2, err := b.SaveConversationIf(ctx, flowID, key, ownerA, v, []interfaces.ChatMessage{msg("l-"+key, "legacy"), msg("a1", "A continues")})
		if err != nil {
			t.Fatalf("A's write to the unowned thread: %v", err)
		}
		if got, gv := load(t, flowID, key, ownerB); len(got) != 0 || gv != 0 {
			t.Errorf("B still reads the thread A's write claimed: (%d, %d)", len(got), gv)
		}
		if l := list(t, flowID, ownerA); len(l) != 1 || l[0].Scope != key || l[0].Version != v2 {
			t.Errorf("A's list = %+v, want the claimed thread at %d", l, v2)
		}
		if _, err := b.SaveConversationIf(ctx, flowID, key, ownerB, v2, nil); !errors.Is(err, interfaces.ErrConversationKeyTaken) {
			t.Errorf("B's write after the claim: err = %v, want ErrConversationKeyTaken", err)
		}
		// A system write (no owner) claims nothing and leaves the owner.
		key2 := uuid.NewString()
		v = systemThread(t, flowID, key2, "")
		if _, err := b.SaveConversationIf(ctx, flowID, key2, "", v, []interfaces.ChatMessage{msg("x", "system")}); err != nil {
			t.Fatal(err)
		}
		if got, _ := load(t, flowID, key2, ownerB); len(got) != 1 {
			t.Error("a system write took the unowned thread")
		}
	})

	run("ConversationOwner_unowned_thread_is_claimed_by_a_title_and_deletable_by_its_holder", func(t *testing.T) {
		flowID := "contract-co-claim2-" + runID
		named := uuid.NewString()
		v := systemThread(t, flowID, named, "")
		tv, err := b.SetConversationTitle(ctx, flowID, named, ownerA, "mine")
		if err != nil || tv != v {
			t.Fatalf("A naming the unowned thread = (%d, %v), want its version %d", tv, err, v)
		}
		if l := list(t, flowID, ownerA); len(l) != 1 || l[0].Title != "mine" {
			t.Errorf("A's list = %+v, want the named, claimed thread", l)
		}
		closed := uuid.NewString()
		v = systemThread(t, flowID, closed, "")
		if err := b.DeleteConversationIf(ctx, flowID, closed, ownerA, v, false); err != nil {
			t.Errorf("A closing an unowned thread under its own thread id: %v, want it deleted", err)
		}
		if got, _ := load(t, flowID, closed, ""); len(got) != 0 {
			t.Error("the unowned thread survived its holder's delete")
		}
	})

	// A block-id key (a uuid every viewer of the flow sees, and that the thread's messages name as their context block)
	// and the "flow" key are predictable: such an unowned conversation is never claimed, stays shared, and is deleted
	// or named only with the flow right.
	run("ConversationOwner_block_and_flow_keys_stay_unowned", func(t *testing.T) {
		flowID := "contract-co-shared-" + runID
		for _, key := range []string{uuid.NewString(), "flow"} {
			block := ""
			if key != "flow" {
				block = key
			}
			v := systemThread(t, flowID, key, block)
			if shared, err := b.IsSharedConversation(ctx, flowID, key); err != nil || !shared {
				t.Errorf("%s: IsSharedConversation = (%v, %v), want true", key, shared, err)
			}
			m := msg("l-"+key, "legacy")
			m.ContextBlockID = block
			v2, err := b.SaveConversationIf(ctx, flowID, key, ownerA, v, []interfaces.ChatMessage{m, msg("a1", "A's turn")})
			if err != nil {
				t.Fatalf("%s: A's append: %v", key, err)
			}
			if got, gv := load(t, flowID, key, ownerB); gv != v2 || len(got) != 2 {
				t.Errorf("%s: A's append took the shared conversation: B reads (%d, %d)", key, len(got), gv)
			}
			if _, err := b.SetConversationTitle(ctx, flowID, key, ownerA, "x"); !errors.Is(err, interfaces.ErrConversationUnowned) {
				t.Errorf("%s: naming it: err = %v, want ErrConversationUnowned", key, err)
			}
			if err := b.DeleteConversationIf(ctx, flowID, key, ownerA, v2, false); !errors.Is(err, interfaces.ErrConversationUnowned) {
				t.Errorf("%s: deleting it without the flow right: err = %v, want ErrConversationUnowned", key, err)
			}
		}
		if l := list(t, flowID, ownerA); len(l) != 0 {
			t.Errorf("A's list = %v, want none", scopes(l))
		}
	})

	run("ConversationOwner_list_is_the_owners_threads_newest_first", func(t *testing.T) {
		flowID, other := "contract-co-list-"+runID, "contract-co-list2-"+runID
		create(t, flowID, "t1", ownerA, msg("m1", "one"))
		v2 := create(t, flowID, "t2", ownerA, msg("m1", "one"), msg("m2", "two"))
		create(t, flowID, "t3", ownerA)
		create(t, flowID, "tb", ownerB, msg("m1", "B's"))
		create(t, other, "t9", ownerA, msg("m1", "another flow"))
		// t1 changes last (a tick later than the creates, so its updated_at is the newest on any clock).
		time.Sleep(2 * time.Millisecond)
		if _, err := b.SaveConversationIf(ctx, flowID, "t1", ownerA, mustVersion(t, b, flowID, "t1"), []interfaces.ChatMessage{msg("m1", "one"), msg("m3", "three")}); err != nil {
			t.Fatalf("update t1: %v", err)
		}
		got := list(t, flowID, ownerA)
		if s := scopes(got); !slices.Equal(sortedScopes(s), []string{"t1", "t2", "t3"}) {
			t.Fatalf("A's list = %v, want t1, t2 and t3 only", s)
		}
		for i := 1; i < len(got); i++ {
			p, c := got[i-1], got[i]
			if p.UpdatedAt.Before(c.UpdatedAt) || p.UpdatedAt.Equal(c.UpdatedAt) && p.Scope < c.Scope {
				t.Errorf("list order: %s (%v) before %s (%v), want updated_at then key descending", p.Scope, p.UpdatedAt, c.Scope, c.UpdatedAt)
			}
		}
		if got[0].Scope != "t1" {
			t.Errorf("the conversation changed last is %s, want t1 first", got[0].Scope)
		}
		for _, c := range got {
			if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() || c.Version <= 0 {
				t.Errorf("%s: summary %+v lacks its times or version", c.Scope, c)
			}
			want := map[string]int{"t1": 2, "t2": 2, "t3": 0}[c.Scope]
			if c.MessageCount != want {
				t.Errorf("%s: messageCount = %d, want %d", c.Scope, c.MessageCount, want)
			}
			if c.Scope == "t2" && c.Version != v2 {
				t.Errorf("t2: version = %d, want %d", c.Version, v2)
			}
		}
		if s := scopes(list(t, flowID, ownerB)); !slices.Equal(s, []string{"tb"}) {
			t.Errorf("B's list = %v, want [tb]", s)
		}
		if l, err := b.ListConversations(ctx, flowID, "", nil, 100); err != nil || len(l) != 0 {
			t.Errorf("the empty owner's list = (%v, %v), want none", scopes(l), err)
		}
		// Keyset paging: two pages of two and one, the second after the first page's last entry.
		first, err := b.ListConversations(ctx, flowID, ownerA, nil, 2)
		if err != nil || len(first) != 2 {
			t.Fatalf("first page = (%v, %v), want two", scopes(first), err)
		}
		last := first[len(first)-1]
		rest, err := b.ListConversations(ctx, flowID, ownerA, &interfaces.PageCursor{At: last.UpdatedAt, ID: last.Scope}, 2)
		if err != nil {
			t.Fatalf("second page: %v", err)
		}
		if s := append(scopes(first), scopes(rest)...); !slices.Equal(s, scopes(got)) {
			t.Errorf("paged list = %v, want %v", s, scopes(got))
		}
	})

	run("ConversationOwner_title_names_the_owners_thread_without_a_new_version", func(t *testing.T) {
		flowID := "contract-co-title-" + runID
		v := create(t, flowID, "k", ownerA, msg("m1", "hi"))
		if _, err := b.SetConversationTitle(ctx, flowID, "k", ownerA, "Löwe 🦁 plan"); err != nil {
			t.Fatalf("SetConversationTitle: %v", err)
		}
		if _, gv := load(t, flowID, "k", ownerA); gv != v {
			t.Errorf("naming moved the version %d -> %d", v, gv)
		}
		if l := list(t, flowID, ownerA); len(l) != 1 || l[0].Title != "Löwe 🦁 plan" || l[0].Version != v {
			t.Errorf("list after naming = %+v, want the title at version %d", l, v)
		}
		// A later save keeps the name.
		if _, err := b.SaveConversationIf(ctx, flowID, "k", ownerA, v, []interfaces.ChatMessage{msg("m1", "hi"), msg("m2", "more")}); err != nil {
			t.Fatalf("save after naming: %v", err)
		}
		if l := list(t, flowID, ownerA); len(l) != 1 || l[0].Title != "Löwe 🦁 plan" {
			t.Errorf("the save dropped the title: %+v", l)
		}
		if _, err := b.SetConversationTitle(ctx, flowID, "k", ownerA, ""); err != nil {
			t.Fatalf("SetConversationTitle(\"\"): %v", err)
		}
		if l := list(t, flowID, ownerA); len(l) != 1 || l[0].Title != "" {
			t.Errorf("the empty title did not remove the name: %+v", l)
		}
		// A thread named before its first turn: created empty, with the name, and the first turn's create-at-0 is a
		// conflict the writer reloads from (it reads version and empty list).
		early := uuid.NewString()
		tv, err := b.SetConversationTitle(ctx, flowID, early, ownerA, "named first")
		if err != nil {
			t.Fatalf("SetConversationTitle on a missing conversation: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), flowID, early) })
		got, ev := load(t, flowID, early, ownerA)
		if len(got) != 0 || ev <= 0 || tv != ev {
			t.Errorf("a named missing conversation = (%d messages, version %d; title answered %d), want an empty conversation with its version", len(got), ev, tv)
		}
		if _, gv := load(t, flowID, early, ownerB); gv != 0 {
			t.Errorf("the named conversation is not A's: B reads version %d", gv)
		}
		// Under a key that is not a thread id a title creates nothing: it would take a predictable key from everyone.
		for _, key := range []string{"early", "flow", strings.ToUpper(early)} {
			if _, err := b.SetConversationTitle(ctx, flowID, key, ownerA, "squat"); !errors.Is(err, interfaces.ErrNotFound) {
				t.Errorf("naming the missing %q: err = %v, want ErrNotFound", key, err)
			}
			if _, gv := load(t, flowID, key, ""); gv != 0 {
				t.Errorf("naming the missing %q stored a conversation", key)
			}
		}
		if _, err := b.SetConversationTitle(ctx, flowID, "k", "", "system"); !errors.Is(err, interfaces.ErrConversationUnowned) {
			t.Errorf("naming without an owner: err = %v, want ErrConversationUnowned", err)
		}
	})

	run("ConversationOwner_deleting_the_flow_takes_every_users_threads", func(t *testing.T) {
		flowID := "contract-co-delflow-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: ownerA, Email: ownerA + "@example.com", Password: "hash"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), ownerA) })
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: flowID, Name: "F", OwnerID: ownerA, Content: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
		create(t, flowID, "a", ownerA, msg("m", "A"))
		create(t, flowID, "b", ownerB, msg("m", "B"))
		if err := b.SaveConversation(ctx, flowID, "flow", []interfaces.ChatMessage{msg("m", "legacy")}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		if err := b.DeleteFlow(ctx, flowID); err != nil {
			t.Fatalf("DeleteFlow: %v", err)
		}
		for _, scope := range []string{"a", "b", "flow"} {
			if got, v := load(t, flowID, scope, ""); len(got) != 0 || v != 0 {
				t.Errorf("%s survived the flow's deletion", scope)
			}
		}
	})

	run("ConversationOwner_erasure_and_export_follow_the_owner", func(t *testing.T) {
		u, v := "contract-co-eu-"+runID, "contract-co-ev-"+runID
		for _, id := range []string{u, v} {
			if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash"}); err != nil {
				t.Fatalf("CreateUser %s: %v", id, err)
			}
			uid := id
			t.Cleanup(func() { _ = b.DeleteUser(context.Background(), uid) })
		}
		own, theirs := "contract-co-eown-"+runID, "contract-co-etheirs-"+runID
		for _, f := range []*interfaces.FlowDocument{{ID: own, Name: "Own", OwnerID: u}, {ID: theirs, Name: "Theirs", OwnerID: v}} {
			f.Content = json.RawMessage(`{}`)
			if err := b.SaveFlow(ctx, f); err != nil {
				t.Fatalf("SaveFlow: %v", err)
			}
			id := f.ID
			t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), id) })
		}
		create(t, own, "u-own", u, msg("m", "u on own"))
		create(t, own, "v-on-u", v, msg("m", "v on u's flow"))
		create(t, theirs, "u-on-v", u, msg("m", "u on v's flow"))
		create(t, theirs, "v-own", v, msg("m", "v on own"))
		if _, err := b.SetConversationTitle(ctx, theirs, "u-on-v", u, "u's thread"); err != nil {
			t.Fatalf("SetConversationTitle: %v", err)
		}
		if err := b.SaveConversation(ctx, theirs, "flow", []interfaces.ChatMessage{msg("m", "legacy on v's flow")}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), theirs, "flow") })
		if err := b.SaveConversation(ctx, own, "flow", []interfaces.ChatMessage{msg("m", "legacy on u's flow")}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), own, "flow") })

		// The export: u's own threads wherever they are, and the unowned ones of u's flows; never v's threads on u's
		// flow (v's content).
		exp, err := b.ExportUserData(ctx, u)
		if err != nil {
			t.Fatalf("ExportUserData: %v", err)
		}
		var keys []string
		for _, c := range exp.Conversations {
			keys = append(keys, c.FlowID+"/"+c.Scope)
			if c.Scope == "u-on-v" && c.Title != "u's thread" {
				t.Errorf("exported u-on-v title = %q, want the owner's name", c.Title)
			}
		}
		want := []string{own + "/flow", own + "/u-own", theirs + "/u-on-v"}
		if !slices.Equal(sortedScopes(keys), sortedScopes(want)) {
			t.Errorf("exported conversations = %v, want %v", keys, want)
		}

		if err := b.DeleteUser(ctx, u); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		gone := map[string]bool{own + "/u-own": true, own + "/v-on-u": true, own + "/flow": true, theirs + "/u-on-v": true}
		for _, key := range []string{own + "/u-own", own + "/v-on-u", own + "/flow", theirs + "/u-on-v", theirs + "/v-own", theirs + "/flow"} {
			flowID, scope, _ := strings.Cut(key, "/")
			got, _ := load(t, flowID, scope, "")
			if gone[key] && len(got) != 0 {
				t.Errorf("%s survived the erasure", key)
			}
			if !gone[key] && len(got) == 0 {
				t.Errorf("%s was erased with u: it is not u's", key)
			}
		}
	})
}

// mustVersion is the current version of a conversation as the system reads it.
func mustVersion(t *testing.T, b interfaces.StorageBackend, flowID, scope string) int64 {
	t.Helper()
	_, v, err := b.LoadConversationVersioned(context.Background(), flowID, scope, "")
	if err != nil {
		t.Fatalf("LoadConversationVersioned: %v", err)
	}
	return v
}

func sortedScopes(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
