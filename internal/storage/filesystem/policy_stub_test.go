package filesystem

import (
	"context"
	"errors"
	"testing"

	"pad-core/models"
)

// TestSavePolicy_DoesNotReportFakeSuccess is the honesty gate on the local
// backend.
//
// SavePolicy returned nil — success — and wrote nothing, and GetPolicy then
// reported ErrNotFound. Desktop mode DOES have a backend (the filesystem one),
// and the handler's policyAvailable guard only checks `backend == nil`, so the
// path is reachable: a user saves a policy, is told it worked, and it is gone.
// A silent success is worse than an error here, because the user has no signal
// to act on and every later run gates on a policy that does not exist.
func TestSavePolicy_DoesNotReportFakeSuccess(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	ctx := context.Background()
	p := &models.Policy{ID: "p1", OrgID: "org-1", Name: "Release gate"}

	saveErr := b.SavePolicy(ctx, p)
	if saveErr == nil {
		// If a future change makes this actually persist, that is fine too —
		// but then it must be readable back.
		got, getErr := b.GetPolicy(ctx, "org-1", "p1")
		if getErr != nil || got == nil {
			t.Fatalf("SavePolicy reported success but GetPolicy returned (%v, %v) — the policy "+
				"was silently discarded, and the user was told it was saved", got, getErr)
		}
	}
}

// TestDeletePolicy_DoesNotReportFakeSuccess covers the same shape on delete:
// reporting that something was removed when nothing was stored is equally
// misleading to a caller that trusts the result.
func TestDeletePolicy_DoesNotReportFakeSuccess(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if err := b.DeletePolicy(context.Background(), "org-1", "nope"); err == nil {
		t.Error("DeletePolicy reported success for a policy this backend cannot store")
	} else if !errors.Is(err, ErrPoliciesUnsupported) {
		t.Errorf("DeletePolicy error should identify the cause, got %v", err)
	}
}
