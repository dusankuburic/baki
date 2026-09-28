package interfaces

import "time"

// OrgSecretStore persists an organisation's SHARED provider credentials
// (G7): one document per (org, provider) that every member can run on when
// they have not configured that provider themselves. Implemented by the
// encrypted database keystore in cloud mode; absent (nil) on the desktop and
// in local mode, where there are no organisations.
//
// Trust model: org admins are untrusted (anyone can create an org), so the
// secret is write-only — GetOrg is for building a provider, never for a
// client response; members read only a secret-free public form. A member's
// own credential always wins over the org's for chat; the knowledge base
// prefers the org's so one corpus is embedded with one resource. Usage on
// the shared credential is attributed to the org and capped by its budget.
// The row goes with the organisation (ON DELETE CASCADE, migration v23).
type OrgSecretStore interface {
	// SaveOrg upserts the credential, recording the admin (user id) who wrote it.
	SaveOrg(orgID, provider, key, updatedBy string) error
	// GetOrg returns the credential, storage.ErrSecretNotFound when absent.
	GetOrg(orgID, provider string) (string, error)
	HasOrg(orgID, provider string) (bool, error)
	DeleteOrg(orgID, provider string) error
	// OrgKeyMeta reports who last wrote the credential and when; ok is false
	// when the org has none.
	OrgKeyMeta(orgID, provider string) (updatedBy string, updatedAt time.Time, ok bool, err error)
}
