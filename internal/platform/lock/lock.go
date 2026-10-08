// Package lock declares the cross-replica lock that background work takes so that one replica runs it at a time
// (the scanner's sweep, the retention purge, the legacy migration, the at-rest and audit backfills). The Postgres
// backend implements it with session-level advisory locks; main.go provides it as an fx value, nil without a
// database, where the process runs alone and needs no lock.
package lock

import "context"

// GlobalLocker takes a lock every replica of the deployment shares.
type GlobalLocker interface {
	// TryGlobalLock takes key without waiting: acquired is false (and release nil) when another holder has it.
	// release must be called exactly once when acquired.
	TryGlobalLock(ctx context.Context, key int64) (release func(), acquired bool, err error)
}
