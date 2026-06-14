package search

import (
	"sync"
	"testing"

	"pad-core/models"
)

// TestSearchIndex_ConcurrentSearchAndRebuild stresses the actual usage
// pattern from FlowService: many concurrent Search calls plus rebuilds
// that swap a fresh index pointer. The race detector must not flag any
// races. This locks in our invariant: a SearchIndex is immutable after
// construction (indexBlock is only called from NewSearchIndex), so
// concurrent readers on a stable pointer are safe without a mutex on
// the index itself — the only synchronization needed is on whichever
// pointer holds the current index (FlowService.docMu).
//
// If a future change starts mutating SearchIndex in place (e.g. an
// AddBlock method that appends to the maps post-construction), this
// test will fail under -race, signaling that the index needs its own
// sync.RWMutex.
func TestSearchIndex_ConcurrentSearchAndRebuild(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	const (
		readers = 16
		rounds  = 200
	)
	var wg sync.WaitGroup
	wg.Add(readers + 1)

	// Many readers: hold a pointer to one index and search it repeatedly.
	// (FlowService might publish a new index pointer between iterations;
	// each reader is a snapshot, mirroring real RLock-then-dereference.)
	for r := range readers {
		go func(seed int) {
			defer wg.Done()
			snap := idx
			for i := range rounds {
				if seed%4 == 0 && i%10 == 0 {
					// Some readers periodically pick up the "latest" index,
					// the way SearchFlow re-reads s.searchIndex.
					snap = NewSearchIndex(doc.ID, doc)
				}
				_ = snap.Search(models.SearchQuery{Text: "excel"})
				_ = snap.Search(models.SearchQuery{Text: "click", Fuzzy: true})
				_ = snap.Search(models.SearchQuery{Text: "loop"})
			}
		}(r)
	}

	// One rebuilder: spam fresh index constructions to provoke any racy
	// initialization. The pointer swap itself is FlowService's concern;
	// here we just exercise NewSearchIndex concurrent with Search.
	go func() {
		defer wg.Done()
		for range rounds {
			_ = NewSearchIndex(doc.ID, doc)
		}
	}()

	wg.Wait()
}
