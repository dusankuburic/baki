package api

import (
	"sync"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

func TestWaitWithBudget(t *testing.T) {
	var done sync.WaitGroup
	if !waitWithBudget(&done, time.Second) {
		t.Error("a completed WaitGroup must return true immediately")
	}
	var stuck sync.WaitGroup
	stuck.Add(1)
	defer stuck.Done()
	start := time.Now()
	if waitWithBudget(&stuck, 50*time.Millisecond) {
		t.Error("a stuck WaitGroup must return false")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("waited %v for a 50ms budget", el)
	}
}

// TestShutdownAuditPool_BoundedWhenWorkerStuck: ShutdownAuditPool used to be
// auditWg.Wait() with no deadline. Simulate a worker that never finishes (a
// flush against an unreachable database) and assert shutdown returns within
// the budget instead of hanging until fx's StopTimeout SIGKILLs the process.
//
// Drives the package globals directly (InitAuditPool is once-only and a stuck
// real worker would poison every later test); everything is restored.
func TestShutdownAuditPool_BoundedWhenWorkerStuck(t *testing.T) {
	prevCh, prevBudget := auditCh, auditShutdownBudget
	prevClosed := auditClosed.Load()
	t.Cleanup(func() {
		auditCh, auditShutdownBudget = prevCh, prevBudget
		auditClosed.Store(prevClosed)
	})
	auditCh = make(chan *storageif.AuditEvent)
	auditShutdownBudget = 100 * time.Millisecond
	auditWg.Add(1) // the stuck worker
	defer auditWg.Done()

	done := make(chan struct{})
	go func() {
		ShutdownAuditPool()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ShutdownAuditPool hung on a stuck worker; it must give up after auditShutdownBudget")
	}
}
