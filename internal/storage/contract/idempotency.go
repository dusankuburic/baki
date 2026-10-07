package contract

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/storage/interfaces"
)

// IdempotencyHarness adapts one IdempotencyStore implementation to RunIdempotencySuite: Postgres (DB-gated) and the
// test fake run the same scenarios, so they cannot drift apart on what a claim, a replay, a rollback, a concurrent
// claim or an expired record means.
type IdempotencyHarness struct {
	Store interfaces.IdempotencyStore
	// User returns a user the store may keep keys for (Postgres: a users row, for the foreign key).
	User func(t *testing.T) string
	// Begin opens a request transaction for userID, as the RLS middleware does. end commits it (true) or rolls it
	// back (false); calls after the first do nothing.
	Begin func(t *testing.T, userID string) (ctx context.Context, end func(commit bool))
	// Expire makes the record of userID's key older than its retention.
	Expire func(t *testing.T, userID, key string)
	// Purge runs the retention sweep and returns how many records it removed.
	Purge func(t *testing.T) int
}

// RunIdempotencySuite runs the Idempotency-Key store contract.
func RunIdempotencySuite(t *testing.T, h IdempotencyHarness) {
	t.Helper()
	for _, sc := range idempotencyScenarios {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, h) })
	}
}

var idempotencyScenarios = []struct {
	name string
	run  func(t *testing.T, h IdempotencyHarness)
}{
	{"claim_complete_then_replay", idemClaimCompleteReplay},
	{"other_fingerprint_is_refused", idemOtherFingerprint},
	{"rollback_releases_the_key", idemRollbackReleases},
	{"claim_in_another_transaction_is_in_progress", idemInProgress},
	{"keys_are_per_user", idemPerUser},
	{"outside_a_transaction_is_unavailable", idemUnavailable},
	{"complete_needs_a_claim", idemCompleteNeedsClaim},
	{"bodies_round_trip", idemBodies},
	{"expired_record_is_claimed_afresh_and_purged", idemExpiry},
	{"replays_are_copies", idemCopies},
}

func newIdemKey() string { return "contract-" + uuid.NewString() }

var idemCreated = interfaces.IdempotentResponse{Status: 201, ContentType: "application/json", Body: []byte(`{"id":"created-once"}`)}

// claim claims key in a transaction of its own, ending it with commit, and returns what the claim answered.
func (h IdempotencyHarness) claim(t *testing.T, userID, key, fingerprint string, commit bool) (*interfaces.IdempotentResponse, error) {
	t.Helper()
	ctx, end := h.Begin(t, userID)
	defer end(commit)
	return h.Store.ClaimIdempotencyKey(ctx, userID, key, fingerprint)
}

// complete claims key and stores resp, in one committed transaction: a create that succeeded.
func (h IdempotencyHarness) complete(t *testing.T, userID, key, fingerprint string, resp interfaces.IdempotentResponse) {
	t.Helper()
	ctx, end := h.Begin(t, userID)
	defer end(false) // a no-op after the commit below
	if got, err := h.Store.ClaimIdempotencyKey(ctx, userID, key, fingerprint); err != nil || got != nil {
		t.Fatalf("first claim of %s = (%+v, %v), want claimed (nil, nil)", key, got, err)
	}
	if err := h.Store.CompleteIdempotencyKey(ctx, userID, key, resp); err != nil {
		t.Fatalf("CompleteIdempotencyKey: %v", err)
	}
	end(true)
}

func wantReplay(t *testing.T, got *interfaces.IdempotentResponse, err error, want interfaces.IdempotentResponse) {
	t.Helper()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got == nil {
		t.Fatal("replay: claimed again (nil, nil), want the stored response")
	}
	if got.Status != want.Status || got.ContentType != want.ContentType || got.BodyOmitted != want.BodyOmitted || !bytes.Equal(got.Body, want.Body) {
		t.Errorf("replay = {%d %q %q omitted=%v}, want {%d %q %q omitted=%v}",
			got.Status, got.ContentType, got.Body, got.BodyOmitted, want.Status, want.ContentType, want.Body, want.BodyOmitted)
	}
}

func wantClaimed(t *testing.T, got *interfaces.IdempotentResponse, err error) {
	t.Helper()
	if err != nil || got != nil {
		t.Fatalf("claim = (%+v, %v), want claimed (nil, nil)", got, err)
	}
}

func idemClaimCompleteReplay(t *testing.T, h IdempotencyHarness) {
	u, key := h.User(t), newIdemKey()
	h.complete(t, u, key, "POST /api/library a", idemCreated)
	// Replaying writes nothing: the record answers every retry the same way.
	for range 2 {
		got, err := h.claim(t, u, key, "POST /api/library a", true)
		wantReplay(t, got, err, idemCreated)
	}
}

func idemOtherFingerprint(t *testing.T, h IdempotencyHarness) {
	u, key := h.User(t), newIdemKey()
	h.complete(t, u, key, "POST /api/library a", idemCreated)
	if got, err := h.claim(t, u, key, "POST /api/library b", false); !errors.Is(err, interfaces.ErrIdempotencyKeyReused) {
		t.Fatalf("claim with another fingerprint = (%+v, %v), want ErrIdempotencyKeyReused", got, err)
	}
	got, err := h.claim(t, u, key, "POST /api/library a", false)
	wantReplay(t, got, err, idemCreated)
}

func idemRollbackReleases(t *testing.T, h IdempotencyHarness) {
	u, key := h.User(t), newIdemKey()
	ctx, end := h.Begin(t, u)
	got, err := h.Store.ClaimIdempotencyKey(ctx, u, key, "POST /api/orgs a")
	wantClaimed(t, got, err)
	if err := h.Store.CompleteIdempotencyKey(ctx, u, key, idemCreated); err != nil {
		t.Fatalf("CompleteIdempotencyKey: %v", err)
	}
	end(false) // the create failed after all: its record goes with it
	got, err = h.claim(t, u, key, "POST /api/orgs b", false)
	wantClaimed(t, got, err)
}

func idemInProgress(t *testing.T, h IdempotencyHarness) {
	u, key := h.User(t), newIdemKey()
	first, endFirst := h.Begin(t, u)
	defer endFirst(false)
	got, err := h.Store.ClaimIdempotencyKey(first, u, key, "POST /api/orgs a")
	wantClaimed(t, got, err)

	// The second claim is answered at once rather than waiting for the first transaction: a waiting request would
	// hold a pooled connection for as long as the first one runs.
	start := time.Now()
	got, err = h.claim(t, u, key, "POST /api/orgs a", false)
	if !errors.Is(err, interfaces.ErrIdempotencyKeyInProgress) {
		t.Fatalf("claim while another transaction holds the key = (%+v, %v), want ErrIdempotencyKeyInProgress", got, err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("the second claim waited %v for the first transaction", waited)
	}

	if err := h.Store.CompleteIdempotencyKey(first, u, key, idemCreated); err != nil {
		t.Fatalf("CompleteIdempotencyKey: %v", err)
	}
	endFirst(true)
	got, err = h.claim(t, u, key, "POST /api/orgs a", false)
	wantReplay(t, got, err, idemCreated)
}

func idemPerUser(t *testing.T, h IdempotencyHarness) {
	alice, bob, key := h.User(t), h.User(t), newIdemKey()
	h.complete(t, alice, key, "POST /api/auth/tokens a", idemCreated)
	bobs := interfaces.IdempotentResponse{Status: 201, ContentType: "application/json", Body: []byte(`{"id":"bobs"}`)}
	h.complete(t, bob, key, "POST /api/auth/tokens a", bobs)

	got, err := h.claim(t, alice, key, "POST /api/auth/tokens a", false)
	wantReplay(t, got, err, idemCreated)
	got, err = h.claim(t, bob, key, "POST /api/auth/tokens a", false)
	wantReplay(t, got, err, bobs)
}

func idemUnavailable(t *testing.T, h IdempotencyHarness) {
	u, key := h.User(t), newIdemKey()
	ctx := context.Background()
	if got, err := h.Store.ClaimIdempotencyKey(ctx, u, key, "POST /api/orgs a"); !errors.Is(err, interfaces.ErrIdempotencyUnavailable) {
		t.Errorf("claim outside a request transaction = (%+v, %v), want ErrIdempotencyUnavailable", got, err)
	}
	if err := h.Store.CompleteIdempotencyKey(ctx, u, key, idemCreated); !errors.Is(err, interfaces.ErrIdempotencyUnavailable) {
		t.Errorf("complete outside a request transaction = %v, want ErrIdempotencyUnavailable", err)
	}
}

func idemCompleteNeedsClaim(t *testing.T, h IdempotencyHarness) {
	u, key := h.User(t), newIdemKey()
	ctx, end := h.Begin(t, u)
	defer end(false)
	if err := h.Store.CompleteIdempotencyKey(ctx, u, key, idemCreated); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("complete without a claim = %v, want ErrNotFound", err)
	}

	done := newIdemKey()
	h.complete(t, u, done, "POST /api/orgs a", idemCreated)
	ctx2, end2 := h.Begin(t, u)
	defer end2(false)
	if err := h.Store.CompleteIdempotencyKey(ctx2, u, done, interfaces.IdempotentResponse{Status: 200}); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("completing a completed key again = %v, want ErrNotFound", err)
	}
}

func idemBodies(t *testing.T, h IdempotencyHarness) {
	u := h.User(t)
	for _, tc := range []struct {
		name string
		resp interfaces.IdempotentResponse
	}{
		{"omitted", interfaces.IdempotentResponse{Status: 201, BodyOmitted: true}},
		{"empty", interfaces.IdempotentResponse{Status: 204, Body: []byte{}}},
		{"binary", interfaces.IdempotentResponse{Status: 200, ContentType: "application/octet-stream", Body: []byte{0, 0xff, 'e', 'n', 'c', 0xc3, 0x28}}},
		{"redirect", interfaces.IdempotentResponse{Status: 303, ContentType: "text/plain", Body: []byte("see other")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := newIdemKey()
			h.complete(t, u, key, "POST /api/orgs "+tc.name, tc.resp)
			got, err := h.claim(t, u, key, "POST /api/orgs "+tc.name, false)
			wantReplay(t, got, err, tc.resp)
		})
	}
}

func idemExpiry(t *testing.T, h IdempotencyHarness) {
	u, old, fresh := h.User(t), newIdemKey(), newIdemKey()
	h.complete(t, u, old, "POST /api/orgs a", idemCreated)
	h.complete(t, u, fresh, "POST /api/orgs a", idemCreated)
	h.Expire(t, u, old)

	// An expired record no longer answers: the key is free for another request, and a rollback puts the expired
	// record back for the sweep.
	got, err := h.claim(t, u, old, "POST /api/orgs b", false)
	wantClaimed(t, got, err)

	if n := h.Purge(t); n < 1 {
		t.Errorf("the retention sweep removed %d records, want the expired one", n)
	}
	got, err = h.claim(t, u, old, "POST /api/orgs b", false)
	wantClaimed(t, got, err)
	got, err = h.claim(t, u, fresh, "POST /api/orgs a", false)
	wantReplay(t, got, err, idemCreated)
}

func idemCopies(t *testing.T, h IdempotencyHarness) {
	u, key := h.User(t), newIdemKey()
	resp := interfaces.IdempotentResponse{Status: 201, ContentType: "application/json", Body: []byte(`{"id":"copy"}`)}
	h.complete(t, u, key, "POST /api/orgs a", resp)
	resp.Body[2] = 'X' // the caller's buffer, reused after the store kept it

	got, err := h.claim(t, u, key, "POST /api/orgs a", false)
	want := interfaces.IdempotentResponse{Status: 201, ContentType: "application/json", Body: []byte(`{"id":"copy"}`)}
	wantReplay(t, got, err, want)
	got.Body[2] = 'Y'
	again, err := h.claim(t, u, key, "POST /api/orgs a", false)
	wantReplay(t, again, err, want)
}
