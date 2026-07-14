package padcloud

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
	"log/slog"
	"time"
)

// dbTokenStore persists the PAD-cloud OAuth token in PostgreSQL so it survives
// process restarts. Uses a dedicated table (created lazily) rather than the
// settings store so connector state doesn't mix with user-facing app settings.
//
// The access and refresh tokens are encrypted at rest with AES-256-GCM (the
// same scheme as the provider keystore). The refresh token is long-lived and
// can mint fresh access tokens for the entire Power Platform API surface, so it
// must not sit in the database as plaintext. When no encryption key is
// configured (aead == nil) the store degrades to plaintext with a warning —
// but production/cloud always supplies one.
type dbTokenStore struct {
	db   *sql.DB
	aead cipher.AEAD
}

// tokenAAD binds the ciphertext to this table/row so a value can't be lifted
// into another encrypted column and still authenticate.
var tokenAAD = []byte("padcloud_token\x00v1")

// NewDBTokenStore builds a DB-backed TokenStore. encryptionKey is used to derive
// the AES-256-GCM key that protects the stored tokens; pass nil/empty only for
// legacy/plaintext behaviour (a warning is logged). The table is created lazily
// (CREATE TABLE IF NOT EXISTS) so no migration is needed.
func NewDBTokenStore(db *sql.DB, encryptionKey []byte) TokenStore {
	s := &dbTokenStore{db: db}
	if len(encryptionKey) > 0 {
		aead, err := newTokenAEAD(encryptionKey)
		if err != nil {
			// Fall back to plaintext rather than disabling persistence entirely;
			// the operator is warned so they can supply a valid key.
			slog.Warn("padcloud: token encryption key invalid — persisting tokens in plaintext", "error", err)
		} else {
			s.aead = aead
		}
	} else {
		slog.Warn("padcloud: no encryption key configured — persisting OAuth tokens in plaintext")
	}
	return s
}

// newTokenAEAD derives an AES-256-GCM AEAD from the deployment encryption key.
func newTokenAEAD(key []byte) (cipher.AEAD, error) {
	sum := sha256.Sum256(key)
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return aead, nil
}

// encField seals a token field, returning base64(nonce||ciphertext). With no
// AEAD configured it returns the plaintext unchanged (legacy behaviour).
func (s *dbTokenStore) encField(plaintext string) (string, error) {
	if s.aead == nil {
		return plaintext, nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), tokenAAD)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decField reverses encField. With no AEAD configured it returns the stored
// value unchanged (legacy plaintext rows).
func (s *dbTokenStore) decField(encoded string) (string, error) {
	if s.aead == nil {
		return encoded, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plaintext, err := s.aead.Open(nil, nonce, ct, tokenAAD)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	return string(plaintext), nil
}

const createTokenTableSQL = `CREATE TABLE IF NOT EXISTS padcloud_token (
	id            smallint PRIMARY KEY DEFAULT 1,
	access_token  text NOT NULL,
	refresh_token text NOT NULL,
	expires_at    timestamptz NOT NULL,
	CONSTRAINT padcloud_token_singleton CHECK (id = 1)
)`

// ensureTable creates the singleton token table if it doesn't exist. Called
// before both Load and Save so a fresh deployment doesn't surface a spurious
// "relation padcloud_token does not exist" error on the startup load.
func (s *dbTokenStore) ensureTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, createTokenTableSQL)
	return err
}

func (s *dbTokenStore) LoadToken(ctx context.Context) (*StoredToken, error) {
	if err := s.ensureTable(ctx); err != nil {
		return nil, err
	}
	var accessEnc, refreshEnc string
	var exp time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT access_token, refresh_token, expires_at FROM padcloud_token WHERE id = 1`).Scan(
		&accessEnc, &refreshEnc, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	access, err := s.decField(accessEnc)
	if err != nil {
		// A decrypt failure almost always means the encryption key was rotated;
		// treat the stored token as absent so the connector re-runs the device
		// flow instead of failing startup.
		slog.Warn("padcloud: stored token could not be decrypted (encryption key may have rotated) — re-auth required", "error", err)
		return nil, nil
	}
	refresh, err := s.decField(refreshEnc)
	if err != nil {
		slog.Warn("padcloud: stored refresh token could not be decrypted (encryption key may have rotated) — re-auth required", "error", err)
		return nil, nil
	}
	return &StoredToken{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp}, nil
}

func (s *dbTokenStore) SaveToken(ctx context.Context, t *StoredToken) error {
	if err := s.ensureTable(ctx); err != nil {
		return err
	}
	accessEnc, err := s.encField(t.AccessToken)
	if err != nil {
		return err
	}
	refreshEnc, err := s.encField(t.RefreshToken)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO padcloud_token (id, access_token, refresh_token, expires_at)
		 VALUES (1, $1, $2, $3)
		 ON CONFLICT (id) DO UPDATE SET access_token = $1, refresh_token = $2, expires_at = $3`,
		accessEnc, refreshEnc, t.ExpiresAt)
	return err
}
