package database

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Idempotency-Key records (migration v32, ADR 0007). A key is claimed inside the request's transaction, so the
// claim and the stored response commit with the request's writes or vanish with them. Requests with one key at the
// same time are told apart by a transaction-scoped advisory lock on (user, key): the transaction holding it runs,
// and another is answered at once (ErrIdempotencyKeyInProgress). Inserting without the lock would wait on the first
// transaction's uncommitted row, a pooled connection pinned for as long as that request runs.

const idempotencyTable = "idempotency_keys"

var _ interfaces.IdempotencyStore = (*PostgresStorageBackend)(nil)

// idempotencyLockKey is the advisory-lock key of a user's key: the 64-bit FNV-1a hash. A collision only answers
// one of two unrelated requests in flight at once ErrIdempotencyKeyInProgress.
func idempotencyLockKey(userID, key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("idempotency\x00" + userID + "\x00" + key))
	return int64(h.Sum64()) //nolint:gosec // G115: the hash's bits reinterpreted as the signed advisory-lock key; wraparound is intended
}

// idempotencyRowID names a record in the sealed body's associated data.
func idempotencyRowID(userID, key string) string { return userID + "\x00" + key }

// claimIdempotencyKeySQL inserts the claim, or takes over a record that no longer answers: expired, or holding no
// response (only this transaction's own claim can be seen so; another transaction's never commits without one).
const claimIdempotencyKeySQL = `
INSERT INTO idempotency_keys AS k (user_id, idem_key, fingerprint, expires_at)
VALUES ($1, $2, $3, NOW() + $4 * INTERVAL '1 second')
ON CONFLICT (user_id, idem_key) DO UPDATE
   SET fingerprint = EXCLUDED.fingerprint, status = 0, content_type = '', body = NULL,
       created_at = NOW(), expires_at = EXCLUDED.expires_at
 WHERE k.status = 0 OR k.expires_at <= NOW()`

// ClaimIdempotencyKey implements interfaces.IdempotencyStore. It needs the request's RLS transaction: on its own
// transaction the claim would commit apart from the request's writes.
func (b *PostgresStorageBackend) ClaimIdempotencyKey(ctx context.Context, userID, key, fingerprint string) (*interfaces.IdempotentResponse, error) {
	if !hasRLSTx(ctx) {
		return nil, interfaces.ErrIdempotencyUnavailable
	}
	var out *interfaces.IdempotentResponse
	err := b.withTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		var locked bool
		if err := tx.QueryRowContext(txCtx, `SELECT pg_try_advisory_xact_lock($1)`, idempotencyLockKey(userID, key)).Scan(&locked); err != nil {
			return fmt.Errorf("idempotency key: lock: %w", err)
		}
		if !locked {
			return interfaces.ErrIdempotencyKeyInProgress
		}
		res, err := tx.ExecContext(txCtx, claimIdempotencyKeySQL, userID, key, fingerprint, int64(interfaces.IdempotencyKeyRetention/time.Second))
		if err != nil {
			return fmt.Errorf("idempotency key: claim: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		var (
			stored string
			resp   interfaces.IdempotentResponse
			body   []byte
		)
		if err := tx.QueryRowContext(txCtx,
			`SELECT fingerprint, status, content_type, body, body IS NULL FROM idempotency_keys WHERE user_id = $1 AND idem_key = $2`,
			userID, key).Scan(&stored, &resp.Status, &resp.ContentType, &body, &resp.BodyOmitted); err != nil {
			return fmt.Errorf("idempotency key: read: %w", err)
		}
		if stored != fingerprint {
			return interfaces.ErrIdempotencyKeyReused
		}
		if !resp.BodyOmitted {
			plain, err := b.openField(idempotencyTable, "body", idempotencyRowID(userID, key), string(body))
			if err != nil {
				return err
			}
			resp.Body = []byte(plain)
		}
		out = &resp
		return nil
	})
	return out, err
}

// CompleteIdempotencyKey implements interfaces.IdempotencyStore. The body is sealed like the other secret columns
// (ADR 0005): the response of a create can be a credential shown once, such as a new API token.
func (b *PostgresStorageBackend) CompleteIdempotencyKey(ctx context.Context, userID, key string, resp interfaces.IdempotentResponse) error {
	if !hasRLSTx(ctx) {
		return interfaces.ErrIdempotencyUnavailable
	}
	var body []byte // NULL: omitted
	if !resp.BodyOmitted {
		sealed, err := b.sealField(idempotencyTable, "body", idempotencyRowID(userID, key), string(resp.Body))
		if err != nil {
			return err
		}
		body = append([]byte{}, sealed...) // never nil: an empty body is not an omitted one
	}
	return b.withTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx,
			`UPDATE idempotency_keys SET status = $3, content_type = $4, body = $5 WHERE user_id = $1 AND idem_key = $2 AND status = 0`,
			userID, key, resp.Status, resp.ContentType, body)
		if err != nil {
			return fmt.Errorf("idempotency key: complete: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return interfaces.ErrNotFound
		}
		return nil
	})
}
