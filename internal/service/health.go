package service

import (
	"context"
	"sync"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// blobCheckCacheTTL is how long a check reuses the last SUCCESSFUL blob reachability result (each fresh check is
// a network call). Failures are never cached: replaying one would count it N times.
const blobCheckCacheTTL = 15 * time.Second

// blobHealthChecker is implemented by storage backends that offload content to blob storage.
type blobHealthChecker interface {
	CheckBlob(ctx context.Context) error
}

// HealthService probes the storage dependencies for the readiness and admin health endpoints. Without a server
// store (desktop) HasStore is false; the methods are safe on a nil service.
type HealthService struct {
	store storageif.HealthChecker
	blob  blobHealthChecker

	blobCheckMu   sync.Mutex
	blobCheckedAt time.Time // zero when the last check failed (never cache failures)
}

func NewHealthService(backend storageif.StorageBackend) *HealthService {
	s := &HealthService{}
	if backend != nil {
		s.store = backend
		s.blob, _ = backend.(blobHealthChecker)
	}
	return s
}

// HasStore reports whether there is a storage backend to probe.
func (s *HealthService) HasStore() bool { return s != nil && s.store != nil }

// PingStore checks the storage backend is reachable.
func (s *HealthService) PingStore(ctx context.Context) error { return s.store.Ping(ctx) }

// HasBlob reports whether the backend offloads content to blob storage.
func (s *HealthService) HasBlob() bool { return s != nil && s.blob != nil }

// CheckBlob returns the blob reachability status, reusing a successful result for blobCheckCacheTTL (failures
// never cached; the mutex collapses concurrent probes into one).
func (s *HealthService) CheckBlob(ctx context.Context) error {
	s.blobCheckMu.Lock()
	defer s.blobCheckMu.Unlock()
	if !s.blobCheckedAt.IsZero() && time.Since(s.blobCheckedAt) < blobCheckCacheTTL {
		return nil
	}
	if err := s.blob.CheckBlob(ctx); err != nil {
		s.blobCheckedAt = time.Time{}
		return err
	}
	s.blobCheckedAt = time.Now()
	return nil
}
