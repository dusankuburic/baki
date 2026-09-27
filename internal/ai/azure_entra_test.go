package ai

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// fakeCred hands out tok-1, tok-2, … with a configurable lifetime and delay.
type fakeCred struct {
	calls int32
	ttl   time.Duration
	delay time.Duration
}

func (f *fakeCred) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if len(opts.Scopes) != 1 || opts.Scopes[0] != azureFoundryScope {
		return azcore.AccessToken{}, context.Canceled
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	n := atomic.AddInt32(&f.calls, 1)
	ttl := f.ttl
	if ttl == 0 {
		ttl = time.Hour
	}
	return azcore.AccessToken{Token: "tok-" + string(rune('0'+n)), ExpiresOn: time.Now().Add(ttl)}, nil
}

func TestEntraTokenSource_CachesUntilNearExpiry(t *testing.T) {
	cred := &fakeCred{}
	src := newEntraTokenSource(cred)
	a, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := src.Token(context.Background())
	if a != b || atomic.LoadInt32(&cred.calls) != 1 {
		t.Errorf("token not cached: %q %q calls=%d", a, b, cred.calls)
	}

	// Within 5 minutes of expiry the next call refreshes.
	short := &fakeCred{ttl: 2 * time.Minute}
	src = newEntraTokenSource(short)
	_, _ = src.Token(context.Background())
	_, _ = src.Token(context.Background())
	if atomic.LoadInt32(&short.calls) != 2 {
		t.Errorf("near-expiry token was not refreshed: calls=%d", short.calls)
	}
}

func TestEntraTokenSource_InvalidateOnlyIfUnchanged(t *testing.T) {
	src := newEntraTokenSource(&fakeCred{})
	tok, _ := src.Token(context.Background())
	src.Invalidate("some-other-token")
	if src.token != tok {
		t.Error("invalidating a different token must keep the cached one")
	}
	src.Invalidate(tok)
	if src.token != "" {
		t.Error("invalidating the cached token must clear it")
	}
	next, _ := src.Token(context.Background())
	if next == tok {
		t.Error("after invalidation a fresh token is fetched")
	}
}

func TestEntraTokenSource_SingleFlight(t *testing.T) {
	cred := &fakeCred{delay: 30 * time.Millisecond}
	src := newEntraTokenSource(cred)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := src.Token(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&cred.calls); got != 1 {
		t.Errorf("concurrent first use fetched %d tokens, want 1", got)
	}
}

func TestEntraSourceFor_ReusesPerCredential(t *testing.T) {
	resetEntraSources()
	a, err := entraSourceFor("11111111-2222-3333-4444-555555555555", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "s1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := entraSourceFor("11111111-2222-3333-4444-555555555555", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "s1")
	c, _ := entraSourceFor("11111111-2222-3333-4444-555555555555", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "s2")
	if a != b {
		t.Error("same credential must share one token source")
	}
	if a == c {
		t.Error("a rotated secret must get its own token source")
	}
}
