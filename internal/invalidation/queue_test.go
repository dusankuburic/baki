package invalidation

import (
	"context"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/platform/eventbus"
)

// newBare builds a fan-out that is never started: the queue and the receiver's bookkeeping, without a Redis.
func newBare(opts ...Option) *Fanout {
	return New(nil, nil, eventbus.New(nil, 0), "replica-1", opts...)
}

func queued(f *Fanout) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.queue {
		out = append(out, p.kind+":"+p.key+"{"+p.scope.String()+"}")
	}
	return out
}

func TestQueue_KeepsOrderAndMergesWhatIsAlreadyWaiting(t *testing.T) {
	f := newBare()
	ctx := context.Background()
	f.PublishInvalidation(ctx, "settings.deployment", nil)
	f.PublishInvalidation(ctx, "rules.deployment-profile", nil)
	f.PublishInvalidation(ctx, "rules.profiles", eventbus.NewScope("org", "o1"))
	f.PublishInvalidation(ctx, "settings.deployment", nil)                          // identical to the first, still waiting
	f.PublishInvalidation(ctx, "rules.profiles", eventbus.NewScope("org", "o2"))    // another scope: kept
	f.PublishInvalidation(ctx, "recheck.ws-access", eventbus.NewScope("what", "x")) // a recheck, on the wire as kind recheck
	want := []string{
		"inval:settings.deployment{}",
		"inval:rules.deployment-profile{}",
		"inval:rules.profiles{org=o1}",
		"inval:rules.profiles{org=o2}",
		"recheck:recheck.ws-access{what=x}",
	}
	if got := queued(f); !slices.Equal(got, want) {
		t.Errorf("queue = %v, want %v (order kept: settings are applied before the profiles resolved from them)", got, want)
	}
}

func TestQueue_TheCallersScopeIsCopied(t *testing.T) {
	f := newBare()
	scope := eventbus.NewScope("org", "o1")
	f.PublishInvalidation(context.Background(), "rules.profiles", scope)
	scope["org"] = "mutated"
	if got := queued(f); !slices.Equal(got, []string{"inval:rules.profiles{org=o1}"}) {
		t.Errorf("queue = %v: the queued scope followed the caller's map", got)
	}
}

// A burst past the limit costs one message: a resync, which tells the peers to drop everything. Nothing is lost by
// dropping the individual messages, because the resync reaches them after every one of those writes committed.
func TestQueue_OverflowCollapsesIntoOneResync(t *testing.T) {
	f := newBare(WithQueueLimit(3))
	ctx := context.Background()
	before := counter(dropped, "queue_full")
	for _, org := range []string{"o1", "o2", "o3"} {
		f.PublishInvalidation(ctx, "rules.profiles", eventbus.NewScope("org", org))
	}
	f.PublishInvalidation(ctx, "rules.profiles", eventbus.NewScope("org", "o4")) // over the limit
	if got := queued(f); !slices.Equal(got, []string{"resync:{}"}) {
		t.Fatalf("queue = %v, want a single resync", got)
	}
	f.PublishInvalidation(ctx, "rules.profiles", eventbus.NewScope("org", "o5")) // covered by the resync still waiting
	f.PublishInvalidation(ctx, "settings.deployment", nil)
	if got := queued(f); !slices.Equal(got, []string{"resync:{}"}) {
		t.Errorf("queue = %v: a message queued behind a resync that covers it", got)
	}
	if d := counter(dropped, "queue_full") - before; d != 6 {
		t.Errorf("dropped{queue_full} grew by %v, want 6 (the three waiting, the overflowing one, and the two covered by the resync)", d)
	}
}

func TestTracker_EpochsPerOrigin(t *testing.T) {
	type step struct {
		origin string
		kind   string
		epoch  uint64
		gap    bool
	}
	for name, steps := range map[string][]step{
		"the first sight of an origin is its baseline": {
			{"a", "inval", 5, false}, {"a", "inval", 6, false},
		},
		"a skipped epoch is a gap": {
			{"a", "inval", 1, false}, {"a", "inval", 3, true}, {"a", "inval", 4, false},
		},
		"origins are independent": {
			{"a", "inval", 1, false}, {"b", "inval", 7, false}, {"a", "inval", 2, false}, {"b", "inval", 8, false},
		},
		"a heartbeat carries the last epoch sent: equal is fine, ahead is a gap": {
			{"a", "inval", 1, false}, {"a", "hb", 1, false}, {"a", "hb", 3, true}, {"a", "hb", 3, false},
		},
		"a heartbeat first sets the baseline": {
			{"a", "hb", 0, false}, {"a", "inval", 1, false}, {"a", "inval", 2, false},
		},
		"an epoch that goes back is a restarted origin, not a gap": {
			{"a", "inval", 40, false}, {"a", "inval", 1, false}, {"a", "inval", 2, false},
		},
		"a repeated message is not a gap": {
			{"a", "inval", 1, false}, {"a", "inval", 1, false},
		},
	} {
		t.Run(name, func(t *testing.T) {
			tr := newTracker()
			for i, s := range steps {
				if got := tr.observe(s.origin, s.kind, s.epoch); got != s.gap {
					t.Errorf("step %d (%s %s %d): gap = %v, want %v", i, s.origin, s.kind, s.epoch, got, s.gap)
				}
			}
		})
	}
}

func TestTracker_ForgetsOriginsThatWentQuiet(t *testing.T) {
	tr := newTracker()
	tr.observe("gone", "inval", 1)
	tr.last["gone"] = tracked{epoch: 1, at: tr.now().Add(-time.Hour)}
	tr.prune()
	if _, ok := tr.last["gone"]; ok {
		t.Error("an origin silent for an hour is still tracked")
	}
}
