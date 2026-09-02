package analyzer

import (
	"context"
	"sync"
	"testing"

	"pad-core/parser"
)

// B1.2: concurrent identical analyses dedup to ONE full walk (singleflight).
func TestCachedAnalysisCtx_SingleflightDedup(t *testing.T) {
	src := "#Region \"Main\"\n    SET A TO 1\n    SET B TO 2\n#EndRegion\n"
	doc, err := parser.ParseText(src, "SFDedup.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	DefaultCache.Clear()
	defer DefaultCache.Clear()

	// Count rule-progress callbacks across ALL callers: with dedup only the
	// leader's walk fires progress — followers join its result.
	var mu sync.Mutex
	totalCallbacks := 0
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			CachedAnalysisCtx(context.Background(), doc, nil, nil, func(cur, tot int, name string) {
				mu.Lock()
				totalCallbacks++
				mu.Unlock()
			})
		}()
	}
	close(start)
	wg.Wait()

	// One walk's callback count = rulesRun-ish; assert it's a single walk's
	// worth, not 8x: measure one walk on a distinct flow.
	doc2, _ := parser.ParseText(src, "SFDedup2.txt", int64(len(src)))
	var oneWalk int
	CachedAnalysisCtx(context.Background(), doc2, nil, nil, func(cur, tot int, name string) { oneWalk++ })

	mu.Lock()
	defer mu.Unlock()
	if totalCallbacks != oneWalk {
		t.Errorf("progress callbacks = %d, want %d (one walk — 8 concurrent callers deduped)", totalCallbacks, oneWalk)
	}
}
