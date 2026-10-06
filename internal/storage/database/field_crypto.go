package database

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Secret columns outside the keystore (org notification channels: the webhook URL, which for Slack and Teams IS
// the credential, and the HMAC signing secret) are encrypted at rest with the keystore's key, so a database dump
// or backup no longer hands them out. Values are "enc:v1:" + base64(nonce || AES-256-GCM ciphertext), with the
// table, column and row id as associated data: a ciphertext copied to another row or column does not decrypt.

const sealedPrefix = "enc:v1:"

// ErrFieldKeyMissing is returned when an encrypted column is read by a process without the key.
var ErrFieldKeyMissing = errors.New("encrypted column but no encryption key configured (set PAD_ENCRYPTION_KEY)")

type fieldSealer struct{ aead cipher.AEAD }

// EnableFieldEncryption turns on at-rest encryption of secret columns, with the AES-256-GCM key derived from secret
// exactly as the encrypted keystore derives its key. Call once at startup, before serving; without it new values are
// stored as given.
func (b *PostgresStorageBackend) EnableFieldEncryption(secret []byte) error {
	aead, err := newKeystoreAEAD(secret)
	if err != nil {
		return err
	}
	b.sealer.Store(&fieldSealer{aead: aead})
	return nil
}

func fieldAAD(table, column, id string) []byte {
	return []byte(table + "\x00" + column + "\x00" + id)
}

// sealField encrypts plaintext for (table, column, id). "" stays "" (no secret), and without a sealer the value
// is stored as given.
func (b *PostgresStorageBackend) sealField(table, column, id, plaintext string) (string, error) {
	s := b.sealer.Load()
	if s == nil || plaintext == "" {
		return plaintext, nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("seal %s.%s: nonce: %w", table, column, err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), fieldAAD(table, column, id))
	return sealedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// openField reverses sealField. A value without the prefix was written before encryption was enabled (or without
// a key) and is returned as is; the backfill seals such rows.
func (b *PostgresStorageBackend) openField(table, column, id, stored string) (string, error) {
	enc, ok := strings.CutPrefix(stored, sealedPrefix)
	if !ok {
		return stored, nil
	}
	s := b.sealer.Load()
	if s == nil {
		return "", fmt.Errorf("open %s.%s of %s: %w", table, column, id, ErrFieldKeyMissing)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("open %s.%s of %s: decode: %w", table, column, id, err)
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("open %s.%s of %s: ciphertext too short", table, column, id)
	}
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], fieldAAD(table, column, id))
	if err != nil {
		return "", fmt.Errorf("open %s.%s of %s: %w", table, column, id, err)
	}
	return string(plain), nil
}

// EncryptOrgChannelsAtRest seals the channel URLs and secrets still stored in plaintext (rows written before
// encryption was enabled) and reports how many rows it sealed. Idempotent and resumable: a row is rewritten
// only while it still holds the values it was read with, so a concurrent save is never overwritten. Runs on the
// pool, outside any request's RLS scope, as the maintenance job it is.
func (b *PostgresStorageBackend) EncryptOrgChannelsAtRest(ctx context.Context) (int, error) {
	if b.sealer.Load() == nil {
		return 0, nil
	}
	pending, err := b.plaintextOrgChannels(ctx)
	if err != nil {
		return 0, err
	}
	sealedRows := 0
	for _, r := range pending {
		url, secret := r.url, r.secret
		if !strings.HasPrefix(url, sealedPrefix) {
			if url, err = b.sealField("org_channels", "url", r.id, url); err != nil {
				return sealedRows, err
			}
		}
		if !strings.HasPrefix(secret, sealedPrefix) {
			if secret, err = b.sealField("org_channels", "secret", r.id, secret); err != nil {
				return sealedRows, err
			}
		}
		res, err := b.db.ExecContext(ctx,
			`UPDATE org_channels SET url = $2, secret = $3 WHERE id = $1 AND url = $4 AND secret = $5`,
			r.id, url, secret, r.url, r.secret)
		if err != nil {
			return sealedRows, fmt.Errorf("seal channel %s: %w", r.id, err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			sealedRows++
		}
	}
	return sealedRows, nil
}

// plainChannelRow is an org channel row with at least one secret column still in plaintext.
type plainChannelRow struct{ id, url, secret string }

// plaintextOrgChannels lists the rows EncryptOrgChannelsAtRest has to seal; the result set is closed before the
// rows are rewritten.
func (b *PostgresStorageBackend) plaintextOrgChannels(ctx context.Context) ([]plainChannelRow, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, url, secret FROM org_channels
		WHERE url NOT LIKE 'enc:v1:%' OR (secret <> '' AND secret NOT LIKE 'enc:v1:%')`)
	if err != nil {
		return nil, fmt.Errorf("list plaintext channels: %w", err)
	}
	defer rows.Close()
	var out []plainChannelRow
	for rows.Next() {
		var r plainChannelRow
		if err := rows.Scan(&r.id, &r.url, &r.secret); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
