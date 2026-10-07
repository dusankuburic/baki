package testutil

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/storage/contract"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestFakeIdempotency_Contract: the fake's Idempotency-Key records answer like Postgres's, its WithRequestTx
// transactions standing in for the RLS middleware's.
func TestFakeIdempotency_Contract(t *testing.T) {
	f := NewFakeBackend()
	contract.RunIdempotencySuite(t, contract.IdempotencyHarness{
		Store: f,
		User:  func(*testing.T) string { return "fake-user-" + uuid.NewString() },
		Begin: func(_ *testing.T, _ string) (context.Context, func(bool)) {
			return f.WithRequestTx(context.Background())
		},
		Expire: func(_ *testing.T, userID, key string) { f.ExpireIdempotencyKey(userID, key) },
		Purge: func(t *testing.T) int {
			res, err := f.PurgeExpiredData(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			return res.IdempotencyKeys
		},
	})
}

// TestFakeIdempotency_DeleteUserRemovesRecords mirrors the Postgres foreign key's cascade.
func TestFakeIdempotency_DeleteUserRemovesRecords(t *testing.T) {
	f := NewFakeBackend()
	ctx, end := f.WithRequestTx(context.Background())
	if _, err := f.ClaimIdempotencyKey(ctx, "u1", "k", "fp"); err != nil {
		t.Fatal(err)
	}
	if err := f.CompleteIdempotencyKey(ctx, "u1", "k", storageif.IdempotentResponse{Status: 201}); err != nil {
		t.Fatal(err)
	}
	end(true)
	if err := f.DeleteUser(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	ctx, end = f.WithRequestTx(context.Background())
	defer end(false)
	if got, err := f.ClaimIdempotencyKey(ctx, "u1", "k", "other"); err != nil || got != nil {
		t.Errorf("claim after the account's erasure = (%+v, %v), want claimed afresh", got, err)
	}
}
