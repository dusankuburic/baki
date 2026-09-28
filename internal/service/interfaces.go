package service

import (
	"context"
	"time"

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

// OrgKeyStore mirrors storage/interfaces.OrgSecretStore where it is consumed:
// an organisation's shared provider credential (G7), written by org admins
// and run on by members who have no credential of their own. Nil on the
// desktop and in local mode (no organisations). The secret is write-only —
// GetOrg builds providers and merges an edit that omits the secret; it is
// never rendered to a client.
type OrgKeyStore interface {
	SaveOrg(orgID, provider, key, updatedBy string) error
	GetOrg(orgID, provider string) (string, error)
	HasOrg(orgID, provider string) (bool, error)
	DeleteOrg(orgID, provider string) error
	OrgKeyMeta(orgID, provider string) (updatedBy string, updatedAt time.Time, ok bool, err error)
}
