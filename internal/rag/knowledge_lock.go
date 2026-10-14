package rag

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"time"

	"pad-core/logger"
)

// ErrKnowledgeLockUnavailable: the cross-replica lock of the org's knowledge base could not be asked for (the
// database did not answer), so the upload or re-index was not started rather than run unprotected (HTTP 503).
var ErrKnowledgeLockUnavailable = errors.New("the knowledge base lock is unavailable — try again in a moment")

// errKnowledgeLockLost is the cancellation cause of an operation whose lease was lost; the caller sees
// ErrKnowledgeBusy, because another replica may hold the org now (lostOr).
var errKnowledgeLockLost = errors.New("knowledge base lock lost")

// knowledgeLockSpace is the high 32 bits of every lease key of an org ("know"). It must stay non-zero and not
// 0xFFFFFFFF: the chunk-write transactions take pg_advisory_xact_lock(hashtext(org_id)), an int4 promoted to the
// bigint keyspace, whose high word is 0 or -1. A lease on one of those keys would be held on one session while the
// operation's own write transaction waited for it on another, and the upload would deadlock against itself until
// the statement timeout. The job locks (lifecycle) use other high words ("baki", or a 64-bit hash).
const knowledgeLockSpace = 0x6b6e6f77

// leaseAcquireTimeout bounds asking for a lease: TryLease waits for a pooled connection before it asks the database, so
// with the pool exhausted an unbounded ask would hold the request until its own deadline. A var for tests.
var leaseAcquireTimeout = 5 * time.Second

// DefaultLeaseCap is how many org leases a replica holds at once when none is configured (WithLeaseCap).
const DefaultLeaseCap = 4

// KnowledgeLockKey is the cross-replica lease key of an org's knowledge base: one lease serialises the org's
// uploads and re-indexes over every replica.
func KnowledgeLockKey(orgID string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("knowledge_org:" + orgID))
	return int64(knowledgeLockSpace)<<32 | int64(h.Sum32())
}

// WithLeaseCap bounds how many org leases this replica holds at once (PAD_KNOWLEDGE_LEASE_CAP): a lease pins a
// database connection for the whole operation, which lasts seconds to minutes, so a burst of orgs must not pin the
// pool. The operation beyond the cap is told busy. n <= 0 means DefaultLeaseCap. Without WithJobLeaser it does nothing.
func WithLeaseCap(n int) Option {
	return func(s *KnowledgeService) { s.leaseCap = n }
}

// lockOrg serialises corpus mutations of one org: an upload landing mid-re-index would carry the pre-switch model
// and be missed by the re-index, and two uploads would both pay to embed. Two levels, both non-blocking (the
// second admin gets ErrKnowledgeBusy; Search never takes them):
//
//   - a mutex in this process, the cheap answer that uses no pool connection;
//   - when the deployment has a lock.Leaser, a lease on KnowledgeLockKey(orgID) over every replica, held for the
//     whole operation (check, embed, write; or the re-index's page loop). The returned context is cancelled when
//     the lease is lost, so an operation whose lock another replica may have taken stops. This replica holds at
//     most leaseCap leases (each pins a connection); the operation beyond it is busy.
//
// The chunk-write transactions take their own short transaction lock beside it (lockKnowledgeOrgs), which
// protects a single write when the lease is lost mid-way. release is idempotent.
func (s *KnowledgeService) lockOrg(ctx context.Context, orgID string) (opCtx context.Context, release func(), err error) {
	v, _ := s.orgLocks.LoadOrStore(orgID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return ctx, nil, ErrKnowledgeBusy
	}
	if s.jobLeaser == nil {
		return ctx, mu.Unlock, nil
	}
	select {
	case s.leaseSlots <- struct{}{}:
	default:
		mu.Unlock()
		return ctx, nil, ErrKnowledgeBusy
	}
	actx, acancel := context.WithTimeout(ctx, leaseAcquireTimeout)
	lease, acquired, err := s.jobLeaser.TryLease(actx, KnowledgeLockKey(orgID))
	acancel()
	if err != nil || !acquired {
		<-s.leaseSlots
		mu.Unlock()
		switch {
		case err == nil:
			return ctx, nil, ErrKnowledgeBusy
		case ctx.Err() != nil:
			return ctx, nil, ctx.Err()
		}
		logger.Warn("knowledge base lock could not be taken", "org_id", orgID, "error", err)
		return ctx, nil, ErrKnowledgeLockUnavailable
	}

	opCtx, cancel := context.WithCancelCause(ctx)
	stop := make(chan struct{})
	go func() {
		select {
		case <-lease.Lost():
			logger.Warn("knowledge base lock lost; stopping the operation", "org_id", orgID)
			cancel(errKnowledgeLockLost)
		case <-stop:
		}
	}()
	var once sync.Once
	return opCtx, func() {
		once.Do(func() {
			close(stop)
			cancel(nil)
			lease.Release()
			<-s.leaseSlots
			mu.Unlock()
		})
	}, nil
}

// lockLost reports whether ctx, an operation's context, was cancelled because its lease was lost.
func lockLost(ctx context.Context) bool { return errors.Is(context.Cause(ctx), errKnowledgeLockLost) }

// lostOr is err, or ErrKnowledgeBusy when the operation stopped because its lease was lost: the org is being
// updated elsewhere, and what the cancelled call reported (a context error wrapped in an embedding failure) is
// not the cause.
func lostOr(ctx context.Context, err error) error {
	if err != nil && lockLost(ctx) {
		return ErrKnowledgeBusy
	}
	return err
}
