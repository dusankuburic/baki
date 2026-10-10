package app

import (
	"context"
	"sync/atomic"
	"testing"

	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
	storagedb "pad-analyzer/internal/storage/database"
)

// blockingBackfill is a field encryptor whose plaintext backfill runs until its context ends.
type blockingBackfill struct {
	fakeResealer
	started, ended atomic.Int32
}

func (b *blockingBackfill) EncryptOrgChannelsAtRest(ctx context.Context) (int, error) {
	b.started.Add(1)
	<-ctx.Done()
	b.ended.Add(1)
	return 0, ctx.Err()
}

var _ storagedb.FieldEncryptor = (*blockingBackfill)(nil)

// Stopping the app cancels the plaintext backfill: it used to run on a context of its own, so the shutdown drain of
// the background runners waited on it. Without a key or a database nothing is scheduled.
func TestScheduleFieldEncryptionBackfill_StopsWithTheApp(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.EncryptionKey = secret.New("current")
	fe, lock := &blockingBackfill{}, &resealLocker{acquired: true}
	lc := fxtest.NewLifecycle(t)
	scheduleFieldEncryptionBackfill(lc, cfg, fe, lock, nil)
	lc.RequireStart()
	waitUntil(t, "the backfill to start", func() bool { return fe.started.Load() == 1 })
	lc.RequireStop()
	waitUntil(t, "the backfill to end on the app's stop", func() bool { return fe.ended.Load() == 1 })
	waitUntil(t, "the cancelled backfill to release its lock", func() bool { return lock.released.Load() == 1 })

	noKey := fxtest.NewLifecycle(t)
	idle := &blockingBackfill{}
	scheduleFieldEncryptionBackfill(noKey, config.Default(), idle, lock, nil)
	noKey.RequireStart()
	noKey.RequireStop()
	if idle.started.Load() != 0 {
		t.Error("a backfill ran without an encryption key")
	}

	desktop := fxtest.NewLifecycle(t)
	scheduleFieldEncryptionBackfill(desktop, cfg, nil, nil, nil)
	desktop.RequireStart()
	desktop.RequireStop()
}
