package database

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Secret columns outside the keystore (org notification channels: the webhook URL, which for Slack and Teams IS
// the credential, the HMAC signing secret and the kinds' other secret fields, Jira's API token) are encrypted at
// rest with the keystore's key, so a database dump or backup no longer hands them out. Values are "enc:v1:" + base64(nonce || AES-256-GCM ciphertext), with the
// table, column and row id as associated data: a ciphertext copied to another row or column does not decrypt.

const sealedPrefix = "enc:v1:"

// ErrFieldKeyMissing is returned when an encrypted column is read by a process without the key.
var ErrFieldKeyMissing = errors.New("encrypted column but no encryption key configured (set PAD_ENCRYPTION_KEY)")

type fieldSealer struct{ keys *sealKeys }

// EnableFieldEncryption turns on at-rest encryption of secret columns, with the AES-256-GCM key derived from secret
// exactly as the encrypted keystore derives its key. Values sealed with one of the previous keys (a rotation, newest
// first) stay readable until ResealFields moves them. Call once at startup, before serving; without it new values
// are stored as given.
func (b *PostgresStorageBackend) EnableFieldEncryption(secret []byte, previous ...[]byte) error {
	keys, err := newSealKeys(secret, previous)
	if err != nil {
		return err
	}
	b.sealer.Store(&fieldSealer{keys: keys})
	return nil
}

func fieldAAD(table, column, id string) []byte {
	return []byte(table + "\x00" + column + "\x00" + id)
}

// sealField encrypts plaintext for (table, column, id) with the current key. "" stays "" (no secret), and without a
// sealer the value is stored as given.
func (b *PostgresStorageBackend) sealField(table, column, id, plaintext string) (string, error) {
	s := b.sealer.Load()
	if s == nil || plaintext == "" {
		return plaintext, nil
	}
	sealed, err := s.keys.seal([]byte(plaintext), fieldAAD(table, column, id))
	if err != nil {
		return "", fmt.Errorf("seal %s.%s: %w", table, column, err)
	}
	return sealedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// openField reverses sealField, with the current key or a previous one. A value without the prefix was written
// before encryption was enabled (or without a key) and is returned as is; the backfill seals such rows.
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
	plain, _, err := s.keys.open(raw, fieldAAD(table, column, id))
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
		url, secret, secrets := r.url, r.secret, r.secrets
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
		if !strings.HasPrefix(secrets, sealedPrefix) {
			if secrets, err = b.sealField("org_channels", "secrets", r.id, secrets); err != nil {
				return sealedRows, err
			}
		}
		res, err := b.db.ExecContext(ctx,
			`UPDATE org_channels SET url = $2, secret = $3, secrets = $4
			 WHERE id = $1 AND url = $5 AND secret = $6 AND secrets = $7`,
			r.id, url, secret, secrets, r.url, r.secret, r.secrets)
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
type plainChannelRow struct{ id, url, secret, secrets string }

// plaintextOrgChannels lists the rows EncryptOrgChannelsAtRest has to seal; the result set is closed before the
// rows are rewritten.
func (b *PostgresStorageBackend) plaintextOrgChannels(ctx context.Context) ([]plainChannelRow, error) {
	// An empty value is never sealed (sealField keeps ""), so a row with an empty url must not match: it would be
	// rewritten on every boot (B11).
	rows, err := b.db.QueryContext(ctx, `SELECT id, url, secret, secrets FROM org_channels
		WHERE (url <> '' AND url NOT LIKE 'enc:v1:%') OR (secret <> '' AND secret NOT LIKE 'enc:v1:%')
		   OR (secrets <> '' AND secrets NOT LIKE 'enc:v1:%')`)
	if err != nil {
		return nil, fmt.Errorf("list plaintext channels: %w", err)
	}
	defer rows.Close()
	var out []plainChannelRow
	for rows.Next() {
		var r plainChannelRow
		if err := rows.Scan(&r.id, &r.url, &r.secret, &r.secrets); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
