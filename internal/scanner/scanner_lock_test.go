package scanner

import (
	"context"
	"sync/atomic"
	"testing"

	"pad-core/models"

	"pad-analyzer/internal/notify"
)

// lockingBackend adds the Postgres advisory-lock method to the in-memory
// backend so the cross-replica guard in ScanOnce is exercised.
type lockingBackend struct {
	*healthBackend
	held     bool
	released atomic.Int32
	key      int64
}

func (b *lockingBackend) TryGlobalLock(_ context.Context, key int64) (func(), bool, error) {
	b.key = key
	if b.held {
		return nil, false, nil
	}
	return func() { b.released.Add(1) }, true, nil
}

// countingAnalyze records how many flows a sweep analysed.
func countingAnalyze(n *atomic.Int32) AnalyzeFunc {
	return func(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		n.Add(1)
		return &models.AnalysisReport{FlowID: doc.ID}, nil
	}
}

// TestScanOnce_SkipsWhileAnotherReplicaHoldsTheLock pins the multi-replica
// guard: every pod runs its own ticker, and without the lock N replicas each
// re-analysed every flow and dispatched the same alert N times.
func TestScanOnce_SkipsWhileAnotherReplicaHoldsTheLock(t *testing.T) {
	b := &lockingBackend{healthBackend: newBackend(), held: true}
	seedFlow(t, b, "f1")
	var analysed atomic.Int32

	s := New(b, countingAnalyze(&analysed), nil, 0, notify.AllowLoopbackTargets)
	s.ScanOnce(context.Background())

	if got := analysed.Load(); got != 0 {
		t.Fatalf("a replica without the lock must not sweep, analysed %d flow(s)", got)
	}
	if b.key != scanLockKey {
		t.Errorf("sweep must ask for scanLockKey, asked for %#x", b.key)
	}
}

// TestScanOnce_SweepsAndReleasesWhenLockAcquired: the holder sweeps normally
// and releases the session lock exactly once afterwards.
func TestScanOnce_SweepsAndReleasesWhenLockAcquired(t *testing.T) {
	b := &lockingBackend{healthBackend: newBackend()}
	seedFlow(t, b, "f1")
	var analysed atomic.Int32

	s := New(b, countingAnalyze(&analysed), nil, 0, notify.AllowLoopbackTargets)
	s.ScanOnce(context.Background())

	if got := analysed.Load(); got != 1 {
		t.Fatalf("lock holder should analyse the stored flow once, analysed %d", got)
	}
	if got := b.released.Load(); got != 1 {
		t.Errorf("lock must be released exactly once, released %d times", got)
	}
}
