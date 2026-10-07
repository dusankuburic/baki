package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/testutil"
)

// blobStore is a store that offloads content to blob storage, counting reachability checks.
type blobStore struct {
	*testutil.FakeBackend
	blobErr error
	checks  int
}

func (b *blobStore) CheckBlob(context.Context) error {
	b.checks++
	return b.blobErr
}

func TestHealthService_NoStore(t *testing.T) {
	var nilSvc *HealthService
	if nilSvc.HasStore() || nilSvc.HasBlob() {
		t.Error("a nil service has nothing to probe")
	}
	if svc := NewHealthService(nil); svc.HasStore() || svc.HasBlob() {
		t.Error("no backend: nothing to probe")
	}
}

func TestHealthService_PingStore(t *testing.T) {
	backend := testutil.NewFakeBackend()
	svc := NewHealthService(backend)
	if !svc.HasStore() || svc.HasBlob() {
		t.Fatalf("HasStore=%v HasBlob=%v, want a store without blob", svc.HasStore(), svc.HasBlob())
	}
	if err := svc.PingStore(context.Background()); err != nil {
		t.Errorf("healthy ping: %v", err)
	}
	backend.PingErr = errors.New("down")
	if err := svc.PingStore(context.Background()); err == nil {
		t.Error("want the ping error")
	}
}

// A successful blob check is reused; a failure is never cached.
func TestHealthService_CheckBlobCachesSuccessOnly(t *testing.T) {
	store := &blobStore{FakeBackend: testutil.NewFakeBackend(), blobErr: errors.New("auth")}
	svc := NewHealthService(store)
	if !svc.HasBlob() {
		t.Fatal("a blob-offloading store must be probed")
	}
	ctx := context.Background()
	_ = svc.CheckBlob(ctx)
	_ = svc.CheckBlob(ctx)
	if store.checks != 2 {
		t.Errorf("failures re-checked %d times, want 2", store.checks)
	}
	store.blobErr = nil
	_ = svc.CheckBlob(ctx)
	if err := svc.CheckBlob(ctx); err != nil || store.checks != 3 {
		t.Errorf("cached success: err=%v checks=%d, want nil and 3", err, store.checks)
	}
}
