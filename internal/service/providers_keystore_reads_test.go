package service

// R-provider-19: the catalogue and the listing probed the keystore once per spec (one Get per bare key, two for
// Copilot, one more per org-shareable spec for the organisation), each on context.Background with the store's own
// 5 s timeout: ~9-17 serial round trips per request, none of which a client that went away could cancel. They now
// read each scope's keys once — the caller's, the organisation's — under the request's context.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// keystoreProbe is the test backend's read side: it counts every keystore read (key by key or by scope) the way
// the database counts queries, and with block set holds each read until block closes or, for a read that has
// one, its context ends — a database that does not answer.
type keystoreProbe struct {
	reads atomic.Int32
	block chan struct{}
}

func (p *keystoreProbe) read(ctx context.Context) error {
	p.reads.Add(1)
	if p.block == nil {
		return nil
	}
	select {
	case <-p.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readNoContext is a read that takes no context: it waits for block whatever happens to the request.
func (p *keystoreProbe) readNoContext() {
	p.reads.Add(1)
	if p.block != nil {
		<-p.block
	}
}

// statesOf is what a scope's rows say of keys: absent keys have no entry.
func statesOf(keys []string, has func(string) bool, get func(string) (string, error)) map[string]storageif.SecretState {
	out := map[string]storageif.SecretState{}
	for _, k := range keys {
		if !has(k) {
			continue
		}
		v, err := get(k)
		switch {
		case errors.Is(err, storage.ErrSecretDecryptFailed):
			out[k] = storageif.SecretUnreadable
		case v == "":
			out[k] = storageif.SecretEmpty
		default:
			out[k] = storageif.SecretUsable
		}
	}
	return out
}

// probeKeys is the personal keystore over a FakeSecretStore, read through probe; unreadable keys (scope/provider)
// fail to decrypt, as the database keystore's do after a key rotation.
type probeKeys struct {
	*testutil.FakeSecretStore
	probe      *keystoreProbe
	unreadable map[string]bool
}

func (s *probeKeys) get(scope, provider string) (string, error) {
	if s.unreadable[scope+"/"+provider] {
		return "", storage.ErrSecretDecryptFailed
	}
	return s.FakeSecretStore.Get(scope, provider)
}

func (s *probeKeys) Get(scope, provider string) (string, error) {
	s.probe.readNoContext()
	return s.get(scope, provider)
}

func (s *probeKeys) Has(scope, provider string) (bool, error) {
	s.probe.readNoContext()
	return s.FakeSecretStore.Has(scope, provider)
}

func (s *probeKeys) GetCtx(ctx context.Context, scope, provider string) (string, error) {
	if err := s.probe.read(ctx); err != nil {
		return "", err
	}
	return s.get(scope, provider)
}

func (s *probeKeys) HasCtx(ctx context.Context, scope, provider string) (bool, error) {
	if err := s.probe.read(ctx); err != nil {
		return false, err
	}
	return s.FakeSecretStore.Has(scope, provider)
}

func (s *probeKeys) KeyStates(ctx context.Context, scope string, keys []string) (map[string]storageif.SecretState, error) {
	if err := s.probe.read(ctx); err != nil {
		return nil, err
	}
	return statesOf(keys,
		func(k string) bool { has, _ := s.FakeSecretStore.Has(scope, k); return has },
		func(k string) (string, error) { return s.get(scope, k) }), nil
}

// probeOrgKeys is probeKeys for the organisation keystore.
type probeOrgKeys struct {
	*testutil.FakeOrgSecretStore
	probe      *keystoreProbe
	unreadable map[string]bool
}

func (s *probeOrgKeys) getOrg(orgID, provider string) (string, error) {
	if s.unreadable[orgID+"/"+provider] {
		return "", storage.ErrSecretDecryptFailed
	}
	return s.FakeOrgSecretStore.GetOrg(orgID, provider)
}

func (s *probeOrgKeys) GetOrg(orgID, provider string) (string, error) {
	s.probe.readNoContext()
	return s.getOrg(orgID, provider)
}

func (s *probeOrgKeys) HasOrg(orgID, provider string) (bool, error) {
	s.probe.readNoContext()
	return s.FakeOrgSecretStore.HasOrg(orgID, provider)
}

func (s *probeOrgKeys) GetOrgCtx(ctx context.Context, orgID, provider string) (string, error) {
	if err := s.probe.read(ctx); err != nil {
		return "", err
	}
	return s.getOrg(orgID, provider)
}

func (s *probeOrgKeys) HasOrgCtx(ctx context.Context, orgID, provider string) (bool, error) {
	if err := s.probe.read(ctx); err != nil {
		return false, err
	}
	return s.FakeOrgSecretStore.HasOrg(orgID, provider)
}

func (s *probeOrgKeys) OrgKeyStates(ctx context.Context, orgID string, keys []string) (map[string]storageif.SecretState, error) {
	if err := s.probe.read(ctx); err != nil {
		return nil, err
	}
	return statesOf(keys,
		func(k string) bool { has, _ := s.FakeOrgSecretStore.HasOrg(orgID, k); return has },
		func(k string) (string, error) { return s.getOrg(orgID, k) }), nil
}

// newCountingProviderService is a cloud-profile provider service on the counting keystores, the personal and the
// organisation's sharing one probe.
func newCountingProviderService(t *testing.T, probe *keystoreProbe) (*ProviderService, *probeKeys, *probeOrgKeys) {
	t.Helper()
	secrets := &probeKeys{FakeSecretStore: testutil.NewFakeSecretStore(), probe: probe, unreadable: map[string]bool{}}
	orgKeys := &probeOrgKeys{FakeOrgSecretStore: testutil.NewFakeOrgSecretStore(), probe: probe, unreadable: map[string]bool{}}
	svc := newProviderServiceForProfile(t, secrets.FakeSecretStore, capability.Cloud)
	svc.secrets = secrets
	svc.orgKeys = orgKeys
	// As DI wires the database keystore: the same store is the keys and their request-scoped reads.
	svc.keyReads = secrets
	svc.orgKeyReads = orgKeys
	return svc, secrets, orgKeys
}

// GET /api/providers/catalog reads the keystore at most twice — the caller's keys once, the organisation's once —
// whatever the number of specs, and the statuses are the ones the per-key reads gave (R-provider-18's table).
func TestProviderCatalog_OneKeystoreReadPerScope(t *testing.T) {
	probe := &keystoreProbe{}
	svc, secrets, orgKeys := newCountingProviderService(t, probe)
	_ = secrets.Save("u1", "openai", "")                   // a legacy empty row: no request can use it
	_ = secrets.Save("u1", "glm", "stored-but-unreadable") // unreadable below
	_ = secrets.Save("u1", "claude", "sk-ant")
	_ = orgKeys.SaveOrg("org-1", "glm", "org-glm-key", "admin")
	_ = orgKeys.SaveOrg("org-1", "gemini", "rotated", "admin")
	_ = orgKeys.SaveOrg("org-1", "xai", "org-xai-key", "admin")
	secrets.unreadable["u1/glm"] = true
	orgKeys.unreadable["org-1/gemini"] = true

	entries, err := svc.ProviderCatalog(context.Background(), "u1", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 4 {
		t.Fatalf("the catalogue lists %d providers: the test needs the cloud profile's", len(entries))
	}
	if n := probe.reads.Load(); n > 2 {
		t.Errorf("the catalogue of %d providers read the keystore %d times, want at most 2 (one per scope)", len(entries), n)
	}
	want := map[string]struct {
		configured bool
		own, org   CredentialStatus
	}{
		"openai": {false, CredentialMissing, CredentialMissing},
		"claude": {true, CredentialConfigured, CredentialMissing},
		"glm":    {false, CredentialUnreadable, CredentialConfigured},
		"gemini": {false, CredentialMissing, CredentialUnreadable},
		"xai":    {true, CredentialMissing, CredentialConfigured},
	}
	for _, e := range entries {
		w, ok := want[e.ID]
		if !ok {
			continue
		}
		delete(want, e.ID)
		if e.Configured != w.configured || e.CredentialStatus != string(w.own) || e.OrgCredentialStatus != string(w.org) {
			t.Errorf("%s: configured=%v own=%q org=%q, want %v %q %q", e.ID, e.Configured, e.CredentialStatus, e.OrgCredentialStatus, w.configured, w.own, w.org)
		}
	}
	for id := range want {
		t.Errorf("%s missing from the catalogue", id)
	}

	// Without an organisation only the caller's keys are read.
	probe.reads.Store(0)
	if _, err := svc.ProviderCatalog(context.Background(), "u1", ""); err != nil {
		t.Fatal(err)
	}
	if n := probe.reads.Load(); n > 1 {
		t.Errorf("the catalogue without an organisation read the keystore %d times, want at most 1", n)
	}
}

// The listing resolves every spec's credential from the same two reads.
func TestListProviders_OneKeystoreReadPerScope(t *testing.T) {
	probe := &keystoreProbe{}
	svc, _, _ := newCountingProviderService(t, probe)
	providers, err := svc.ListProviders(context.Background(), "u1", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) < 4 {
		t.Fatalf("the listing has %d providers: the test needs the cloud profile's", len(providers))
	}
	if n := probe.reads.Load(); n > 2 {
		t.Errorf("the listing of %d providers read the keystore %d times, want at most 2 (one per scope)", len(providers), n)
	}
}

// A client that goes away ends the keystore read it was waiting on: the catalogue, the listing and the key-status
// route return the context's error instead of holding a pool connection until the database answers.
func TestKeystoreReads_EndWithTheRequest(t *testing.T) {
	probe := &keystoreProbe{block: make(chan struct{})}
	t.Cleanup(func() { close(probe.block) })
	svc, secrets, _ := newCountingProviderService(t, probe)
	_ = secrets.Save("u1", "openai", "sk")
	sys := NewSystemService(storage.NewInMemorySettingsStore(), secrets, nil, nil, "local", nil, nil).WithKeyReads(secrets)

	calls := map[string]func(context.Context) error{
		"catalogue": func(ctx context.Context) error {
			_, err := svc.ProviderCatalog(ctx, "u1", "org-1")
			return err
		},
		"listing": func(ctx context.Context) error {
			_, err := svc.ListProviders(ctx, "u1", "org-1")
			return err
		},
		"key status": func(ctx context.Context) error {
			_, err := sys.ApiKeyStatus(ctx, "u1", "openai")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- call(ctx) }()
			time.Sleep(50 * time.Millisecond) // the read is in flight
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("err = %v, want the request's context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the read outlived its request: still waiting on the keystore 2 s after the cancel")
			}
		})
	}
}

// keyByKey is the credential reads of one question about scope (and orgID): no scope read, each key read on its
// own, as a single spec's status is.
func keyByKey(svc *ProviderService, scope, orgID string) *credentialReads {
	return &credentialReads{s: svc, ctx: context.Background(), scope: scope, orgID: orgID}
}
