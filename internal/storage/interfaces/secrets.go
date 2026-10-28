package interfaces

import (
	"context"
	"strings"
	"time"
)

// OrgSecretStore persists an organisation's SHARED provider credentials: one document per (org, provider)
// every member can run on when they have not configured that provider themselves (encrypted database
// keystore in cloud mode; nil on the desktop/local mode, where there are no organisations).
//
// Trust model: org admins are untrusted (anyone can create an org), so the secret is write-only — GetOrg is
// for building a provider, never a client response. A member's own credential always wins for chat; the
// knowledge base prefers the org's so one corpus is embedded with one resource; usage is attributed to the
// org and capped by its budget. The row goes with the organisation (ON DELETE CASCADE, v23).
//
// Lender tracking (v24): a row records both the last saver (updated_by) and the secret's supplier
// (secret_set_by) — the lender. An edit that keeps the stored secret moves updated_by only, so the
// disable-on-departure path matches the lender, not the last editor.
type OrgSecretStore interface {
	// SaveOrg upserts the credential with a newly supplied secret: updatedBy is both last writer and lender.
	SaveOrg(orgID, provider, key, updatedBy string) error
	// GetOrg returns the credential, storage.ErrSecretNotFound when absent.
	GetOrg(orgID, provider string) (string, error)
	HasOrg(orgID, provider string) (bool, error)
	DeleteOrg(orgID, provider string) error
	// OrgKeyMeta reports who last wrote the credential, who supplied its secret and when; ok is false when the org has none.
	OrgKeyMeta(orgID, provider string) (info OrgKeyInfo, ok bool, err error)
	// SaveOrgIfUnchanged is SaveOrg with compare-and-swap on the stored updated_at (zero expected = only when
	// the org has no row); ok is false, nothing written, when the row changed or appeared meanwhile.
	// secretCarried keeps the stored secret's lender; otherwise updatedBy supplied the secret and becomes it.
	SaveOrgIfUnchanged(orgID, provider, key, updatedBy string, secretCarried bool, expected time.Time) (updatedAt time.Time, ok bool, err error)
	// DeleteOrgIfSavedBy deletes the credential only while userID is still its secret's lender — secret_set_by
	// (updated_by only for a row no save has stamped since the v24 backfill) — disabling a departed lender's
	// credential without touching a newer save by another admin. userID "" deletes nothing.
	DeleteOrgIfSavedBy(orgID, provider, userID string) (deleted bool, err error)
	// OrgsWithKeySavedBy lists the orgs whose credential for provider userID lent (DeleteOrgIfSavedBy's lender rule).
	OrgsWithKeySavedBy(provider, userID string) ([]string, error)
	// Key families (an Azure AI Foundry resource each) and the undo of a save, which every organisation keystore
	// offers: only the encrypted database keystore holds organisation credentials.
	OrgSecretFamilyStore
	OrgSecretRestorer
}

// OrgKeyInfo is an organisation credential row's metadata — never the secret.
type OrgKeyInfo struct {
	// UpdatedBy is the admin (user id) who last saved the document.
	UpdatedBy string
	// SecretSetBy is the admin who supplied the stored secret, the credential's lender; equal to
	// UpdatedBy unless a later save carried the secret over.
	SecretSetBy string
	// UpdatedAt is the last save's time (the compare-and-swap token).
	UpdatedAt time.Time
}

// --- Key families (several Azure AI Foundry resources) ---
//
// A provider can own several credentials: "azure-foundry" is the default resource's and "azure-foundry/<id>"
// each other resource's. Such a set is a key FAMILY: a key is in family f when it equals f or starts with
// f + "/" (InSecretFamily), so "azure-foundryX" is never in family "azure-foundry". The SQL form is
// `provider = f OR starts_with(provider, f || '/')`, never LIKE, whose _ and % would be wildcards inside f.
// The organisation keystore's family operations are part of OrgSecretStore. The personal ones (SecretFamilyLister)
// are the encrypted database keystore's alone: the OS keyring cannot enumerate its entries, so the deployment provides
// the lister as a value of its own, nil with the keyring.

// InSecretFamily reports whether provider is in key family.
func InSecretFamily(provider, family string) bool {
	return provider == family || strings.HasPrefix(provider, family+"/")
}

// SecretFamilyLister lists one owner's personal keys of a family and
// deletes one of them conditionally.
type SecretFamilyLister interface {
	// ListFamily returns scope's keys in family, ordered by provider, with each row's last save time
	// (the orphan sweep's grace period needs it).
	ListFamily(scope, family string) ([]SecretFamilyEntry, error)
	// DeleteIfUnchanged deletes scope's key provider only while its last save is still updatedAt — the time
	// ListFamily reported — so a key saved again since that read is kept (the orphan sweep and a removed
	// resource's post-commit delete decide on an earlier read). deleted is false when no such row went.
	DeleteIfUnchanged(scope, provider string, updatedAt time.Time) (deleted bool, err error)
}

// SecretFamilyEntry is one personal key of a family — never its secret.
type SecretFamilyEntry struct {
	Provider  string
	UpdatedAt time.Time
}

// SecretState is what a stored secret row holds, told without its value: the provider catalogue and listing read
// the state of every key of a scope in one keystore read (R-provider-19) where they read each key on its own.
type SecretState int

const (
	// SecretUsable is a row that opens to a non-empty value.
	SecretUsable SecretState = iota + 1
	// SecretUnreadable is a row no configured key opens (Get: storage.ErrSecretDecryptFailed).
	SecretUnreadable
	// SecretEmpty is a legacy empty row, which no request can use.
	SecretEmpty
)

// SecretStateLister reads one owner's provider keys in one read, under the caller's context (the store's own
// timeout stays the ceiling). keys names the rows wanted; a key the owner holds no row for has no entry.
type SecretStateLister interface {
	KeyStates(ctx context.Context, scope string, keys []string) (map[string]SecretState, error)
}

// OrgSecretStateLister is SecretStateLister for an organisation's shared credentials.
type OrgSecretStateLister interface {
	OrgKeyStates(ctx context.Context, orgID string, keys []string) (map[string]SecretState, error)
}

// SecretContextReader reads one provider key under the caller's context: a request that ends aborts the read.
// Get and Has are their context-free counterparts.
type SecretContextReader interface {
	GetCtx(ctx context.Context, scope, provider string) (string, error)
	HasCtx(ctx context.Context, scope, provider string) (bool, error)
}

// OrgSecretContextReader is SecretContextReader for an organisation's shared credentials.
type OrgSecretContextReader interface {
	GetOrgCtx(ctx context.Context, orgID, provider string) (string, error)
	HasOrgCtx(ctx context.Context, orgID, provider string) (bool, error)
}

// OrgSecretFamilyStore is the org keystore's family counterpart. The lender rule is OrgsWithKeySavedBy's:
// userID lent a row when it is the row's secret_set_by, or its updated_by for a row no save has stamped
// since the v24 backfill; an empty userID matches nothing.
type OrgSecretFamilyStore interface {
	// ListOrgKeyFamily returns the org's keys in family, ordered by provider, with each row's metadata.
	ListOrgKeyFamily(orgID, family string) ([]OrgKeyFamilyEntry, error)
	// OrgKeyFamilySavedBy lists every (org, key) in family that userID lent, ordered by org then provider.
	OrgKeyFamilySavedBy(family, userID string) ([]OrgKeyRef, error)
	// DeleteOrgKeyFamilyIfSavedBy deletes, in one statement, the org's keys in family that userID lent, and
	// returns each deleted key with the document it held, so the caller can forget every credential from the
	// process caches (an undecryptable row is returned with Unreadable set and no document).
	DeleteOrgKeyFamilyIfSavedBy(orgID, family, userID string) ([]DeletedOrgKey, error)
	// DeleteOrgKeyIfUnchanged deletes the org's key provider only while its updated_at is still updatedAt
	// (ListOrgKeyFamily's or OrgKeyMeta's reading) — DeleteIfUnchanged's counterpart. deleted is false when no row went.
	DeleteOrgKeyIfUnchanged(orgID, provider string, updatedAt time.Time) (deleted bool, err error)
}

// OrgSecretRestorer undoes an org credential save the caller made, for a write that spans the keystore and
// another store (the Azure registry saves a resource's credential, then writes its registry row; when that
// write fails the credential is put back as it was).
type OrgSecretRestorer interface {
	// SnapshotOrg reads the org's key row exactly as stored — the sealed document, its writer, its lender
	// and its stamp — in one read; ok is false when there is none (an undecryptable row is snapshotted as is).
	SnapshotOrg(orgID, provider string) (snap OrgKeySnapshot, ok bool, err error)
	// RestoreOrgIfUnchanged puts snap back — document, writer, lender and stamp, so the token the admin's
	// client holds is current again — or, with snap nil (no row before the save), deletes the row; either
	// only while the row still carries current, the stamp the save returned: a newer save is never undone.
	RestoreOrgIfUnchanged(orgID, provider string, current time.Time, snap *OrgKeySnapshot) (restored bool, err error)
}

// OrgKeySnapshot is an org key row as SnapshotOrg read it. Sealed is the stored (encrypted) document,
// opaque outside the keystore that sealed it and bound to its (org, provider) row: it can only be written back there.
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
	// Document is the deleted credential's plaintext; empty when Unreadable (undecryptable with the current key).
	Document   string
	Unreadable bool
}
