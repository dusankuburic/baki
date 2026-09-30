package testutil

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/contract"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// fakeAzureHarness runs the registry contract on the fake; pending selects
// the file store's two-step create (EnablePendingWrites).
func fakeAzureHarness(pending bool) contract.AzureRegistryHarness {
	owner := func(kind storageif.AzureOwnerKind) storageif.AzureOwner {
		return storageif.AzureOwner{Kind: kind, ID: string(kind) + "-" + contract.NewAzureTestID()}
	}
	return contract.AzureRegistryHarness{
		Fresh: func(t *testing.T, kind storageif.AzureOwnerKind) (storageif.AzureRegistryStore, storageif.AzureOwner, bool) {
			f := NewFakeAzureRegistry()
			if pending {
				f.EnablePendingWrites("boot-" + contract.NewAzureTestID())
			}
			return f, owner(kind), true
		},
		Another: func(t *testing.T, _ storageif.AzureRegistryStore, kind storageif.AzureOwnerKind) (storageif.AzureOwner, bool) {
			return owner(kind), true
		},
	}
}

// TestFakeAzureRegistry_Contract: the fake answers like Postgres and the
// file store, in both write models.
func TestFakeAzureRegistry_Contract(t *testing.T) {
	t.Run("transactional", func(t *testing.T) { contract.RunAzureRegistrySuite(t, fakeAzureHarness(false)) })
	t.Run("pending_writes", func(t *testing.T) { contract.RunAzureRegistrySuite(t, fakeAzureHarness(true)) })
}

// TestFakeAzureRegistry_FailureInjectionAndCalls pins the fake's test
// controls: an injected failure replaces the method (nothing is written),
// calls are counted, and clearing restores the method.
func TestFakeAzureRegistry_FailureInjectionAndCalls(t *testing.T) {
	ctx := context.Background()
	f := NewFakeAzureRegistry()
	owner := storageif.AzureUserOwner("u1")
	boom := errors.New("registry down")

	f.FailOn("CreateConnection", boom)
	if _, err := f.CreateConnection(ctx, owner, storageif.AzureConnectionRecord{ID: "default", Name: "res"}); !errors.Is(err, boom) {
		t.Fatalf("CreateConnection with an injected failure = %v, want %v", err, boom)
	}
	if n, _ := f.Counts(ctx, owner); n.Connections != 0 {
		t.Errorf("a failed call wrote %d resources", n.Connections)
	}
	f.FailOn("CreateConnection", nil)
	if _, err := f.CreateConnection(ctx, owner, storageif.AzureConnectionRecord{ID: "default", Name: "res"}); err != nil {
		t.Fatalf("CreateConnection after removing the failure: %v", err)
	}
	if got := f.Calls("CreateConnection"); got != 2 {
		t.Errorf("Calls(CreateConnection) = %d, want 2 (failed calls count)", got)
	}

	f.FailOn("WithOwnerLock", boom)
	ran := false
	if err := f.WithOwnerLock(ctx, owner, func(context.Context) error { ran = true; return nil }); !errors.Is(err, boom) || ran {
		t.Errorf("WithOwnerLock with an injected failure = %v (fn ran: %v)", err, ran)
	}
	f.ClearFailures()
	if err := f.WithOwnerLock(ctx, owner, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("WithOwnerLock after ClearFailures = %v (fn ran: %v)", err, ran)
	}

	f.ResetCalls()
	if got := f.Calls("CreateConnection"); got != 0 {
		t.Errorf("Calls after ResetCalls = %d", got)
	}
	f.SetStatus(storageif.AzureRegistryStatus{State: storageif.AzureRegistryReadOnly})
	if st := f.State(ctx); st.State != storageif.AzureRegistryReadOnly {
		t.Errorf("State = %+v, want the status set", st)
	}
	if got := f.Calls("State"); got != 1 {
		t.Errorf("Calls(State) = %d, want 1", got)
	}
}
