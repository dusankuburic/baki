package app

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/lifecycle"
)

// postBootStub records what runPostBootIndexWork asked of the database.
type postBootStub struct {
	upkeepErr      error
	vectorCalls    int
	vectorDeadline time.Time // the build's context, when it was called
	vectorHas      bool
	vectorErr      error
}

func (s *postBootStub) SchemaUpkeep(context.Context) error { return s.upkeepErr }
func (s *postBootStub) EnsureVectorIndex(ctx context.Context, _ int) error {
	s.vectorCalls++
	s.vectorDeadline, s.vectorHas = ctx.Deadline()
	s.vectorErr = ctx.Err()
	return nil
}
func (s *postBootStub) EmbeddingDim() int { return 1536 }

// TestPostBootIndexWork_NoBuildAfterShutdownStoppedTheUpkeep: shutdown's drain stops the upkeep job while the index
// context is still live (the storage stop hook cancels it later); the HNSW build must not start then, on a new job
// the drain has already passed (UPK-1).
func TestPostBootIndexWork_NoBuildAfterShutdownStoppedTheUpkeep(t *testing.T) {
	s := &postBootStub{upkeepErr: lifecycle.ErrJobStopped}
	runPostBootIndexWork(context.Background(), s)
	if s.vectorCalls != 0 {
		t.Errorf("the HNSW build started %d time(s) after shutdown stopped the upkeep", s.vectorCalls)
	}
}

// TestPostBootIndexWork_TheBuildHasABudgetOfItsOwn: an upkeep held up past its budget (a long transaction pins its
// concurrent builds) still leaves the HNSW build its whole budget, never an expired context.
func TestPostBootIndexWork_TheBuildHasABudgetOfItsOwn(t *testing.T) {
	s := &postBootStub{upkeepErr: context.DeadlineExceeded}
	runPostBootIndexWork(context.Background(), s)
	if s.vectorCalls != 1 {
		t.Fatalf("the HNSW build ran %d times, want once", s.vectorCalls)
	}
	if s.vectorErr != nil || !s.vectorHas || time.Until(s.vectorDeadline) < vectorIndexBuildTimeout-time.Minute {
		t.Errorf("the HNSW build got a context with deadline %v (set %v), err %v; want its own %v", s.vectorDeadline, s.vectorHas, s.vectorErr, vectorIndexBuildTimeout)
	}
}
