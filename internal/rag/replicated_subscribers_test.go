package rag

import (
	"context"
	"slices"
	"testing"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/testutil"
)

// The corpus count cache tells the other replicas which org's count it dropped, and nothing for an event the local
// drop ignores (an empty scope would mean every org's count).
func TestCorpusCache_ReplicatesTheOrgItDrops(t *testing.T) {
	bus := eventbus.New(nil, 0)
	rec := &eventbus.ReplicasRecorder{}
	bus.SetReplicas(rec)
	svc := NewKnowledgeService(testutil.NewFakeBackend(), nil, nil, WithEventBus(bus))
	SubscribeCorpusCache(bus, svc)
	for _, tc := range []struct {
		name  string
		event eventbus.Event
		want  []string
	}{
		{"document added", events.KnowledgeDocChanged{OrgID: "o1", DocID: "d1", Kind: events.KnowledgeDocAdded}, []string{"rag.corpus-counts{org=o1}"}},
		{"re-index of every document", events.KnowledgeDocChanged{OrgID: "o1", Kind: events.KnowledgeDocReindexed}, []string{"rag.corpus-counts{org=o1}"}},
		{"no org", events.KnowledgeDocChanged{DocID: "d1", Kind: events.KnowledgeDocAdded}, nil},
		{"org deleted", events.OrgMemberChanged{OrgID: "o1", Kind: events.OrgDeleted}, []string{"rag.corpus-counts{org=o1}"}},
		{"member added", events.OrgMemberChanged{OrgID: "o1", UserID: "u1", Kind: events.OrgMemberAdded}, nil},
	} {
		rec.Reset()
		bus.Publish(context.Background(), tc.event)
		if got := rec.Published(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: published %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A peer's corpus change reaches the count cache through its applier: the org's count is no longer trusted (it
// reads as unknown, so a search embeds as it always did), and an empty scope (the receiver's resync after a gap)
// drops every org's count.
func TestCorpusCache_AppliesARemoteInvalidation(t *testing.T) {
	store := &countStore{FakeBackend: &testutil.FakeBackend{}}
	bus := eventbus.New(nil, 0)
	svc := NewKnowledgeService(store, nil, nil, WithEventBus(bus))
	SubscribeCorpusCache(bus, svc)
	ctx := context.Background()
	counted := func() int { return int(store.counts.Load()) }

	svc.corpusEmpty(ctx, "o1")
	svc.corpusEmpty(ctx, "o2")
	svc.corpusEmpty(ctx, "o1")
	if counted() != 2 {
		t.Fatalf("premise: two orgs counted once each, got %d reads", counted())
	}
	if !bus.DeliverRemote("rag.corpus-counts", eventbus.NewScope("org", "o1")) {
		t.Fatal("no applier for rag.corpus-counts")
	}
	if got := svc.corpusEmpty(ctx, "o1"); got {
		t.Error("an org whose corpus a peer changed reads as empty (it should read as unknown until the settle window passes)")
	}
	if counted() != 2 {
		t.Errorf("o1 was counted again at once (%d reads): the settle marker should hold", counted())
	}
	svc.corpusEmpty(ctx, "o2")
	if counted() != 2 {
		t.Errorf("o2's count was dropped by o1's invalidation (%d reads)", counted())
	}

	bus.DeliverRemote("rag.corpus-counts", nil)
	svc.corpusEmpty(ctx, "o1")
	svc.corpusEmpty(ctx, "o2")
	if counted() != 4 {
		t.Errorf("after the whole-cache drop %d reads, want 4 (both orgs counted again)", counted())
	}
}

// A count read before the whole-cache drop is refused, as for a single org.
func TestCorpusCache_WholeDropFencesCountsInFlight(t *testing.T) {
	svc := NewKnowledgeService(testutil.NewFakeBackend(), nil, nil)
	c := svc.caches()
	captured := c.gen.Load("o1")
	svc.InvalidateAllCorpora()
	if c.gen.FillOK(context.Background(), "o1", captured) {
		t.Error("a count read before InvalidateAllCorpora may still be stored")
	}
}
