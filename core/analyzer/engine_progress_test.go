package analyzer

import (
	"sync"
	"testing"
)

// TestRunAnalysis_ProgressOnlyOnPercentChange: progress used to fire every 50
// (block, rule) checks per shard — thousands of events per analysis, each one
// an SSE broadcast under a global lock. It must fire only when the integer
// percentage advances (at most 101 calls), strictly increasing, and always end
// with the final current == total call.
func TestRunAnalysis_ProgressOnlyOnPercentChange(t *testing.T) {
	doc := buildLargeFlow(2000)
	type ev struct{ cur, total int }
	var mu sync.Mutex
	var events []ev
	report := RunAnalysis(doc, AllRules(), nil, func(cur, total int, _ string) {
		mu.Lock()
		events = append(events, ev{cur, total})
		mu.Unlock()
	})
	total := report.Stats.BlocksAnalyzed * report.Stats.RulesRun
	if total/progressFlushEvery <= 101 {
		t.Fatalf("flow too small to tell throttled from unthrottled progress: %d checks", total)
	}

	if len(events) > 101 {
		t.Fatalf("got %d progress events for %d checks; want at most 101 (one per percent)", len(events), total)
	}
	if len(events) < 50 {
		t.Fatalf("got only %d progress events; progress should still advance through the percentages", len(events))
	}
	last := events[len(events)-1]
	if last.cur != total || last.total != total {
		t.Fatalf("final progress = %d/%d, want %d/%d", last.cur, last.total, total, total)
	}
	prevPct := -1
	for i, e := range events {
		pct := e.cur * 100 / e.total
		if pct <= prevPct {
			t.Fatalf("event %d: percentage %d did not advance past %d", i, pct, prevPct)
		}
		prevPct = pct
	}
}
