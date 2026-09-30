package testutil

import (
	"sync"

	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// FakeAzureRegistry is the Azure AI Foundry registry for service and API
// tests: storage.MemoryAzureRegistry — the same rules as the file store
// (validation, uniqueness, limits, designations), for any number of user
// and org owners — plus failure injection and call counting. The shared
// contract suite (internal/storage/contract) runs against it, Postgres and
// the file store alike.
//
// It is transactional-like by default (PendingWrites false, like Postgres);
// EnablePendingWrites switches it to the file store's two-step create, and
// calling it again with a new id simulates a restart. SetStatus sets the
// registryState it reports. Do not call SetHook: the fake uses it.
type FakeAzureRegistry struct {
	*storage.MemoryAzureRegistry

	mu       sync.Mutex
	failures map[string]error
	calls    map[string]int
}

var _ storageif.AzureRegistryStore = (*FakeAzureRegistry)(nil)

// NewFakeAzureRegistry returns an empty fake registry.
func NewFakeAzureRegistry() *FakeAzureRegistry {
	f := &FakeAzureRegistry{
		MemoryAzureRegistry: storage.NewMemoryAzureRegistry(),
		failures:            map[string]error{},
		calls:               map[string]int{},
	}
	f.SetHook(f.intercept)
	return f
}

// intercept counts the call and returns the injected failure, if any.
func (f *FakeAzureRegistry) intercept(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	return f.failures[method]
}

// FailOn makes every later call of method — an AzureRegistryStore method
// name such as "ListModels" or "WithOwnerLock" — return err instead of
// running; a nil err removes the failure. State cannot fail (it has no
// error result) but is still counted.
func (f *FakeAzureRegistry) FailOn(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failures, method)
		return
	}
	f.failures[method] = err
}

// ClearFailures removes every injected failure.
func (f *FakeAzureRegistry) ClearFailures() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = map[string]error{}
}

// Calls reports how many times method was called, failed calls included.
func (f *FakeAzureRegistry) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// ResetCalls zeroes every call count.
func (f *FakeAzureRegistry) ResetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = map[string]int{}
}
