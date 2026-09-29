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
//
// Lender tracking (migration v24): a row records both the admin who last
// saved the document (updated_by) and the admin who supplied its stored
// secret (secret_set_by) — the credential's lender. An edit that keeps the
// stored secret (a deployment name changed, the secret omitted) moves
// updated_by only, so when the lender leaves the admin role the credential
// is still found and disabled, and members are shown the right lender.
type OrgSecretStore interface {
	// SaveOrg upserts the credential with a newly supplied secret: updatedBy
	// is recorded as both the last writer and the secret's lender.
	SaveOrg(orgID, provider, key, updatedBy string) error
	// GetOrg returns the credential, storage.ErrSecretNotFound when absent.
	GetOrg(orgID, provider string) (string, error)
	HasOrg(orgID, provider string) (bool, error)
	DeleteOrg(orgID, provider string) error
	// OrgKeyMeta reports who last wrote the credential, who supplied its
	// secret and when; ok is false when the org has none.
	OrgKeyMeta(orgID, provider string) (info OrgKeyInfo, ok bool, err error)
	// SaveOrgIfUnchanged is SaveOrg with compare-and-swap on the stored
	// updated_at (a zero expected = only when the org has no row); ok is
	// false, nothing written, when the row changed or appeared meanwhile.
	// secretCarried says the document keeps the stored secret (the save
	// omitted it): the secret's lender then stays as it was; otherwise
	// updatedBy supplied the secret and becomes its lender.
	SaveOrgIfUnchanged(orgID, provider, key, updatedBy string, secretCarried bool, expected time.Time) (updatedAt time.Time, ok bool, err error)
	// DeleteOrgIfSavedBy deletes the credential only while userID is still
	// its secret's lender (secret_set_by) or its last writer (updated_by) —
	// disabling a departed admin's credential without touching a newer save
	// by another admin. userID "" deletes nothing.
	DeleteOrgIfSavedBy(orgID, provider, userID string) (deleted bool, err error)
	// OrgsWithKeySavedBy lists the orgs whose credential for provider userID
	// lent (secret_set_by) or last wrote (updated_by).
	OrgsWithKeySavedBy(provider, userID string) ([]string, error)
}

// OrgKeyInfo is an organisation credential row's metadata — never the
// secret.
type OrgKeyInfo struct {
	// UpdatedBy is the admin (user id) who last saved the document.
	UpdatedBy string
	// SecretSetBy is the admin who supplied the stored secret, the
	// credential's lender; equal to UpdatedBy unless a later save carried
	// the secret over.
	SecretSetBy string
	// UpdatedAt is the last save's time (the compare-and-swap token).
	UpdatedAt time.Time
}
