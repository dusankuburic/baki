package service

import (
	"context"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// SettingsProvider abstracts read/write access to persisted app settings and recent-file
// tracking (desktop *storage.SettingsStore, cloud *BackendSettingsProvider, mockable in
// tests). Get takes no context on purpose: it sits on hot paths (every analyze, batch,
// chat budget check) and the cloud implementation serves it from a TTL cache, not a DB
// round-trip; Update does take one, so a cloud write joins the request's RLS transaction
// and is rolled back with it if the request later fails.
type SettingsProvider interface {
	Get() *models.AppSettings
	Update(ctx context.Context, settings models.AppSettings) error
	AddRecentFile(path string, size int64) error
	RemoveRecentFile(path string) error
	ClearRecentFiles() error
}

// KeyStore abstracts API-key persistence: the OS keychain on desktop, an encrypted
// database-backed store in cloud. Defined here where it is consumed ("accept interfaces,
// return structs"), removing the service layer's dependency on the storage package's
// global-state wrappers.
type KeyStore interface {
	Save(scope, provider, key string) error
	Get(scope, provider string) (string, error)
	Has(scope, provider string) (bool, error)
	Delete(scope, provider string) error
}

// OrgKeyStore mirrors storage/interfaces.OrgSecretStore where it is consumed: an
// organisation's shared provider credential (G7), written by org admins and run on by
// members with no credential of their own; nil on the desktop and in local mode. The
// secret is write-only — GetOrg builds providers and merges an edit that omits the
// secret, never rendering it to a client; each row records its last writer and its
// secret's lender (see storageif.OrgSecretStore).
type OrgKeyStore interface {
	SaveOrg(orgID, provider, key, updatedBy string) error
	GetOrg(orgID, provider string) (string, error)
	HasOrg(orgID, provider string) (bool, error)
	DeleteOrg(orgID, provider string) error
	OrgKeyMeta(orgID, provider string) (info storageif.OrgKeyInfo, ok bool, err error)
	SaveOrgIfUnchanged(orgID, provider, key, updatedBy string, secretCarried bool, expected time.Time) (updatedAt time.Time, ok bool, err error)
	DeleteOrgIfSavedBy(orgID, provider, userID string) (deleted bool, err error)
	OrgsWithKeySavedBy(provider, userID string) ([]string, error)
}
