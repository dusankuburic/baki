package analyzer

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestCachedAnalysisCtx_CancelledWalkIsNeverCached is the audit repro: a walk
// cancelled mid-way used to be stored in DefaultCache and returned as if
// complete (8 findings cached for a flow whose true result is 640), and the
// next normal call was served that truncated report.
func TestCachedAnalysisCtx_CancelledWalkIsNeverCached(t *testing.T) {
	DefaultCache.Clear()
	defer DefaultCache.Clear()

	doc := buildLargeFlow(3000)
	doc.ID = "cancel-not-cached"
	want := len(RunAnalysis(buildLargeFlow(3000), AllRules(), nil, nil).Findings)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel on the first progress tick: the shards are mid-walk.
	var once sync.Once
	got := CachedAnalysisCtx(ctx, doc, AllRules(), nil, func(int, int, string) { once.Do(cancel) })
	if got != nil {
		t.Fatalf("cancelled walk returned a report (%d of %d findings); want nil", len(got.Findings), want)
	}
	if n := len(DefaultCache.AllReports()); n != 0 {
		t.Fatalf("cancelled walk left %d report(s) in DefaultCache; want 0", n)
	}

	full := CachedAnalysis(doc, AllRules(), nil, nil)
	if full == nil || len(full.Findings) != want {
		n := -1
		if full != nil {
			n = len(full.Findings)
		}
		t.Fatalf("normal call after a cancelled one got %d findings, want %d", n, want)
	}
}

// TestCachedAnalysisCtx_JoinerOfCancelledLeaderGetsCompleteReport: a
// singleflight joiner runs under the LEADER's context. When the leader is
// cancelled mid-walk, a joiner whose own context is live must get the complete
// report (by re-running), never the leader's truncated one.
func TestCachedAnalysisCtx_JoinerOfCancelledLeaderGetsCompleteReport(t *testing.T) {
	DefaultCache.Clear()
	defer DefaultCache.Clear()

	doc := buildLargeFlow(3000)
	doc.ID = "cancel-joiner"
	want := len(RunAnalysis(buildLargeFlow(3000), AllRules(), nil, nil).Findings)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	leaderDone := make(chan bool)
	go func() {
		r := CachedAnalysisCtx(leaderCtx, doc, AllRules(), nil, func(int, int, string) {
			// Hold the leader mid-walk until the joiner has joined the flight,
			// then cancel it.
			once.Do(func() {
				close(started)
				<-release
				cancelLeader()
			})
		})
		leaderDone <- r == nil
	}()
	<-started

	joinerDone := make(chan int)
	go func() {
		r := CachedAnalysisCtx(context.Background(), doc, AllRules(), nil, nil)
		if r == nil {
			joinerDone <- -1
			return
		}
		joinerDone <- len(r.Findings)
	}()
	// The joiner only hashes (memoized) and misses the cache before entering
	// the flight; give it ample time to block on the in-flight leader.
	time.Sleep(200 * time.Millisecond)
	close(release)

	if !<-leaderDone {
		t.Error("cancelled leader received a report; want nil")
	}
	if got := <-joinerDone; got != want {
		t.Fatalf("joiner of a cancelled leader got %d findings, want the complete %d", got, want)
	}
	if r := CachedAnalysis(doc, AllRules(), nil, nil); r == nil || len(r.Findings) != want {
		t.Fatalf("cache holds an incomplete report after the joiner's re-run")
	}
}
