package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"pad-analyzer/internal/egress"
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

	// Within entraTokenMinRemaining of expiry the next call waits for a
	// refresh.
	short := &fakeCred{ttl: entraTokenMinRemaining / 2}
	src = newEntraTokenSource(short)
	_, _ = src.Token(context.Background())
	_, _ = src.Token(context.Background())
	if atomic.LoadInt32(&short.calls) != 2 {
		t.Errorf("near-expiry token was not refreshed: calls=%d", short.calls)
	}
}

// between entraTokenRefreshAhead and entraTokenMinRemaining of expiry the cached token is returned at once
// and refreshed in the background — no caller pays the round trip, and a failed refresh fails nobody.
func TestEntraTokenSource_EarlyRefreshRunsInTheBackground(t *testing.T) {
	cred := &fakeCred{ttl: 2 * time.Minute, delay: 30 * time.Millisecond}
	src := newEntraTokenSource(cred)
	first, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	again, err := src.Token(context.Background())
	if err != nil || again != first {
		t.Fatalf("a token with more than %v left must be served from the cache: %q %v", entraTokenMinRemaining, again, err)
	}
	if elapsed := time.Since(start); elapsed >= cred.delay {
		t.Errorf("the caller waited %v for the background refresh", elapsed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&cred.calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := atomic.LoadInt32(&cred.calls); n != 2 {
		t.Fatalf("token fetches = %d, want the initial one plus one background refresh", n)
	}
	for time.Now().Before(deadline) {
		src.mu.Lock()
		cached := src.token
		src.mu.Unlock()
		if cached != first {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	src.mu.Lock()
	cached := src.token
	src.mu.Unlock()
	if cached == first {
		t.Error("the background refresh must fill the cache for the next caller")
	}

	// A failing refresh keeps serving the cached token, and it is not
	// retried by every caller while the cooldown lasts.
	flaky := &flakyCred{ttl: 2 * time.Minute}
	src = newEntraTokenSource(flaky)
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	flaky.fail.Store(true)
	for range 3 {
		if got, err := src.Token(context.Background()); err != nil || got != tok {
			t.Fatalf("a failed background refresh must not fail the caller: %q %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for flaky.calls.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := flaky.calls.Load(); n != 2 {
		t.Fatalf("token fetches = %d after the failed refresh, want 2", n)
	}
	for range 5 {
		if got, err := src.Token(context.Background()); err != nil || got != tok {
			t.Fatalf("within the cooldown the cached token must be served: %q %v", got, err)
		}
	}
	if n := flaky.calls.Load(); n != 2 {
		t.Errorf("token fetches = %d across five more turns, want 2 (the cooldown holds the failed refresh back)", n)
	}
	// Once the cooldown lapses, the next turn refreshes again.
	src.mu.Lock()
	src.refreshFailedAt = time.Now().Add(-2 * entraRebuildCooldown)
	src.mu.Unlock()
	if got, err := src.Token(context.Background()); err != nil || got != tok {
		t.Fatalf("after the cooldown the cached token is still served: %q %v", got, err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for flaky.calls.Load() != 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := flaky.calls.Load(); n != 3 {
		t.Fatalf("token fetches = %d after the cooldown, want 3", n)
	}
	// Below entraTokenMinRemaining a caller still waits for a real fetch and
	// surfaces its error.
	src.mu.Lock()
	src.expires = time.Now().Add(entraTokenMinRemaining / 2)
	src.mu.Unlock()
	if _, err := src.Token(context.Background()); err == nil {
		t.Error("a caller below the minimum remaining must see the refresh error")
	}
}

// flakyCred mints tokens until fail is set, then refuses every request.
type flakyCred struct {
	fail  atomic.Bool
	calls atomic.Int32
	ttl   time.Duration
}

func (f *flakyCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	n := f.calls.Add(1)
	if f.fail.Load() {
		return azcore.AccessToken{}, errors.New("AADSTS90033: A transient error has occurred")
	}
	return azcore.AccessToken{Token: fmt.Sprintf("tok-%d", n), ExpiresOn: time.Now().Add(f.ttl)}, nil
}

// stickyCred returns the SAME token on every GetToken, as azidentity's MSAL cache does for an unexpired
// token; a rebuilt credential is the only way to a different one.
type stickyCred struct {
	token string
	calls atomic.Int32
}

func (s *stickyCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	s.calls.Add(1)
	return azcore.AccessToken{Token: s.token, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// D-26: Invalidate clears the cache AND rebuilds the credential (a fresh MSAL cache), bounded by
// entraRebuildCooldown; without a constructor (tests) it only clears the cache.
func TestEntraTokenSource_InvalidateRebuildsCredential(t *testing.T) {
	src := newEntraTokenSource(&stickyCred{token: "tok-1"})
	var rebuilds atomic.Int32
	src.newCred = func() (azcore.TokenCredential, error) {
		n := rebuilds.Add(1)
		return &stickyCred{token: fmt.Sprintf("tok-%d", n+1)}, nil
	}
	tok, err := src.Token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("first token = %q, %v", tok, err)
	}
	src.Invalidate("some-other-token")
	if rebuilds.Load() != 0 {
		t.Error("invalidating a different token must not rebuild the credential")
	}
	src.Invalidate(tok)
	if rebuilds.Load() != 1 {
		t.Fatalf("rebuilds after invalidation = %d, want 1", rebuilds.Load())
	}
	if next, _ := src.Token(context.Background()); next != "tok-2" {
		t.Errorf("after invalidation the rebuilt credential must sign in: got %q", next)
	}
	// Within the cooldown a second refusal only clears the cache: the same
	// credential (and MSAL's cached token) answers again.
	src.Invalidate("tok-2")
	if rebuilds.Load() != 1 {
		t.Errorf("a rebuild within %v of the last one must be skipped, got %d rebuilds", entraRebuildCooldown, rebuilds.Load())
	}
	if again, _ := src.Token(context.Background()); again != "tok-2" {
		t.Errorf("without a rebuild the sticky credential answers the same token, got %q", again)
	}
	src.mu.Lock()
	src.rebuiltAt = time.Now().Add(-2 * entraRebuildCooldown)
	src.mu.Unlock()
	src.Invalidate("tok-2")
	if rebuilds.Load() != 2 {
		t.Errorf("past the cooldown the credential is rebuilt again, got %d rebuilds", rebuilds.Load())
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

// The token fetch must not bypass the egress guard: the credential's transport is the package's runtime
// client (guarded, no-redirects in cloud mode), not azcore's default pipeline.
func TestEntraCredentialOptions_UseTheRuntimeClient(t *testing.T) {
	saveAzureRuntime(t, AzureFoundryOptions{Egress: egress.BlockInternal})
	opts := entraCredentialOptions()
	if opts.Transport != policy.Transporter(azureHTTPClient()) {
		t.Fatalf("credential transport = %T, want the runtime azure client", opts.Transport)
	}
	tr, ok := azureHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("cloud client transport = %T, want a guarded *http.Transport", azureHTTPClient().Transport)
	}
	// The guard is live on the token path too: an internal address is refused at the dial, while the
	// authority itself (a fixed public host) passes — the property that keeps Entra auth working in cloud mode.
	if _, err := tr.DialContext(context.Background(), "tcp", "127.0.0.1:443"); !errors.Is(err, egress.ErrInternalAddress) {
		t.Errorf("a loopback dial through the credential's transport must be refused in cloud mode, got %v", err)
	}
	if azureHTTPClient().CheckRedirect == nil {
		t.Error("the credential's client must refuse redirects")
	}
}

// The registry makes room by dropping the least recently used source, not by wiping itself (the wipe also
// discarded the sources active chat turns were about to use).
func TestEntraSources_EvictionDropsLeastRecentlyUsed(t *testing.T) {
	resetEntraSources()
	t.Cleanup(resetEntraSources)
	const tenant = "11111111-2222-3333-4444-555555555555"
	const client = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	src := func(secret string) *entraTokenSource {
		t.Helper()
		s, err := entraSourceFor(tenant, client, secret)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// Fill the registry, then age the FIRST source so it is unambiguously the
	// least recently used one.
	first := src("s-old")
	for i := 1; i < entraSourcesMax; i++ {
		src(fmt.Sprintf("s-%03d", i))
	}
	fresh := src(fmt.Sprintf("s-%03d", entraSourcesMax-1)) // hit: refreshes, no growth
	entraSources.mu.Lock()
	for k, e := range entraSources.m {
		if e.src == first {
			e.lastUsed = time.Now().Add(-time.Hour)
			entraSources.m[k] = e
		}
	}
	entraSources.mu.Unlock()

	// One more principal beyond the bound evicts exactly the stale source.
	next := src("s-overflow")
	entraSources.mu.Lock()
	size := len(entraSources.m)
	firstKept := lookupEntraSourceLocked(first)
	freshKept := lookupEntraSourceLocked(fresh)
	nextKept := lookupEntraSourceLocked(next)
	entraSources.mu.Unlock()
	if size != entraSourcesMax {
		t.Errorf("registry size after eviction = %d, want %d", size, entraSourcesMax)
	}
	if firstKept {
		t.Error("the least recently used source must be evicted")
	}
	if !freshKept || !nextKept {
		t.Error("recently used sources must survive the eviction")
	}
}

// lookupEntraSourceLocked reports whether the registry holds src (caller
// holds the mutex).
func lookupEntraSourceLocked(src *entraTokenSource) bool {
	for _, e := range entraSources.m {
		if e.src == src {
			return true
		}
	}
	return false
}

// a source nobody built a provider from for entraSourceIdleTTL is reclaimed on the next registry access —
// the LRU cap alone never fires on a small deployment.
func TestEntraSources_IdleEntriesArePruned(t *testing.T) {
	resetEntraSources()
	t.Cleanup(resetEntraSources)
	const tenant = "11111111-2222-3333-4444-555555555555"
	const client = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	stale, err := entraSourceFor(tenant, client, "s-stale")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := entraSourceFor(tenant, client, "s-fresh")
	if err != nil {
		t.Fatal(err)
	}
	entraSources.mu.Lock()
	for k, e := range entraSources.m {
		if e.src == stale {
			e.lastUsed = time.Now().Add(-entraSourceIdleTTL - time.Minute)
			entraSources.m[k] = e
		}
	}
	entraSources.mu.Unlock()

	if _, err := entraSourceFor(tenant, client, "s-third"); err != nil {
		t.Fatal(err)
	}
	entraSources.mu.Lock()
	staleKept := lookupEntraSourceLocked(stale)
	freshKept := lookupEntraSourceLocked(fresh)
	size := len(entraSources.m)
	entraSources.mu.Unlock()
	if staleKept {
		t.Error("an entry idle past entraSourceIdleTTL must be pruned")
	}
	if !freshKept || size != 2 {
		t.Errorf("fresh entries must survive the prune: kept=%v size=%d", freshKept, size)
	}
	// A pruned principal that comes back gets a new source (one sign-in).
	if again, _ := entraSourceFor(tenant, client, "s-stale"); again == stale {
		t.Error("a pruned source must not be handed out again")
	}
	if forgetEntraSource(tenant, client, "s-fresh"); lookupEntraSource(fresh) {
		t.Error("forgetEntraSource must drop the principal's entry")
	}
}

func lookupEntraSource(src *entraTokenSource) bool {
	entraSources.mu.Lock()
	defer entraSources.mu.Unlock()
	return lookupEntraSourceLocked(src)
}

// blockingCred blocks every GetToken until release is closed or the fetch's own ctx ends, so a test can
// cancel the caller that started the fetch while another waits on it.
type blockingCred struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingCred() *blockingCred {
	return &blockingCred{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingCred) GetToken(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	b.calls.Add(1)
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return azcore.AccessToken{Token: "tok-shared", ExpiresOn: time.Now().Add(time.Hour)}, nil
	case <-ctx.Done():
		return azcore.AccessToken{}, ctx.Err()
	}
}

// the shared fetch is detached from the caller that started it: that caller's deadline ends its own wait
// promptly, a concurrent caller with a live ctx still gets the token, and the cache is filled.
func TestEntraTokenSource_LeaderCancelDoesNotFailJoiners(t *testing.T) {
	cred := newBlockingCred()
	src := newEntraTokenSource(cred)

	leaderCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	leaderErr := make(chan error, 1)
	go func() {
		_, err := src.Token(leaderCtx)
		leaderErr <- err
	}()
	<-cred.started

	type result struct {
		tok string
		err error
	}
	joiner := make(chan result, 1)
	go func() {
		tok, err := src.Token(context.Background())
		joiner <- result{tok, err}
	}()

	select {
	case err := <-leaderErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the leader must end with its own deadline, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leader did not return at its deadline")
	}
	close(cred.release)
	select {
	case r := <-joiner:
		if r.err != nil || r.tok != "tok-shared" {
			t.Errorf("the joiner must get the token: %q %v", r.tok, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the joiner never returned")
	}
	if tok, err := src.Token(context.Background()); err != nil || tok != "tok-shared" {
		t.Errorf("the detached fetch must fill the cache: %q %v", tok, err)
	}
	if n := cred.calls.Load(); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}
}

const entraTestTenant = "11111111-2222-3333-4444-555555555555"

// entraAuthority is a fake Entra ID authority (tenant discovery + token endpoint) that refuses every
// client-credentials request with the given status and body, counting token requests.
func entraAuthority(t *testing.T, status int, body map[string]any) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var tokenCalls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "https://" + r.Host + "/" + entraTestTenant
		switch {
		case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
			writeJSON(w, http.StatusOK, map[string]any{
				"token_endpoint":         base + "/oauth2/v2.0/token",
				"authorization_endpoint": base + "/oauth2/v2.0/authorize",
				"issuer":                 base + "/v2.0",
			})
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			tokenCalls.Add(1)
			writeJSON(w, status, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &tokenCalls
}

// a refused client secret is an auth failure (ErrApiKeyInvalid, ErrEntraCredentialsRejected) with a one-line
// message carrying the AADSTS code — not azidentity's multi-line dump — and is not retried with a second sign-in.
func TestEntraTokenSource_RejectedCredentialsAreOneLineAuthErrors(t *testing.T) {
	authority, tokenCalls := entraAuthority(t, http.StatusUnauthorized, map[string]any{
		"error":             "invalid_client",
		"error_description": "AADSTS7000215: Invalid client secret provided. Ensure the secret being sent in the request is the client secret value.\r\nTrace ID: 0000\r\nCorrelation ID: 1111\r\nTimestamp: 2026-09-27 10:00:00Z",
		"error_codes":       []int{7000215},
		"trace_id":          "0000",
		"correlation_id":    "1111",
	})
	cred, err := azidentity.NewClientSecretCredential(entraTestTenant, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "wrong-secret",
		&azidentity.ClientSecretCredentialOptions{
			ClientOptions:            azcore.ClientOptions{Cloud: cloud.Configuration{ActiveDirectoryAuthorityHost: authority.URL + "/"}, Transport: authority.Client()},
			DisableInstanceDiscovery: true,
		})
	if err != nil {
		t.Fatal(err)
	}

	_, err = newEntraTokenSource(cred).Token(context.Background())
	if !errors.Is(err, ErrApiKeyInvalid) || !errors.Is(err, ErrEntraCredentialsRejected) || !errors.Is(err, errEntraToken) {
		t.Fatalf("want an auth failure, got %v", err)
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n") || !strings.Contains(msg, "AADSTS7000215 invalid_client") {
		t.Errorf("want one line with the AADSTS code, got %q", msg)
	}
	for _, leak := range []string{"Trace ID", "orrelation", "aka.ms", "RESPONSE 401"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message leaks %q: %q", leak, msg)
		}
	}
	var afe *azidentity.AuthenticationFailedError
	if !errors.As(err, &afe) {
		t.Error("the azidentity error must stay reachable for the log")
	}
	if n := tokenCalls.Load(); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}

	// Through the provider: one sign-in, no request to the resource, no
	// invalidate-and-refetch round.
	var served atomic.Int32
	resource := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served.Add(1) }))
	t.Cleanup(resource.Close)
	cfg := AzureFoundryConfig{Endpoint: resource.URL, Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra,
		TenantID: entraTestTenant, ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "wrong-secret"}}
	p := newAzureFoundryProvider(cfg, nil, resource.Client())
	p.tokens = newEntraTokenSource(cred)
	p.tokenFn = p.entraToken
	_, err = p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, ErrEntraCredentialsRejected) || strings.ContainsAny(err.Error(), "\r\n") {
		t.Errorf("chat: want a one-line rejected-credentials error, got %v", err)
	}
	if n := tokenCalls.Load(); n != 2 {
		t.Errorf("token requests after the chat = %d, want 2 (one per source, no retry)", n)
	}
	if served.Load() != 0 {
		t.Error("the resource must not be called without a token")
	}
}

// erroringCred returns err from every GetToken.
type erroringCred struct{ err error }

func (f erroringCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, f.err
}

// other token failures are one line too, and are not auth
// failures (the credentials may be fine).
func TestEntraTokenSource_OtherFailuresAreOneLine(t *testing.T) {
	outage := &azidentity.AuthenticationFailedError{RawResponse: &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Status:     "503 Service Unavailable",
		Body:       io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable","error_codes":[50001],"trace_id":"t"}`)),
	}}
	network := errors.New("ClientSecretCredential: Post \"https://login.microsoftonline.com/x/oauth2/v2.0/token\": dial tcp: lookup login.microsoftonline.com: no such host\nsecond line")
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"token endpoint outage": {outage, "AADSTS50001 temporarily_unavailable"},
		"network failure":       {network, "does not resolve"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newEntraTokenSource(erroringCred{tc.err}).Token(context.Background())
			if err == nil || !errors.Is(err, errEntraToken) {
				t.Fatalf("want an entra token error, got %v", err)
			}
			if errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrEntraCredentialsRejected) {
				t.Errorf("not an auth failure: %v", err)
			}
			// D-30: a typed sentinel the chat service can code and copy — deliberately NOT ErrProviderDown:
			// the retry wrapper would re-run 30 s fetches and the breaker would open for the identity outage.
			if !errors.Is(err, ErrEntraTokenUnavailable) {
				t.Errorf("want ErrEntraTokenUnavailable, got %v", err)
			}
			if errors.Is(err, ErrProviderDown) || isRetryable(err) {
				t.Errorf("a token-endpoint failure must not be retryable / provider-down: %v", err)
			}
			if msg := err.Error(); strings.ContainsAny(msg, "\r\n") || !strings.Contains(msg, tc.want) {
				t.Errorf("want one line containing %q, got %q", tc.want, msg)
			}
		})
	}
	// A refused credential is not "unavailable".
	rejected := classifyEntraTokenError(&azidentity.AuthenticationFailedError{RawResponse: &http.Response{
		StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_client","error_codes":[7000215]}`)),
	}})
	if errors.Is(rejected, ErrEntraTokenUnavailable) || !errors.Is(rejected, ErrEntraCredentialsRejected) {
		t.Errorf("a refused credential must be ErrEntraCredentialsRejected only, got %v", rejected)
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("  first line \r\nsecond", 100); got != "first line" {
		t.Errorf("oneLine = %q", got)
	}
	if got := oneLine("ééé", 3); got != "é…" {
		t.Errorf("a cut must fall on a rune boundary: %q", got)
	}
}

// The CLI credential's token source is shared per tenant and its
// "not signed in" failure becomes the az-login instruction.
func TestEntraCLISourceAndUnavailableMessage(t *testing.T) {
	t.Cleanup(resetEntraSources)
	src, err := entraCLISourceFor("")
	if err != nil {
		t.Fatalf("entraCLISourceFor: %v", err)
	}
	if again, _ := entraCLISourceFor(""); again != src {
		t.Error("one token source per (empty) tenant")
	}
	tenant, err := entraCLISourceFor("contoso.onmicrosoft.com")
	if err != nil || tenant == src {
		t.Errorf("a named tenant needs its own source: %v", err)
	}
	forgetEntraCLISource("contoso.onmicrosoft.com")

	classified := classifyEntraTokenError(errors.New("AzureCLICredential: executable not found on path"))
	if _, ok := EntraRejectionReason(classified); ok {
		t.Error("an unavailable CLI is not a credential rejection")
	}
	if !strings.Contains(classified.Error(), "az login") {
		t.Errorf("unavailable CLI = %q, want the az login instruction", classified.Error())
	}
	if !errors.Is(classified, errEntraToken) || !errors.Is(classified, ErrEntraTokenUnavailable) {
		t.Errorf("the unavailable CLI must be a token-unavailable failure, got %v", classified)
	}
}

// The CLI registry prunes its own map under its own lock — it used to run the service-principal prune,
// iterating entraSources.m while holding only entraCLISources.mu, crashing with "concurrent map iteration
// and map write". Run under -race.
func TestEntraCLISourceFor_ConcurrentWithServicePrincipal(t *testing.T) {
	resetEntraSources()
	t.Cleanup(resetEntraSources)
	const tenant = "11111111-2222-3333-4444-555555555555"
	const client = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := range 25 {
				if _, err := entraCLISourceFor(fmt.Sprintf("tenant-%d.onmicrosoft.com", j%5)); err != nil {
					t.Errorf("entraCLISourceFor: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := range 25 {
				if _, err := entraSourceFor(tenant, client, fmt.Sprintf("s-%d-%d", i, j)); err != nil {
					t.Errorf("entraSourceFor: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	entraSources.mu.Lock()
	sp := len(entraSources.m)
	entraSources.mu.Unlock()
	entraCLISources.mu.Lock()
	cli := len(entraCLISources.m)
	entraCLISources.mu.Unlock()
	if sp != 100 || cli != 5 {
		t.Errorf("registries = %d service-principal / %d CLI sources, want 100 / 5", sp, cli)
	}
}

// A CLI source nobody built a provider from for entraSourceIdleTTL is
// reclaimed on the next CLI registry access (the CLI map was never pruned).
func TestEntraCLISources_IdleEntriesArePruned(t *testing.T) {
	resetEntraSources()
	t.Cleanup(resetEntraSources)
	stale, err := entraCLISourceFor("stale.onmicrosoft.com")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := entraCLISourceFor("fresh.onmicrosoft.com")
	if err != nil {
		t.Fatal(err)
	}
	entraCLISources.mu.Lock()
	for k, e := range entraCLISources.m {
		if e.src == stale {
			e.lastUsed = time.Now().Add(-entraSourceIdleTTL - time.Minute)
			entraCLISources.m[k] = e
		}
	}
	entraCLISources.mu.Unlock()

	if _, err := entraCLISourceFor("third.onmicrosoft.com"); err != nil {
		t.Fatal(err)
	}
	entraCLISources.mu.Lock()
	var staleKept, freshKept bool
	for _, e := range entraCLISources.m {
		staleKept = staleKept || e.src == stale
		freshKept = freshKept || e.src == fresh
	}
	size := len(entraCLISources.m)
	entraCLISources.mu.Unlock()
	if staleKept {
		t.Error("a CLI source idle past entraSourceIdleTTL must be pruned")
	}
	if !freshKept || size != 2 {
		t.Errorf("fresh CLI sources must survive the prune: kept=%v size=%d", freshKept, size)
	}
	if again, _ := entraCLISourceFor("stale.onmicrosoft.com"); again == stale {
		t.Error("a pruned CLI source must not be handed out again")
	}
}
