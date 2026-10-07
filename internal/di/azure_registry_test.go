package di

import (
	"context"
	"testing"

	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/scanner"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// registryBackend is a storage backend that holds an Azure registry (the
// Postgres backend's capability) and takes the export hook.
type registryBackend struct {
	*testutil.FakeBackend
	registry storageif.AzureRegistryStore
	export   storageif.AzureExportConfigFunc
}

func (b *registryBackend) AzureRegistry() storageif.AzureRegistryStore { return b.registry }

func (b *registryBackend) SetAzureExportConfigSource(fn storageif.AzureExportConfigFunc) {
	b.export = fn
}

// Part 9: the registry lives in Postgres with auth on and the database
// backend (user ids real), else in the config directory's azure-models.json
// (desktop, database backend with auth off) — never settings.json — and in
// memory only where neither exists.
func TestProvideAzureRegistry_Selection(t *testing.T) {
	cfg := config.Default()
	dir := t.TempDir()

	reg, err := ProvideAzureRegistry(cfg, nil, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	file, ok := reg.(*storage.FileAzureRegistry)
	if !ok {
		t.Fatalf("desktop registry = %T, want the file store", reg)
	}
	if file.Path() != dir+`\azure-models.json` && file.Path() != dir+"/azure-models.json" {
		t.Errorf("file = %s", file.Path())
	}

	db := &registryBackend{FakeBackend: testutil.NewFakeBackend(), registry: storage.NewMemoryAzureRegistry()}
	if reg, _ := ProvideAzureRegistry(cfg, db, nil, dir); reg == db.registry {
		t.Error("auth off: the database registry must not be used (owner ids are not real users)")
	}
	cfg.Auth.Enabled = true
	if reg, _ := ProvideAzureRegistry(cfg, db, nil, dir); reg != db.registry {
		t.Errorf("auth on + database backend: registry = %T, want the backend's", reg)
	}
	if reg, _ := ProvideAzureRegistry(cfg, testutil.NewFakeBackend(), nil, ""); reg == nil {
		t.Error("no store at all must still give an (in-memory) registry")
	} else if _, isFile := reg.(*storage.FileAzureRegistry); isFile {
		t.Error("no config directory: no file store")
	}
}

// The service is per-user (and org-capable) exactly with the database
// registry, offers the knowledge-base designation where the knowledge base
// embeds with Azure, and installs the data-subject export's hook.
func TestProvideAzureModelService_Wiring(t *testing.T) {
	cfg := config.Default()
	desktop := ProvideAzureModelService(storage.NewMemoryAzureRegistry(), testutil.NewFakeSecretStore(), nil, nil, nil, nil, cfg)
	if desktop.OrgRegistriesAvailable() || desktop.KnowledgeEmbeddingEnabled() {
		t.Errorf("desktop: orgs %v, knowledge %v", desktop.OrgRegistriesAvailable(), desktop.KnowledgeEmbeddingEnabled())
	}

	cfg.Mode = config.ModeCloud
	cfg.Storage.EmbeddingProvider = "azure-foundry"
	backend := &registryBackend{FakeBackend: testutil.NewFakeBackend()}
	cloud := ProvideAzureModelService(storagedb.NewPostgresAzureRegistry(nil), testutil.NewFakeSecretStore(), testutil.NewFakeOrgSecretStore(), nil, nil, backend, cfg)
	if !cloud.OrgRegistriesAvailable() || !cloud.KnowledgeEmbeddingEnabled() {
		t.Errorf("cloud: orgs %v, knowledge %v", cloud.OrgRegistriesAvailable(), cloud.KnowledgeEmbeddingEnabled())
	}
	if backend.export == nil {
		t.Fatal("the export hook was not installed")
	}
	if got, err := backend.export(context.Background(), "u1", []string{"abcdefghij"}); err != nil || len(got) != 0 {
		t.Errorf("export of a missing credential = %v, %v", got, err)
	}

	// The knowledge service embeds through the registry when wired.
	ks := ProvideKnowledgeService(nil, nil, nil, cfg, cloud, nil)
	if !ks.AzureEmbedderEnabled() {
		t.Error("the knowledge service does not embed through the registry")
	}
	if ProvideKnowledgeService(nil, nil, nil, cfg, nil, nil).AzureEmbedderEnabled() {
		t.Error("no registry, no registry embedder")
	}
}

// The whole fx graph resolves with the registry in it: the registry, the
// service, and the services that take it (provider, system, knowledge),
// under the router.
func TestServiceModule_GraphWithAzureRegistry(t *testing.T) {
	err := fx.ValidateApp(
		fx.Provide(
			func() context.Context { return context.Background() },
			func() *config.Config { return config.Default() },
			func(cfg *config.Config) config.DeploymentMode { return cfg.Mode },
			func() storageif.StorageBackend { return nil },
			func() chan struct{} { return make(chan struct{}) },
			func(m *api.EventManager) service.EventNotifier { return m },
			func() *auth.Manager { return nil },
			func() *collaboration.OrgService { return nil },
			func() *notify.Dispatcher { return nil },
			func() *scanner.Scanner { return nil },
			func() *api.MigrationRunner { return nil },
			func() api.ScanNowFunc { return nil },
			func() service.KeyStore { return testutil.NewFakeSecretStore() },
			audit.Provide,
		),
		ServiceModule,
		APIModule,
		redisx.Module,
		fx.Invoke(func(*api.Router, *service.AzureModelService, *service.ProviderService, *service.SystemService, *rag.KnowledgeService) {
		}),
	)
	if err != nil {
		t.Fatalf("fx graph: %v", err)
	}
}
