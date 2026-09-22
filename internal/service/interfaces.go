package service

import (
	"context"

	"pad-core/models"
)

// SettingsProvider abstracts read/write access to persisted app settings and
// recent-file tracking. It is satisfied by *storage.SettingsStore on the
// desktop and by *BackendSettingsProvider in cloud mode, and can be mocked in
// tests without dragging in filesystem dependencies.
//
// Get takes no context on purpose: it is called on hot paths (every analyze,
// every batch, every chat budget check) and the cloud implementation serves it
// from a TTL cache rather than a database round-trip. Update does take one, so
// that a cloud write joins the request's RLS transaction and is rolled back
// with it if the request later fails.
type SettingsProvider interface {
	Get() *models.AppSettings
	Update(ctx context.Context, settings models.AppSettings) error
	AddRecentFile(path string, size int64) error
	RemoveRecentFile(path string) error
	ClearRecentFiles() error
}

// KeyStore abstracts API-key persistence. The default implementation is the
// OS keychain (desktop mode); cloud deployments use an encrypted, database-backed
// store. Defining the interface here (where it's consumed) follows the Go
// "accept interfaces, return structs" principle and removes the service layer's
// dependency on the storage package's global-state wrappers.
type KeyStore interface {
	Save(scope, provider, key string) error
	Get(scope, provider string) (string, error)
	Has(scope, provider string) (bool, error)
	Delete(scope, provider string) error
}
