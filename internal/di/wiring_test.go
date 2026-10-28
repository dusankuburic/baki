package di

import (
	"context"
	"sync/atomic"
	"testing"

	"go.uber.org/fx"

	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/reqtx"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// The services build — constructors run, not only the graph's types (TestAppGraph) — in each deployment
// mode, with the registry and the rule wiring reaching the services that take them as constructor
// parameters. The desktop has no backend; local+db and cloud run on one.
func TestServiceModule_BuildsInEveryMode(t *testing.T) {
	// The desktop modes resolve the per-user config directory: keep it out of the real one.
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("AppData", home)
	t.Setenv("HOME", home)

	modes := []struct {
		name    string
		mode    config.DeploymentMode
		backend func() storageif.StorageBackend
		auth    bool
	}{
		{"desktop", config.ModeLocal, func() storageif.StorageBackend { return nil }, false},
		{"local+db", config.ModeLocal, func() storageif.StorageBackend { return testutil.NewFakeBackend() }, false},
		{"cloud", config.ModeCloud, func() storageif.StorageBackend { return testutil.NewFakeBackend() }, true},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Mode = m.mode
			cfg.Auth.Enabled = m.auth
			reads := &recordingKeyReads{keys: testutil.NewFakeSecretStore()}
			var (
				analysis  *service.AnalysisService
				azure     *service.AzureModelService
				chat      *service.ChatService
				dashboard *service.DashboardService
				knowledge *rag.KnowledgeService
				library   *service.LibraryService
				providers *service.ProviderService
				system    *service.SystemService
			)
			app := fx.New(
				fx.NopLogger,
				fx.Provide(
					func() context.Context { return context.Background() },
					func() *config.Config { return cfg },
					func(cfg *config.Config) config.DeploymentMode { return cfg.Mode },
					m.backend,
					func() service.EventNotifier { return service.NilNotifier{} },
					func() *collaboration.OrgService { return nil },
				),
				noDatabase(),
				fakeKeyStoreWith(reads),
				ServiceModule,
				redisx.Module,
				fx.Populate(&analysis, &azure, &chat, &dashboard, &knowledge, &library, &providers, &system),
			)
			if err := app.Err(); err != nil {
				t.Fatalf("build: %v", err)
			}
			if providers.AzureModels() != azure {
				t.Error("the provider service does not hold the graph's Azure model registry")
			}
			if analysis.AuthEnabled() != m.auth {
				t.Errorf("AnalysisService.AuthEnabled() = %v, want %v", analysis.AuthEnabled(), m.auth)
			}
			if knowledge == nil || chat == nil || dashboard == nil || library == nil || system == nil {
				t.Error("a service was not built")
			}
			// The keystore's request-scoped reads reach the services that read keys for a request (R-provider-19):
			// the catalogue reads the caller's keys in one read, the key-status route under the request's context.
			reads.calls.Store(0)
			if _, err := providers.ProviderCatalog(context.Background(), "u1", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := system.ApiKeyStatus(context.Background(), "u1", "openai"); err != nil {
				t.Fatal(err)
			}
			if n := reads.calls.Load(); n != 2 {
				t.Errorf("the keystore's request-scoped reads served %d reads, want 2 (the catalogue's scope read, the key status)", n)
			}
		})
	}
}

// fakeKeyStore provides what internal/app's provideKeyStore provides, on the test keystore: the keys, the listing
// of their families and the request-scoped reads (as the database keystore does), no organisation credential store.
func fakeKeyStore() fx.Option {
	return fakeKeyStoreWith(&recordingKeyReads{keys: testutil.NewFakeSecretStore()})
}

// fakeKeyStoreWith is fakeKeyStore on reads' keystore.
func fakeKeyStoreWith(reads *recordingKeyReads) fx.Option {
	keys := reads.keys
	return fx.Provide(
		func() service.KeyStore { return keys },
		func() storageif.SecretFamilyLister { return keys },
		func() service.KeyReads { return reads },
		func() service.OrgKeyStore { return nil },
		func() service.OrgKeyReads { return nil },
	)
}

// recordingKeyReads is the keystore's request-scoped reads over the test keystore, counting the reads it serves.
type recordingKeyReads struct {
	keys  *testutil.FakeSecretStore
	calls atomic.Int32
}

func (r *recordingKeyReads) GetCtx(_ context.Context, scope, provider string) (string, error) {
	r.calls.Add(1)
	return r.keys.Get(scope, provider)
}

func (r *recordingKeyReads) HasCtx(_ context.Context, scope, provider string) (bool, error) {
	r.calls.Add(1)
	return r.keys.Has(scope, provider)
}

func (r *recordingKeyReads) KeyStates(_ context.Context, scope string, keys []string) (map[string]storageif.SecretState, error) {
	r.calls.Add(1)
	out := map[string]storageif.SecretState{}
	for _, k := range keys {
		if has, _ := r.keys.Has(scope, k); has {
			if v, _ := r.keys.Get(scope, k); v != "" {
				out[k] = storageif.SecretUsable
			} else {
				out[k] = storageif.SecretEmpty
			}
		}
	}
	return out, nil
}

// noDatabase provides what internal/app's provideStorageBackend provides beside the backend, as it does without a
// database: none of the database's own capabilities (the fake stands in for the backend's role interfaces only).
func noDatabase() fx.Option {
	return fx.Provide(
		func() storagedb.AzureRegistryProvider { return nil },
		func() service.BlobHealthChecker { return nil },
		func() reqtx.Opener { return nil },
		func() storageif.IdempotencyStore { return nil },
	)
}
