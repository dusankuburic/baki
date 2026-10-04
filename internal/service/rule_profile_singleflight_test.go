package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// countingProfileBackend counts the org reads a resolve makes.
type countingProfileBackend struct {
	*testutil.FakeBackend
	orgReads atomic.Int32
}

func (b *countingProfileBackend) LoadOrgSettings(ctx context.Context, orgID string) (*storageif.AppSettings, error) {
	b.orgReads.Add(1)
	return b.FakeBackend.LoadOrgSettings(ctx, orgID)
}

// TestRuleProfileResolver_ConcurrentColdResolveBuildsOnce: a cold or expired
// profile used to be resolved once PER concurrent caller — a "fix all" batch,
// the scanner sweep and interactive analyses of one org each re-reading org
// settings and custom rules and re-compiling every rule. The singleflight
// around the cold build makes them share one resolve.
func TestRuleProfileResolver_ConcurrentColdResolveBuildsOnce(t *testing.T) {
	b := &countingProfileBackend{FakeBackend: testutil.NewFakeBackend()}
	storeRule(t, b.FakeBackend, "org-1", "r1", "HTTP", true)
	r := NewRuleProfileResolver(b, deploymentSettings(nil), nil)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if p := r.Resolve(context.Background(), "org-1"); p == nil {
				t.Error("Resolve returned nil")
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := b.orgReads.Load(); got != 1 {
		t.Fatalf("cold resolve of one org made %d org-settings reads for 16 concurrent callers, want 1", got)
	}
}
