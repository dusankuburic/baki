// Package lock declares the cross-replica lock that background work takes so that one replica runs it at a time
// (the scanner's sweep, the retention purge, the legacy migration, the at-rest and audit backfills). The Postgres
// backend implements it with session-level advisory locks; internal/app provides it as an fx value, nil without a
// database, where the process runs alone and needs no lock.
package lock

import (
	"context"
	"sync"
)

// GlobalLocker takes a lock every replica of the deployment shares.
type GlobalLocker interface {
	// TryGlobalLock takes key without waiting: acquired is false (and release nil) when another holder has it.
	// release must be called exactly once when acquired.
	TryGlobalLock(ctx context.Context, key int64) (release func(), acquired bool, err error)
}

// Lease is a held cross-replica lock that tells its holder when it stops holding it. A session lock lives on one
// database connection while the job's work runs on others: when that connection dies (a network blip, a failover,
// pg_terminate_backend) the database drops the lock and another replica may take it, so the holder must stop.
type Lease interface {
	// Lost is closed once the lock is known not to be held any more: a heartbeat could not confirm it, or Release
	// found the lock already gone. A lease that cannot tell returns a channel that never closes.
	Lost() <-chan struct{}
	// Release gives the lock up. Idempotent; call it once the work is done, also after Lost closed.
	Release()
}

// Leaser takes leases. The Postgres backend implements it with the same advisory locks as GlobalLocker plus a
// heartbeat on the pinned session (ADR 0023).
type Leaser interface {
	// TryLease takes key without waiting: acquired is false (and the lease nil) when another holder has it.
	TryLease(ctx context.Context, key int64) (lease Lease, acquired bool, err error)
}

// AsLeaser adapts a GlobalLocker that cannot report a lost lock (a test fake, or the database's plain lock while the
// app still hands jobs a GlobalLocker): its leases never report Lost. nil gives nil: no locker, the process runs
// alone.
func AsLeaser(l GlobalLocker) Leaser {
	if l == nil {
		return nil
	}
	return lockerLeaser{l}
}

type lockerLeaser struct{ l GlobalLocker }

func (a lockerLeaser) TryLease(ctx context.Context, key int64) (Lease, bool, error) {
	release, acquired, err := a.l.TryGlobalLock(ctx, key)
	if err != nil || !acquired {
		return nil, false, err
	}
	return &releaseLease{release: release}, true, nil
}

// releaseLease is a lease over a plain release func.
type releaseLease struct {
	release func()
	once    sync.Once
}

// Lost returns nil, a channel that is never ready: the locker cannot tell.
func (r *releaseLease) Lost() <-chan struct{} { return nil }

func (r *releaseLease) Release() { r.once.Do(r.release) }
