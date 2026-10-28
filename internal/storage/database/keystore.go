package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// keystoreTimeout bounds every keystore statement: the whole timeout for the context-free methods, a ceiling on
// the caller's context for the ...Ctx reads (a request that ends first aborts the read).
const keystoreTimeout = 5 * time.Second

// EncryptedKeyStore persists provider API keys in PostgreSQL, encrypted at rest with AES-256-GCM; it
// implements storage.SecretStore as the cloud-mode secret backend (no OS keychain there). The encryption
// key should be a dedicated deployment secret (PAD_ENCRYPTION_KEY) separate from the JWT signing key (the
// auth secret is accepted for backward compatibility). Rotating it lists the replaced key among the previous
// keys (PAD_ENCRYPTION_KEY_PREVIOUS), which still open what they sealed until ResealKeys moves it; a key no
// longer configured renders its ciphertext undecryptable (Get reports ErrSecretDecryptFailed; the provider
// must be re-authenticated).
//
// RLS note: provider_keys deliberately has NO row-level-security policies — like the other auth-infra
// tables it is keyed by user_id and every query here carries an explicit `WHERE user_id = $1`; the AES-GCM
// associated data (keyAAD) additionally binds each ciphertext to its (scope, provider) row, so a row-swap
// confused-deputy can't reuse a stolen ciphertext under a different identity. If RLS is ever added here,
// these methods must switch to BeginRLS (and the retention purge would need a non-request principal).
type EncryptedKeyStore struct {
	db   *sql.DB
	keys *sealKeys
	// warned holds the rows whose decrypt failure was already logged: the catalogue and the listing read every
	// key on each call, so an unreadable row would otherwise log on every page load. Bounded by the rows.
	warned sync.Map
}

// warnDecryptOnce logs a row's decrypt failure the first time this process meets it (provider only, no owner id).
func (s *EncryptedKeyStore) warnDecryptOnce(msg, rowKey, provider string, err error) {
	if _, seen := s.warned.LoadOrStore(rowKey, struct{}{}); seen {
		return
	}
	logger.Warn(msg, "provider", provider, "error", err)
}

// The keystore also holds orgs' shared credentials, lists/releases key families, and restores an org
// credential a failed cross-store write replaced.
var (
	_ interfaces.OrgSecretStore       = (*EncryptedKeyStore)(nil)
	_ interfaces.SecretFamilyLister   = (*EncryptedKeyStore)(nil)
	_ interfaces.OrgSecretFamilyStore = (*EncryptedKeyStore)(nil)
	_ interfaces.OrgSecretRestorer    = (*EncryptedKeyStore)(nil)
	// The provider catalogue and listing read a scope's keys in one query, under the request's context.
	_ interfaces.SecretStateLister      = (*EncryptedKeyStore)(nil)
	_ interfaces.OrgSecretStateLister   = (*EncryptedKeyStore)(nil)
	_ interfaces.SecretContextReader    = (*EncryptedKeyStore)(nil)
	_ interfaces.OrgSecretContextReader = (*EncryptedKeyStore)(nil)
)

// NewEncryptedKeyStore builds a database-backed encrypted keystore. The secret
// must be non-empty; the AES-256 key is its SHA-256 digest. previous are the keys
// it replaced, newest first: they only open.
func (b *PostgresStorageBackend) NewEncryptedKeyStore(secret []byte, previous ...[]byte) (*EncryptedKeyStore, error) {
	keys, err := newSealKeys(secret, previous)
	if err != nil {
		return nil, err
	}
	return &EncryptedKeyStore{db: b.db, keys: keys}, nil
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

// keyAAD returns the AES-GCM associated data binding a ciphertext to its (scope, provider) row:
// authentication fails if a stored ciphertext is moved to a different identity (row-swap confused-deputy).
func keyAAD(scope, provider string) []byte {
	return []byte(scope + "\x00" + provider)
}

// encrypt seals plaintext with the current key and a fresh random nonce and binds it to aad,
// returning a base64(nonce||ciphertext) string suitable for text-column storage.
func (s *EncryptedKeyStore) encrypt(plaintext string, aad []byte) (string, error) {
	sealed, err := s.keys.seal([]byte(plaintext), aad)
	if err != nil {
		return "", fmt.Errorf("keystore: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decrypt reverses encrypt, with the current key or a previous one; aad must match the value passed to encrypt.
func (s *EncryptedKeyStore) decrypt(encoded string, aad []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("keystore: decode: %w", err)
	}
	plaintext, _, err := s.keys.open(raw, aad)
	if errors.Is(err, errCiphertextTooShort) {
		return "", fmt.Errorf("keystore: %w", err)
	}
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

// Get returns the decrypted key, storage.ErrSecretNotFound if no row exists, or storage.ErrSecretDecryptFailed
// if a row exists but cannot be decrypted with the current deployment key (almost always: rotated) — the
// distinction lets the API surface "re-enter the key after secret rotation" instead of "no key configured".
func (s *EncryptedKeyStore) Get(scope, provider string) (string, error) {
	return s.GetCtx(context.Background(), scope, provider)
}

// GetCtx is Get under ctx, keystoreTimeout its ceiling.
func (s *EncryptedKeyStore) GetCtx(ctx context.Context, scope, provider string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, keystoreTimeout)
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
		// Log so the operator can see that a rotation invalidated stored keys;
		// keep it at warn with provider only (no user id), once per row.
		s.warnDecryptOnce("keystore: ciphertext decrypt failed (deployment key may have rotated)",
			"u\x00"+scope+"\x00"+provider, provider, err)
		return "", storage.ErrSecretDecryptFailed
	}
	return plaintext, nil
}

// Has reports whether a key row exists for the (scope, provider) pair.
func (s *EncryptedKeyStore) Has(scope, provider string) (bool, error) {
	return s.HasCtx(context.Background(), scope, provider)
}

// HasCtx is Has under ctx, keystoreTimeout its ceiling.
func (s *EncryptedKeyStore) HasCtx(ctx context.Context, scope, provider string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, keystoreTimeout)
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
// Trust model. An org admin is NOT a trusted principal (anyone can create an organisation and be its
// admin), so a shared credential is only ever what that admin chose to lend the org's members: written
// by admins, readable in its secret-free form by members, never returned to any client. A member's OWN
// credential always wins over the org's for chat (ProviderFactory.ForOrg), and usage on the org credential
// is attributed to the org and capped by its budget. The row goes with the organisation (ON DELETE CASCADE).
//
// Lender tracking (v24): updated_by is the admin who last saved the document, secret_set_by the admin who
// supplied its stored secret — the lender. A save that carries the stored secret over changes updated_by
// only; the disable-on-departure path matches the lender, so the lender's departure still disables a
// credential another admin has since edited.

// orgKeyAAD is keyAAD's counterpart for org_provider_keys: the "org" prefix keeps an org ciphertext from
// ever authenticating under a user scope (or vice versa), even for an org id that equals some user id.
func orgKeyAAD(orgID, provider string) []byte {
	return []byte("org\x00" + orgID + "\x00" + provider)
}

// SaveOrg upserts the encrypted shared key for an (org, provider) pair with a newly supplied secret,
// recording updatedBy as both the writer and the secret's lender.
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

// SaveOrgIfUnchanged is SaveOrg with compare-and-swap on updated_at: writes only when the stored row's
// updated_at still equals expected (zero expected = only when the org has no row yet) and returns the new
// updated_at. ok is false when another save or delete got there first, so a stale read cannot silently
// overwrite a newer configuration. secretCarried keeps the row's secret_set_by; otherwise updatedBy is the lender.
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

// DeleteOrgIfSavedBy removes the org's shared key only while userID is still the admin who supplied its
// secret — the lender, secret_set_by (updated_by only for a row no save has stamped since the v24 backfill) —
// atomically, so a newer save by another admin with their own secret is never deleted; deleted is false when
// there was no such row.
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
// userID lent (DeleteOrgIfSavedBy's rule) — account erasure disables them.
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

// GetOrg returns the decrypted shared key, storage.ErrSecretNotFound when the org has none, or
// storage.ErrSecretDecryptFailed when the row cannot be decrypted with the current deployment key (see Get).
func (s *EncryptedKeyStore) GetOrg(orgID, provider string) (string, error) {
	return s.GetOrgCtx(context.Background(), orgID, provider)
}

// GetOrgCtx is GetOrg under ctx, keystoreTimeout its ceiling.
func (s *EncryptedKeyStore) GetOrgCtx(ctx context.Context, orgID, provider string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, keystoreTimeout)
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
		s.warnDecryptOnce("keystore: org ciphertext decrypt failed (deployment key may have rotated)",
			"o\x00"+orgID+"\x00"+provider, provider, err)
		return "", storage.ErrSecretDecryptFailed
	}
	return plaintext, nil
}

// HasOrg reports whether the org has a shared key row for the provider.
func (s *EncryptedKeyStore) HasOrg(orgID, provider string) (bool, error) {
	return s.HasOrgCtx(context.Background(), orgID, provider)
}

// HasOrgCtx is HasOrg under ctx, keystoreTimeout its ceiling.
func (s *EncryptedKeyStore) HasOrgCtx(ctx context.Context, orgID, provider string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, keystoreTimeout)
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

// OrgKeyMeta reports who last wrote the org's shared key, who supplied its secret and when, without
// touching the ciphertext; ok is false when the org has no row.
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

// --- Key states ---
//
// The provider catalogue and listing need, for every provider a deployment offers, whether the caller (and their
// organisation) holds a usable key, an unreadable one or none (R-provider-19). One query per owner answers it:
// the rows are opened here, the same way Get opens one, and only their state leaves the store.

// KeyStates reports the state of each of keys scope holds, in one query under ctx (keystoreTimeout its ceiling);
// a key with no row has no entry. An undecryptable row is SecretUnreadable and logged once, as Get logs it.
func (s *EncryptedKeyStore) KeyStates(ctx context.Context, scope string, keys []string) (map[string]interfaces.SecretState, error) {
	return s.keyStates(ctx, "keystore: key states",
		`SELECT provider, ciphertext FROM provider_keys WHERE user_id = $1 AND provider = ANY($2)`, scope, keys,
		func(provider string) ([]byte, string) {
			return keyAAD(scope, provider), "u\x00" + scope + "\x00" + provider
		},
		"keystore: ciphertext decrypt failed (deployment key may have rotated)")
}

// OrgKeyStates is KeyStates for the organisation's shared keys.
func (s *EncryptedKeyStore) OrgKeyStates(ctx context.Context, orgID string, keys []string) (map[string]interfaces.SecretState, error) {
	return s.keyStates(ctx, "keystore: org key states",
		`SELECT provider, ciphertext FROM org_provider_keys WHERE org_id = $1 AND provider = ANY($2)`, orgID, keys,
		func(provider string) ([]byte, string) {
			return orgKeyAAD(orgID, provider), "o\x00" + orgID + "\x00" + provider
		},
		"keystore: org ciphertext decrypt failed (deployment key may have rotated)")
}

// keyStates runs a key-states query for owner's keys and opens each row with the AAD row gives (and the row's
// warn-once key); what names the query in errors, warn the decrypt-failure log line.
func (s *EncryptedKeyStore) keyStates(ctx context.Context, what, query, owner string, keys []string,
	row func(provider string) (aad []byte, warnKey string), warn string) (map[string]interfaces.SecretState, error) {
	out := make(map[string]interfaces.SecretState, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, keystoreTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, query, owner, keys)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		var provider, ct string
		if err := rows.Scan(&provider, &ct); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		aad, warnKey := row(provider)
		plaintext, err := s.decrypt(ct, aad)
		switch {
		case err != nil:
			s.warnDecryptOnce(warn, warnKey, provider, err)
			out[provider] = interfaces.SecretUnreadable
		case plaintext == "":
			out[provider] = interfaces.SecretEmpty
		default:
			out[provider] = interfaces.SecretUsable
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// --- Key families ---
//
// A family is a provider's set of credentials: "azure-foundry" (the default resource) plus
// "azure-foundry/<id>" (every other resource). Membership is `provider = $f OR starts_with(provider, $f || '/')`
// — starts_with, not LIKE: "_" is a LIKE wildcard, and a forgotten '/' would let "azure-foundryX" in.
// Every query stays bounded by its owner column (the tables carry no RLS).

// validFamily refuses a family name no provider has: the empty name, or one
// that is itself a family member ("a/b").
func validFamily(family string) error {
	if family == "" || strings.Contains(family, "/") {
		return fmt.Errorf("keystore: invalid key family %q", family)
	}
	return nil
}

// ListFamily returns scope's keys in family, ordered by provider (byte order, whatever the database
// collation), with each row's last save time — the orphan sweep only removes keys older than its grace period.
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
// DeleteOrgIfSavedBy's rule. An empty userID matches nothing.
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

// DeleteOrgKeyFamilyIfSavedBy deletes the org's keys in family that userID lent (OrgKeyFamilySavedBy's rule)
// in ONE statement and returns each deleted key with its decrypted document — reading first and deleting after
// would race a newer save; RETURNING hands back exactly the rows that went. An undecryptable row (rotated
// deployment key) is still deleted and returned Unreadable.
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

// --- Conditional deletes and the org restore ---
//
// The Azure registry keeps each resource's credential here and its row in another store, with no transaction
// spanning the two. Its deletes decide on an earlier read, so they delete only a row still carrying the stamp
// that read saw: a credential saved again in between survives. A credential change whose registry write then
// fails puts the previous org row back (SnapshotOrg, RestoreOrgIfUnchanged) — conditionally too, never
// undoing a newer save.

// DeleteIfUnchanged deletes scope's key only while its updated_at is still updatedAt (the time ListFamily
// reported); deleted is false when no row matched — the key is gone, or it was saved again since.
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

// SnapshotOrg reads an org key row exactly as stored — its ciphertext (bound to this row by orgKeyAAD,
// so it can only ever be written back here), writer, lender and stamp — in one statement.
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

// RestoreOrgIfUnchanged writes snap back — ciphertext, updated_by, secret_set_by and updated_at, so the
// previous compare-and-swap token is current again — or, with snap nil, deletes the row; both only while the
// row's updated_at is still current. restored is false when a newer save got there first. A ciphertext a previous
// key sealed goes back sealed with the current key: the snapshot may predate the re-seal pass moving the row.
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
			orgID, provider, s.underCurrentKey(snap.Sealed, orgKeyAAD(orgID, provider)), snap.UpdatedBy, snap.SecretSetBy, snap.UpdatedAt, current)
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

// underCurrentKey is a stored ciphertext as the current key seals it: one a previous key sealed is sealed again,
// anything else (already current, or opened by no configured key) is returned as stored.
func (s *EncryptedKeyStore) underCurrentKey(stored string, aad []byte) string {
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return stored
	}
	plain, current, err := s.keys.open(raw, aad)
	if err != nil || current {
		return stored
	}
	sealed, err := s.keys.seal(plain, aad)
	if err != nil {
		return stored
	}
	return base64.StdEncoding.EncodeToString(sealed)
}
