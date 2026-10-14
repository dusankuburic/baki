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

// BlobHealthChecker reports whether the blob storage that flow content is offloaded to is reachable (nil when
// none is configured). The database backend provides it; nil without a database.
type BlobHealthChecker interface {
	CheckBlob(ctx context.Context) error
}

// HealthService probes the storage dependencies for the readiness and admin health endpoints. Without a server
// store (desktop) HasStore is false; the methods are safe on a nil service.
type HealthService struct {
	store storageif.HealthChecker
	blob  BlobHealthChecker

	blobCheckMu   sync.Mutex
	blobCheckedAt time.Time // zero when the last check failed (never cache failures)
}

// NewHealthService probes backend and blob; either may be nil.
func NewHealthService(backend storageif.StorageBackend, blob BlobHealthChecker) *HealthService {
	s := &HealthService{blob: blob}
	if backend != nil {
		s.store = backend
	}
	return s
}

// HasStore reports whether there is a storage backend to probe.
func (s *HealthService) HasStore() bool { return s != nil && s.store != nil }

// PingStore checks the storage backend is reachable.
func (s *HealthService) PingStore(ctx context.Context) error { return s.store.Ping(ctx) }

// HasBlob reports whether there is a blob checker to probe.
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

// SchemaVersion is the highest schema migration the database has applied, which during a rolling deploy can be ahead
// of this binary. 0 with no error when there is no store or it has no schema to report.
func (s *HealthService) SchemaVersion(ctx context.Context) (int, error) {
	if !s.HasStore() {
		return 0, nil
	}
	return s.store.CurrentSchemaVersion(ctx)
}
