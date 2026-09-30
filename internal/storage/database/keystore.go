package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

const keystoreTimeout = 5 * time.Second

// EncryptedKeyStore persists provider API keys in PostgreSQL, encrypted at rest
// with AES-256-GCM. It implements storage.SecretStore so it can be injected as
// the active secret backend in cloud mode, where the OS keychain is absent.
//
// The encryption key should be a dedicated deployment secret (PAD_ENCRYPTION_KEY),
// separate from the JWT signing key (PAD_AUTH_SECRET), so rotating one doesn't
// affect the other. For backward compatibility the auth secret is accepted
// when no dedicated key is configured. Rotating the encryption key renders
// previously stored ciphertext undecryptable (Get reports the key as not
// found, and the provider must be re-authenticated).
//
// RLS note: provider_keys has NO row-level-security policies, deliberately. Like
// the other auth-infra tables (refresh_tokens, token_blacklist, api_tokens), it
// is keyed by user_id and every query here carries an explicit `WHERE user_id =
// $1`, so access is bounded by the query itself rather than the session var. The
// AES-GCM associated data (keyAAD) additionally binds each ciphertext to its
// (scope, provider) row, so a row-swap confused-deputy can't reuse a stolen
// ciphertext under a different identity. If RLS is ever added to provider_keys,
// these methods must switch to BeginRLS (and the background retention purge in
// postgres_retention.go would need a non-request principal).
type EncryptedKeyStore struct {
	db   *sql.DB
	aead cipher.AEAD
}

// The keystore also holds organisations' shared credentials (G7), lists
// and releases key families (several Azure AI Foundry resources, Part 9),
// and puts back an org credential a failed cross-store write replaced.
var (
	_ interfaces.OrgSecretStore       = (*EncryptedKeyStore)(nil)
	_ interfaces.SecretFamilyLister   = (*EncryptedKeyStore)(nil)
	_ interfaces.OrgSecretFamilyStore = (*EncryptedKeyStore)(nil)
	_ interfaces.OrgSecretRestorer    = (*EncryptedKeyStore)(nil)
)

// NewEncryptedKeyStore builds a database-backed encrypted keystore. The secret
// must be non-empty; the AES-256 key is its SHA-256 digest.
func (b *PostgresStorageBackend) NewEncryptedKeyStore(secret []byte) (*EncryptedKeyStore, error) {
	aead, err := newKeystoreAEAD(secret)
	if err != nil {
		return nil, err
	}
	return &EncryptedKeyStore{db: b.db, aead: aead}, nil
}

// newKeystoreAEAD derives an AES-256-GCM AEAD from a deployment secret.
func newKeystoreAEAD(secret []byte) (cipher.AEAD, error) {
	if len(secret) == 0 {
		return nil, errors.New("keystore: empty encryption secret")
	}
	sum := sha256.Sum256(secret)
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("keystore: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keystore: new gcm: %w", err)
	}
	return aead, nil
}

// keyAAD returns the AES-GCM associated data that binds a ciphertext to its
// (scope, provider) row. Authentication fails if a stored ciphertext is moved to
// a different identity, preventing a row-swap confused-deputy by anyone able to
// write the provider_keys table.
func keyAAD(scope, provider string) []byte {
	return []byte(scope + "\x00" + provider)
}

// encrypt seals plaintext with a fresh random nonce and binds it to aad,
// returning a base64(nonce||ciphertext) string suitable for text-column storage.
func (s *EncryptedKeyStore) encrypt(plaintext string, aad []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("keystore: nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), aad)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decrypt reverses encrypt; aad must match the value passed to encrypt.
func (s *EncryptedKeyStore) decrypt(encoded string, aad []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("keystore: decode: %w", err)
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("keystore: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plaintext, err := s.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return "", fmt.Errorf("keystore: open: %w", err)
	}
	return string(plaintext), nil
}

// Save upserts the encrypted key for a (scope, provider) pair. An empty scope
// is the legacy/local namespace (stored as user_id = ”).
func (s *EncryptedKeyStore) Save(scope, provider, key string) error {
	ct, err := s.encrypt(key, keyAAD(scope, provider))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO provider_keys (user_id, provider, ciphertext, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (user_id, provider) DO UPDATE SET ciphertext = EXCLUDED.ciphertext, updated_at = NOW()`,
		scope, provider, ct,
	); err != nil {
		return fmt.Errorf("keystore: save: %w", err)
	}
	return nil
}

// Get returns the decrypted key, storage.ErrSecretNotFound if no row exists,
// or storage.ErrSecretDecryptFailed if a row exists but cannot be decrypted
// with the current deployment key (almost always: PAD_AUTH_SECRET was
// rotated). Distinguishing the two lets the API surface a clear "your AI
// key needs to be re-entered after secret rotation" rather than masking the
// rotation as "no key configured" and prompting the user to enter it again
// indefinitely.
func (s *EncryptedKeyStore) Get(scope, provider string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	var ct string
	err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM provider_keys WHERE user_id = $1 AND provider = $2`, scope, provider).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", storage.ErrSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keystore: get: %w", err)
	}
	plaintext, err := s.decrypt(ct, keyAAD(scope, provider))
	if err != nil {
		// Log so the operator can see that a rotation invalidated stored
		// keys; don't log scope (user id) at info level — keep it at warn
		// with provider only.
		logger.Warn("keystore: ciphertext decrypt failed (deployment key may have rotated)",
			"provider", provider, "error", err)
		return "", storage.ErrSecretDecryptFailed
	}
	return plaintext, nil
}

// Has reports whether a key row exists for the (scope, provider) pair.
func (s *EncryptedKeyStore) Has(scope, provider string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM provider_keys WHERE user_id = $1 AND provider = $2)`, scope, provider,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("keystore: has: %w", err)
	}
	return exists, nil
}

// Delete removes a stored provider key. Deleting a missing key is a no-op.
func (s *EncryptedKeyStore) Delete(scope, provider string) error {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM provider_keys WHERE user_id = $1 AND provider = $2`, scope, provider); err != nil {
		return fmt.Errorf("keystore: delete: %w", err)
	}
	return nil
}

// --- Organisation-shared credentials (org_provider_keys, migration v23) ---
//
// Trust model. An org admin is NOT a trusted principal (anyone can create an
// organisation and be its admin), so a shared credential is only ever what
// that admin chose to lend the org's members: it is written by admins,
// readable in its public (secret-free) form by members, and never returned
// to any client — the secret is write-only. A member's OWN credential always
// wins over the org's for chat (ProviderFactory.ForOrg), so an admin cannot
// route a member's turns through the admin's resource once the member has
// configured their own; usage run on the org credential is attributed to
// the org (and capped by the org's daily budget) like every org-scoped turn.
// The row goes with the organisation (ON DELETE CASCADE).
//
// Lender tracking (migration v24): updated_by is the admin who last saved
// the document, secret_set_by the admin who supplied its stored secret —
// the credential's lender. A save that carries the stored secret over
// changes updated_by only; the disable-on-departure path matches the
// lender (secret_set_by), so the lender's departure still disables a
// credential another admin has since edited — and that admin's departure
// does not delete a credential they merely borrowed and the lender still
// manages.

// orgKeyAAD is keyAAD's counterpart for the org_provider_keys table. The
// "org" prefix keeps an org ciphertext from ever authenticating under a user
// scope (or vice versa), even for an org id that equals some user id.
func orgKeyAAD(orgID, provider string) []byte {
	return []byte("org\x00" + orgID + "\x00" + provider)
}

// SaveOrg upserts the encrypted shared key for an (org, provider) pair with
// a newly supplied secret, recording updatedBy as both the admin who wrote
// it and the secret's lender.
func (s *EncryptedKeyStore) SaveOrg(orgID, provider, key, updatedBy string) error {
	ct, err := s.encrypt(key, orgKeyAAD(orgID, provider))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO org_provider_keys (org_id, provider, ciphertext, updated_by, secret_set_by, updated_at)
		 VALUES ($1, $2, $3, $4, $4, NOW())
		 ON CONFLICT (org_id, provider) DO UPDATE
		   SET ciphertext = EXCLUDED.ciphertext, updated_by = EXCLUDED.updated_by,
		       secret_set_by = EXCLUDED.secret_set_by, updated_at = NOW()`,
		orgID, provider, ct, updatedBy,
	); err != nil {
		return fmt.Errorf("keystore: save org: %w", err)
	}
	return nil
}

// SaveOrgIfUnchanged is SaveOrg with compare-and-swap on updated_at: it
// writes only when the stored row's updated_at still equals expected — or,
// with a zero expected, only when the org has no row yet — and returns the
// new updated_at. ok is false (nothing written) when another save or a
// delete got there first. An admin's edit made from a stale read therefore
// cannot silently overwrite another admin's newer configuration.
//
// secretCarried keeps the row's secret_set_by (the document still holds the
// secret its lender supplied); otherwise updatedBy supplied the secret and
// becomes the lender. A new row always records updatedBy as the lender.
func (s *EncryptedKeyStore) SaveOrgIfUnchanged(orgID, provider, key, updatedBy string, secretCarried bool, expected time.Time) (updatedAt time.Time, ok bool, err error) {
	ct, err := s.encrypt(key, orgKeyAAD(orgID, provider))
	if err != nil {
		return time.Time{}, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	if expected.IsZero() {
		err = s.db.QueryRowContext(ctx,
			`INSERT INTO org_provider_keys (org_id, provider, ciphertext, updated_by, secret_set_by, updated_at)
			 VALUES ($1, $2, $3, $4, $4, NOW())
			 ON CONFLICT (org_id, provider) DO NOTHING
			 RETURNING updated_at`,
			orgID, provider, ct, updatedBy,
		).Scan(&updatedAt)
	} else {
		err = s.db.QueryRowContext(ctx,
			`UPDATE org_provider_keys
			    SET ciphertext = $3, updated_by = $4,
			        secret_set_by = CASE WHEN $6::boolean THEN secret_set_by ELSE $4 END,
			        updated_at = NOW()
			  WHERE org_id = $1 AND provider = $2 AND updated_at = $5
			 RETURNING updated_at`,
			orgID, provider, ct, updatedBy, expected, secretCarried,
		).Scan(&updatedAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("keystore: save org: %w", err)
	}
	return updatedAt, true, nil
}

// DeleteOrgIfSavedBy removes the org's shared key only while userID is still
// the admin who supplied its secret — the lender, secret_set_by (updated_by
// only for a row no save has stamped since the v24 backfill) — atomically,
// so a newer save by another admin with their own secret is never deleted
// in its place, and an edit that carried the stored secret does not make
// its editor a lender. deleted is false when there was no such row; an
// empty userID matches nothing.
func (s *EncryptedKeyStore) DeleteOrgIfSavedBy(orgID, provider, userID string) (deleted bool, err error) {
	if userID == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM org_provider_keys
		  WHERE org_id = $1 AND provider = $2 AND (secret_set_by = $3 OR (secret_set_by = '' AND updated_by = $3))`, orgID, provider, userID)
	if err != nil {
		return false, fmt.Errorf("keystore: delete org: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("keystore: delete org: %w", err)
	}
	return n > 0, nil
}

// OrgsWithKeySavedBy lists the organisations whose shared key for provider
// userID lent (secret_set_by; updated_by only for rows no save has
// stamped since the v24 backfill) — account erasure disables them. An
// empty userID matches nothing.
func (s *EncryptedKeyStore) OrgsWithKeySavedBy(provider, userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id FROM org_provider_keys
		  WHERE provider = $1 AND (secret_set_by = $2 OR (secret_set_by = '' AND updated_by = $2))
		  ORDER BY org_id`, provider, userID)
	if err != nil {
		return nil, fmt.Errorf("keystore: org keys by updater: %w", err)
	}
	defer rows.Close()
	var orgIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("keystore: org keys by updater: %w", err)
		}
		orgIDs = append(orgIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keystore: org keys by updater: %w", err)
	}
	return orgIDs, nil
}

// GetOrg returns the decrypted shared key, storage.ErrSecretNotFound when
// the org has none, or storage.ErrSecretDecryptFailed when the row cannot be
// decrypted with the current deployment key (see Get).
func (s *EncryptedKeyStore) GetOrg(orgID, provider string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	var ct string
	err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM org_provider_keys WHERE org_id = $1 AND provider = $2`, orgID, provider).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", storage.ErrSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keystore: get org: %w", err)
	}
	plaintext, err := s.decrypt(ct, orgKeyAAD(orgID, provider))
	if err != nil {
		logger.Warn("keystore: org ciphertext decrypt failed (deployment key may have rotated)",
			"provider", provider, "error", err)
		return "", storage.ErrSecretDecryptFailed
	}
	return plaintext, nil
}

// HasOrg reports whether the org has a shared key row for the provider.
func (s *EncryptedKeyStore) HasOrg(orgID, provider string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM org_provider_keys WHERE org_id = $1 AND provider = $2)`, orgID, provider,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("keystore: has org: %w", err)
	}
	return exists, nil
}

// DeleteOrg removes the org's shared key. Deleting a missing key is a no-op.
func (s *EncryptedKeyStore) DeleteOrg(orgID, provider string) error {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM org_provider_keys WHERE org_id = $1 AND provider = $2`, orgID, provider); err != nil {
		return fmt.Errorf("keystore: delete org: %w", err)
	}
	return nil
}

// OrgKeyMeta reports who last wrote the org's shared key, who supplied its
// secret and when, without touching the ciphertext; ok is false when the org
// has no row.
func (s *EncryptedKeyStore) OrgKeyMeta(orgID, provider string) (info interfaces.OrgKeyInfo, ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	err = s.db.QueryRowContext(ctx,
		`SELECT updated_by, secret_set_by, updated_at FROM org_provider_keys WHERE org_id = $1 AND provider = $2`, orgID, provider,
	).Scan(&info.UpdatedBy, &info.SecretSetBy, &info.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return interfaces.OrgKeyInfo{}, false, nil
	}
	if err != nil {
		return interfaces.OrgKeyInfo{}, false, fmt.Errorf("keystore: org key meta: %w", err)
	}
	return info, true, nil
}

// --- Key families (Part 9) ---
//
// A family is a provider's set of credentials: "azure-foundry" (the default
// resource) plus "azure-foundry/<id>" (every other resource). Membership is
// `provider = $f OR starts_with(provider, $f || '/')` — starts_with, not
// LIKE: "_" is a LIKE wildcard, so LIKE 'azure-foundry/%' would read a
// family name containing one as a pattern, and a forgotten '/' would let
// "azure-foundryX" in. Every query stays bounded by its owner column, like
// the rest of this file (the tables carry no RLS).

// validFamily refuses a family name no provider has: the empty name, or one
// that is itself a family member ("a/b").
func validFamily(family string) error {
	if family == "" || strings.Contains(family, "/") {
		return fmt.Errorf("keystore: invalid key family %q", family)
	}
	return nil
}

// ListFamily returns scope's keys in family, ordered by provider (byte
// order, whatever the database collation), with each row's last save time —
// the orphan sweep only removes keys older than its grace period, so an
// in-flight create's secret is never swept.
func (s *EncryptedKeyStore) ListFamily(scope, family string) ([]interfaces.SecretFamilyEntry, error) {
	if err := validFamily(family); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider, updated_at FROM provider_keys
		  WHERE user_id = $1 AND (provider = $2 OR starts_with(provider, $2 || '/'))
		  ORDER BY provider COLLATE "C"`, scope, family)
	if err != nil {
		return nil, fmt.Errorf("keystore: list family: %w", err)
	}
	defer rows.Close()
	out := []interfaces.SecretFamilyEntry{}
	for rows.Next() {
		var e interfaces.SecretFamilyEntry
		if err := rows.Scan(&e.Provider, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("keystore: list family: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keystore: list family: %w", err)
	}
	return out, nil
}

// ListOrgKeyFamily returns the org's keys in family, ordered by provider,
// with each row's writer, lender and last save time.
func (s *EncryptedKeyStore) ListOrgKeyFamily(orgID, family string) ([]interfaces.OrgKeyFamilyEntry, error) {
	if err := validFamily(family); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider, updated_by, secret_set_by, updated_at FROM org_provider_keys
		  WHERE org_id = $1 AND (provider = $2 OR starts_with(provider, $2 || '/'))
		  ORDER BY provider COLLATE "C"`, orgID, family)
	if err != nil {
		return nil, fmt.Errorf("keystore: list org family: %w", err)
	}
	defer rows.Close()
	out := []interfaces.OrgKeyFamilyEntry{}
	for rows.Next() {
		var e interfaces.OrgKeyFamilyEntry
		if err := rows.Scan(&e.Provider, &e.UpdatedBy, &e.SecretSetBy, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("keystore: list org family: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keystore: list org family: %w", err)
	}
	return out, nil
}

// OrgKeyFamilySavedBy lists every (org, key) in family that userID lent —
// the same lender rule as OrgsWithKeySavedBy: secret_set_by, or updated_by
// for a row no save has stamped since the v24 backfill. An empty userID
// matches nothing.
func (s *EncryptedKeyStore) OrgKeyFamilySavedBy(family, userID string) ([]interfaces.OrgKeyRef, error) {
	if err := validFamily(family); err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id, provider FROM org_provider_keys
		  WHERE (provider = $1 OR starts_with(provider, $1 || '/'))
		    AND (secret_set_by = $2 OR (secret_set_by = '' AND updated_by = $2))
		  ORDER BY org_id COLLATE "C", provider COLLATE "C"`, family, userID)
	if err != nil {
		return nil, fmt.Errorf("keystore: org family by lender: %w", err)
	}
	defer rows.Close()
	var out []interfaces.OrgKeyRef
	for rows.Next() {
		var ref interfaces.OrgKeyRef
		if err := rows.Scan(&ref.OrgID, &ref.Provider); err != nil {
			return nil, fmt.Errorf("keystore: org family by lender: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keystore: org family by lender: %w", err)
	}
	return out, nil
}

// DeleteOrgKeyFamilyIfSavedBy deletes the org's keys in family that userID
// lent (OrgKeyFamilySavedBy's rule) in ONE statement and returns each
// deleted key with its decrypted document. Reading the documents first and
// deleting afterwards would race a newer save by another admin: the caller
// could forget the old credential from its caches while the new one was the
// row actually deleted — or delete a row it never read. RETURNING hands back
// exactly the rows that went. A row that cannot be decrypted (the
// deployment key rotated) is still deleted and returned Unreadable.
func (s *EncryptedKeyStore) DeleteOrgKeyFamilyIfSavedBy(orgID, family, userID string) ([]interfaces.DeletedOrgKey, error) {
	if err := validFamily(family); err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`DELETE FROM org_provider_keys
		  WHERE org_id = $1 AND (provider = $2 OR starts_with(provider, $2 || '/'))
		    AND (secret_set_by = $3 OR (secret_set_by = '' AND updated_by = $3))
		 RETURNING provider, ciphertext`, orgID, family, userID)
	if err != nil {
		return nil, fmt.Errorf("keystore: delete org family: %w", err)
	}
	defer rows.Close()
	type deletedRow struct{ provider, ciphertext string }
	var deleted []deletedRow
	for rows.Next() {
		var r deletedRow
		if err := rows.Scan(&r.provider, &r.ciphertext); err != nil {
			return nil, fmt.Errorf("keystore: delete org family: %w", err)
		}
		deleted = append(deleted, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keystore: delete org family: %w", err)
	}
	sort.Slice(deleted, func(i, j int) bool { return deleted[i].provider < deleted[j].provider })
	out := make([]interfaces.DeletedOrgKey, 0, len(deleted))
	for _, r := range deleted {
		doc, err := s.decrypt(r.ciphertext, orgKeyAAD(orgID, r.provider))
		if err != nil {
			logger.Warn("keystore: deleted org ciphertext could not be decrypted (deployment key may have rotated)",
				"provider", r.provider, "error", err)
			out = append(out, interfaces.DeletedOrgKey{Provider: r.provider, Unreadable: true})
			continue
		}
		out = append(out, interfaces.DeletedOrgKey{Provider: r.provider, Document: doc})
	}
	return out, nil
}

// --- Conditional deletes and the org restore (Part 9) ---
//
// The Azure AI Foundry registry keeps each resource's credential here and
// its row in another store, with no transaction spanning the two. Its
// deletes decide on an earlier read (the orphan sweep lists the family, a
// removed resource's credential goes after the registry commit), so they
// delete only a row still carrying the stamp that read saw: a credential
// saved again in between survives. And a credential change whose registry
// write then fails puts the previous org row back (SnapshotOrg,
// RestoreOrgIfUnchanged) — conditionally too, never undoing a newer save.

// DeleteIfUnchanged deletes scope's key only while its updated_at is still
// updatedAt (the time ListFamily reported); deleted is false when no row
// matched — the key is gone, or it was saved again since.
func (s *EncryptedKeyStore) DeleteIfUnchanged(scope, provider string, updatedAt time.Time) (deleted bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM provider_keys WHERE user_id = $1 AND provider = $2 AND updated_at = $3`, scope, provider, updatedAt)
	if err != nil {
		return false, fmt.Errorf("keystore: delete if unchanged: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("keystore: delete if unchanged: %w", err)
	}
	return n > 0, nil
}

// DeleteOrgKeyIfUnchanged is DeleteIfUnchanged for an org key row.
func (s *EncryptedKeyStore) DeleteOrgKeyIfUnchanged(orgID, provider string, updatedAt time.Time) (deleted bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM org_provider_keys WHERE org_id = $1 AND provider = $2 AND updated_at = $3`, orgID, provider, updatedAt)
	if err != nil {
		return false, fmt.Errorf("keystore: delete org key if unchanged: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("keystore: delete org key if unchanged: %w", err)
	}
	return n > 0, nil
}

// SnapshotOrg reads an org key row exactly as stored — its ciphertext
// (bound to this row by orgKeyAAD, so it can only ever be written back
// here), writer, lender and stamp — in one statement. A row the current key
// cannot decrypt is snapshotted as it is.
func (s *EncryptedKeyStore) SnapshotOrg(orgID, provider string) (snap interfaces.OrgKeySnapshot, ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	err = s.db.QueryRowContext(ctx,
		`SELECT ciphertext, updated_by, secret_set_by, updated_at FROM org_provider_keys WHERE org_id = $1 AND provider = $2`, orgID, provider,
	).Scan(&snap.Sealed, &snap.UpdatedBy, &snap.SecretSetBy, &snap.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return interfaces.OrgKeySnapshot{}, false, nil
	}
	if err != nil {
		return interfaces.OrgKeySnapshot{}, false, fmt.Errorf("keystore: snapshot org key: %w", err)
	}
	return snap, true, nil
}

// RestoreOrgIfUnchanged writes snap back — ciphertext, updated_by,
// secret_set_by and updated_at, so the previous compare-and-swap token is
// current again — or, with snap nil, deletes the row, in both cases only
// while the row's updated_at is still current (the stamp of the save being
// undone). restored is false when a newer save got there first.
func (s *EncryptedKeyStore) RestoreOrgIfUnchanged(orgID, provider string, current time.Time, snap *interfaces.OrgKeySnapshot) (restored bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	var res sql.Result
	if snap == nil {
		res, err = s.db.ExecContext(ctx,
			`DELETE FROM org_provider_keys WHERE org_id = $1 AND provider = $2 AND updated_at = $3`, orgID, provider, current)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE org_provider_keys
			    SET ciphertext = $3, updated_by = $4, secret_set_by = $5, updated_at = $6
			  WHERE org_id = $1 AND provider = $2 AND updated_at = $7`,
			orgID, provider, snap.Sealed, snap.UpdatedBy, snap.SecretSetBy, snap.UpdatedAt, current)
	}
	if err != nil {
		return false, fmt.Errorf("keystore: restore org key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("keystore: restore org key: %w", err)
	}
	return n > 0, nil
}
