package interfaces

import (
	"context"
	"errors"
	"time"
)

// IdempotencyKeyRetention is how long the response of a request sent with an Idempotency-Key answers retries
// with that key; the retention sweep purges older records.
const IdempotencyKeyRetention = 24 * time.Hour

// ErrIdempotencyKeyReused: the key belongs to a completed request with another fingerprint (HTTP 422).
var ErrIdempotencyKeyReused = errors.New("idempotency key already used for a different request")

// ErrIdempotencyKeyInProgress: another transaction holds the key and has not finished (HTTP 409).
var ErrIdempotencyKeyInProgress = errors.New("a request with this idempotency key is still in progress")

// ErrIdempotencyUnavailable: ctx carries no request transaction to keep the key in, so the key cannot be honoured
// and the caller proceeds without it.
var ErrIdempotencyUnavailable = errors.New("idempotency keys need the request transaction")

// IdempotentResponse is what a completed request with an Idempotency-Key answered, replayed to its retries.
type IdempotentResponse struct {
	Status      int
	ContentType string
	Body        []byte
	// BodyOmitted: the body was over the stored cap, so only the status is kept and a replay has no body.
	BodyOmitted bool
}

// IdempotencyStore keeps the responses of requests sent with an Idempotency-Key, per user and key (ADR 0007).
// Both methods run inside the request's transaction: the claim and the stored response commit with the request's
// writes, and a rollback removes them with those writes. Optional: callers type-assert.
type IdempotencyStore interface {
	// ClaimIdempotencyKey claims key for userID's request with fingerprint, or finds the response a completed
	// request with the key stored. It returns (nil, nil) when claimed: the request runs, and stores its response
	// with CompleteIdempotencyKey before its transaction commits. A completed request with the same fingerprint
	// returns its response; one with another fingerprint ErrIdempotencyKeyReused. A key another transaction has
	// claimed and not ended is ErrIdempotencyKeyInProgress, without waiting for it. A record older than
	// IdempotencyKeyRetention no longer counts. Without a request transaction on ctx: ErrIdempotencyUnavailable.
	ClaimIdempotencyKey(ctx context.Context, userID, key, fingerprint string) (*IdempotentResponse, error)
	// CompleteIdempotencyKey stores resp (Status 200–399) on a key this transaction claimed; ErrNotFound when it
	// holds no claim on the key.
	CompleteIdempotencyKey(ctx context.Context, userID, key string, resp IdempotentResponse) error
}
