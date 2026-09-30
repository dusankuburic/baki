package interfaces

import (
	"strings"
	"time"
)

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
// is still found and disabled, a departing editor's edit does not delete
// the lender's credential, and members are shown the right lender.
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
	// its secret's lender — secret_set_by (updated_by only for a row no
	// save has stamped since the v24 backfill) — disabling a departed
	// lender's credential without touching a newer save by another admin or
	// deleting one an editor merely carried. userID "" deletes nothing.
	DeleteOrgIfSavedBy(orgID, provider, userID string) (deleted bool, err error)
	// OrgsWithKeySavedBy lists the orgs whose credential for provider userID
	// lent (secret_set_by; updated_by only for rows no save has stamped
	// since the v24 backfill).
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

// --- Key families (Part 9: several Azure AI Foundry resources) ---
//
// A provider can own several credentials: "azure-foundry" is the default
// resource's and "azure-foundry/<id>" each other resource's. Such a set is a
// key FAMILY: a key is in family f when it equals f or starts with f + "/"
// (InSecretFamily) — so "azure-foundryX" is never in family "azure-foundry".
// The SQL form is `provider = f OR starts_with(provider, f || '/')`, never
// LIKE, whose _ and % would be wildcards inside f.
//
// The family operations are separate, optional interfaces: the encrypted
// database keystore and the test fakes implement them, the OS keyring cannot
// enumerate its entries and does not (callers type-assert; the desktop
// registry rolls pending rows back instead of sweeping for orphans).

// InSecretFamily reports whether provider is in key family.
func InSecretFamily(provider, family string) bool {
	return provider == family || strings.HasPrefix(provider, family+"/")
}

// SecretFamilyLister lists one owner's personal keys of a family and
// deletes one of them conditionally.
type SecretFamilyLister interface {
	// ListFamily returns scope's keys in family, ordered by provider, with
	// each row's last save time (the orphan sweep's grace period needs it).
	ListFamily(scope, family string) ([]SecretFamilyEntry, error)
	// DeleteIfUnchanged deletes scope's key provider only while its last
	// save is still updatedAt — the time ListFamily reported for it — so a
	// key saved again since that read is kept: the orphan sweep and a
	// removed resource's post-commit delete decide on an earlier read.
	// deleted is false, with no error, when no such row went.
	DeleteIfUnchanged(scope, provider string, updatedAt time.Time) (deleted bool, err error)
}

// SecretFamilyEntry is one personal key of a family — never its secret.
type SecretFamilyEntry struct {
	Provider  string
	UpdatedAt time.Time
}

// OrgSecretFamilyStore is the org keystore's family counterpart. The
// lender rule is OrgsWithKeySavedBy's: userID lent a row when it is the
// row's secret_set_by, or its updated_by for a row no save has stamped
// since the v24 backfill; an empty userID matches nothing.
type OrgSecretFamilyStore interface {
	// ListOrgKeyFamily returns the org's keys in family, ordered by
	// provider, with each row's metadata.
	ListOrgKeyFamily(orgID, family string) ([]OrgKeyFamilyEntry, error)
	// OrgKeyFamilySavedBy lists every (org, key) in family that userID
	// lent, ordered by org then provider.
	OrgKeyFamilySavedBy(family, userID string) ([]OrgKeyRef, error)
	// DeleteOrgKeyFamilyIfSavedBy deletes, in one statement, the org's
	// keys in family that userID lent, and returns each deleted key with
	// the document it held, so the caller can forget every credential from
	// the process caches (a row that could not be decrypted is returned
	// with Unreadable set and no document).
	DeleteOrgKeyFamilyIfSavedBy(orgID, family, userID string) ([]DeletedOrgKey, error)
	// DeleteOrgKeyIfUnchanged deletes the org's key provider only while its
	// updated_at is still updatedAt (ListOrgKeyFamily's or OrgKeyMeta's
	// reading): a key saved again since is kept (SecretFamilyLister.
	// DeleteIfUnchanged's counterpart). deleted is false when no row went.
	DeleteOrgKeyIfUnchanged(orgID, provider string, updatedAt time.Time) (deleted bool, err error)
}

// OrgSecretRestorer undoes an org credential save the caller made, for a
// write that spans the keystore and another store (the Azure AI Foundry
// model registry saves a resource's credential, then writes its registry
// row; when that write fails the credential is put back as it was).
// Optional: callers type-assert, and without it report the credential as
// changed.
type OrgSecretRestorer interface {
	// SnapshotOrg reads the org's key row exactly as stored — the sealed
	// document, its writer, its lender and its stamp — in one read; ok is
	// false when there is none. A row the current deployment key cannot
	// decrypt is snapshotted too (its sealed document goes back as it was).
	SnapshotOrg(orgID, provider string) (snap OrgKeySnapshot, ok bool, err error)
	// RestoreOrgIfUnchanged puts snap back — document, writer, lender and
	// stamp, so the token the admin's client holds is current again — or,
	// with snap nil (there was no row before the save), deletes the row;
	// either only while the row still carries current, the stamp the save
	// returned: a newer save by anyone is never undone (restored false).
	RestoreOrgIfUnchanged(orgID, provider string, current time.Time, snap *OrgKeySnapshot) (restored bool, err error)
}

// OrgKeySnapshot is an org key row as SnapshotOrg read it. Sealed is the
// stored (encrypted) document, opaque outside the keystore that sealed it
// and bound to its (org, provider) row: it can only be written back there.
type OrgKeySnapshot struct {
	OrgKeyInfo
	Sealed string
}

// OrgKeyFamilyEntry is one org key of a family — never its secret.
type OrgKeyFamilyEntry struct {
	Provider string
	OrgKeyInfo
}

// OrgKeyRef names one org key.
type OrgKeyRef struct {
	OrgID    string
	Provider string
}

// DeletedOrgKey is one key DeleteOrgKeyFamilyIfSavedBy removed.
type DeletedOrgKey struct {
	Provider string
	// Document is the deleted credential's plaintext; empty when
	// Unreadable (it could not be decrypted with the current key).
	Document   string
	Unreadable bool
}
