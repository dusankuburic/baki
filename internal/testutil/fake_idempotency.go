package testutil

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// FakeBackend's IdempotencyStore keeps its records the way Postgres keeps them in the request transaction (ADR
// 0007): a claim and its response belong to the transaction that wrote them until it commits, and vanish when it
// rolls back; another transaction finds the key in progress meanwhile. The transaction is the one WithRequestTx put
// on ctx, standing in for the RLS middleware's. Without one the store answers ErrIdempotencyUnavailable, as Postgres
// does outside the request transaction, so a router over the fake ignores Idempotency-Key like a desktop.

var _ interfaces.IdempotencyStore = (*FakeBackend)(nil)

type fakeRequestTxKey struct{}

// fakeRequestTx is one request transaction of the fake.
type fakeRequestTx struct{ ended bool }

type fakeIdemKey struct{ user, key string }

type fakeIdemRow struct {
	fingerprint string
	resp        interfaces.IdempotentResponse
	expires     time.Time
	tx          *fakeRequestTx // the transaction that wrote the row, until it ends; nil once committed
	prev        *fakeIdemRow   // the committed row it took over, put back on a rollback
}

type fakeIdempotency struct {
	mu   sync.Mutex
	rows map[fakeIdemKey]*fakeIdemRow
}

// WithRequestTx returns ctx carrying a request transaction for the fake's transactional records (idempotency keys),
// as the RLS middleware puts one on a request. end commits it (true) or rolls it back (false); later calls do
// nothing.
func (m *FakeBackend) WithRequestTx(ctx context.Context) (context.Context, func(commit bool)) {
	tx := &fakeRequestTx{}
	return context.WithValue(ctx, fakeRequestTxKey{}, tx), func(commit bool) { m.idem.end(tx, commit) }
}

// ExpireIdempotencyKey ages the committed record of userID's key past its retention.
func (m *FakeBackend) ExpireIdempotencyKey(userID, key string) {
	m.idem.mu.Lock()
	defer m.idem.mu.Unlock()
	if row := m.idem.rows[fakeIdemKey{userID, key}]; row != nil {
		row.expires = time.Now().Add(-time.Second)
	}
}

func fakeTxOf(ctx context.Context) *fakeRequestTx {
	tx, _ := ctx.Value(fakeRequestTxKey{}).(*fakeRequestTx)
	return tx
}

var errFakeTxEnded = errors.New("fake request transaction already ended")

func (m *FakeBackend) ClaimIdempotencyKey(ctx context.Context, userID, key, fingerprint string) (*interfaces.IdempotentResponse, error) {
	tx := fakeTxOf(ctx)
	if tx == nil {
		return nil, interfaces.ErrIdempotencyUnavailable
	}
	s := &m.idem
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx.ended {
		return nil, errFakeTxEnded
	}
	k := fakeIdemKey{userID, key}
	row := s.rows[k]
	if row != nil && row.tx != nil && row.tx != tx {
		return nil, interfaces.ErrIdempotencyKeyInProgress
	}
	if row == nil || row.resp.Status == 0 || !time.Now().Before(row.expires) {
		prev := row
		if row != nil && row.tx == tx {
			prev = row.prev
		}
		if s.rows == nil {
			s.rows = map[fakeIdemKey]*fakeIdemRow{}
		}
		s.rows[k] = &fakeIdemRow{fingerprint: fingerprint, expires: time.Now().Add(interfaces.IdempotencyKeyRetention), tx: tx, prev: prev}
		return nil, nil
	}
	if row.fingerprint != fingerprint {
		return nil, interfaces.ErrIdempotencyKeyReused
	}
	resp := copyIdempotentResponse(row.resp)
	return &resp, nil
}

func (m *FakeBackend) CompleteIdempotencyKey(ctx context.Context, userID, key string, resp interfaces.IdempotentResponse) error {
	tx := fakeTxOf(ctx)
	if tx == nil {
		return interfaces.ErrIdempotencyUnavailable
	}
	s := &m.idem
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx.ended {
		return errFakeTxEnded
	}
	row := s.rows[fakeIdemKey{userID, key}]
	if row == nil || row.tx != tx || row.resp.Status != 0 {
		return interfaces.ErrNotFound
	}
	row.resp = copyIdempotentResponse(resp)
	return nil
}

func (s *fakeIdempotency) end(tx *fakeRequestTx, commit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx.ended {
		return
	}
	tx.ended = true
	for k, row := range s.rows {
		switch {
		case row.tx != tx:
		case commit:
			row.tx, row.prev = nil, nil
		case row.prev != nil:
			s.rows[k] = row.prev
		default:
			delete(s.rows, k)
		}
	}
}

// purge removes the committed records past their expiry, as the retention sweep does.
func (s *fakeIdempotency) purge() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, row := range s.rows {
		if row.tx == nil && !time.Now().Before(row.expires) {
			delete(s.rows, k)
			n++
		}
	}
	return n
}

// deleteUser removes userID's records, as the foreign key's cascade does on account erasure.
func (s *fakeIdempotency) deleteUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.rows {
		if k.user == userID {
			delete(s.rows, k)
		}
	}
}

func copyIdempotentResponse(r interfaces.IdempotentResponse) interfaces.IdempotentResponse {
	r.Body = bytes.Clone(r.Body)
	return r
}
