package service

import "pad-core/models"

// SettingsProvider abstracts read/write access to persisted app settings and
// recent-file tracking. It is satisfied by *storage.SettingsStore in production
// and can be mocked in tests without dragging in filesystem dependencies.
type SettingsProvider interface {
	Get() *models.AppSettings
	Update(models.AppSettings) error
	AddRecentFile(path string, size int64) error
	RemoveRecentFile(path string) error
	ClearRecentFiles() error
}

// SecretStore abstracts API-key persistence. The default implementation is the
// OS keychain (desktop mode); cloud deployments use an encrypted, database-backed
// store. Defining the interface here (where it's consumed) follows the Go
// "accept interfaces, return structs" principle and removes the service layer's
// dependency on the storage package's global-state wrappers.
type SecretStore interface {
	Save(scope, provider, key string) error
	Get(scope, provider string) (string, error)
	Has(scope, provider string) (bool, error)
	Delete(scope, provider string) error
}
