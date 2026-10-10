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

// databaseRegistry stands in for the database's Azure registry provider: the
// registry tables and the data-subject export's hook.
type databaseRegistry struct {
	registry storageif.AzureRegistryStore
	export   storageif.AzureExportConfigFunc
}

func (d *databaseRegistry) AzureRegistry() storageif.AzureRegistryStore { return d.registry }

func (d *databaseRegistry) SetAzureExportConfigSource(fn storageif.AzureExportConfigFunc) {
	d.export = fn
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

	db := &databaseRegistry{registry: storage.NewMemoryAzureRegistry()}
	if reg, _ := ProvideAzureRegistry(cfg, db, nil, dir); reg == db.registry {
		t.Error("auth off: the database registry must not be used (owner ids are not real users)")
	}
	cfg.Auth.Enabled = true
	if reg, _ := ProvideAzureRegistry(cfg, db, nil, dir); reg != db.registry {
		t.Errorf("auth on + database: registry = %T, want the database's", reg)
	}
	if reg, _ := ProvideAzureRegistry(cfg, nil, nil, ""); reg == nil {
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
	desktop := ProvideAzureModelService(storage.NewMemoryAzureRegistry(), testutil.NewFakeSecretStore(), nil, nil, nil, nil, nil, nil, cfg)
	if desktop.OrgRegistriesAvailable() || desktop.KnowledgeEmbeddingEnabled() {
		t.Errorf("desktop: orgs %v, knowledge %v", desktop.OrgRegistriesAvailable(), desktop.KnowledgeEmbeddingEnabled())
	}

	db := &databaseRegistry{}
	authOff := ProvideAzureModelService(storage.NewMemoryAzureRegistry(), testutil.NewFakeSecretStore(), nil, nil, nil, nil, testutil.NewFakeBackend(), db, cfg)
	if authOff.OrgRegistriesAvailable() || db.export != nil {
		t.Errorf("database with auth off: orgs %v, export hook installed %v; want the file registry's single owner", authOff.OrgRegistriesAvailable(), db.export != nil)
	}

	// The knowledge-base designation needs the organisations' shared corpus: a server store with accounts, in the
	// cloud or in the local development profile alike.
	cfg.Storage.Backend = config.StorageDatabase
	cfg.Auth.Enabled = true
	cfg.Storage.EmbeddingProvider = "azure-foundry"
	keys := testutil.NewFakeSecretStore()
	localDB := ProvideAzureModelService(storagedb.NewPostgresAzureRegistry(nil), keys, keys, testutil.NewFakeOrgSecretStore(), nil, nil, testutil.NewFakeBackend(), db, cfg)
	if !localDB.OrgRegistriesAvailable() || !localDB.KnowledgeEmbeddingEnabled() {
		t.Errorf("local with a database and accounts: orgs %v, knowledge %v", localDB.OrgRegistriesAvailable(), localDB.KnowledgeEmbeddingEnabled())
	}
	cfg.Mode = config.ModeCloud
	cloud := ProvideAzureModelService(storagedb.NewPostgresAzureRegistry(nil), keys, keys, testutil.NewFakeOrgSecretStore(), nil, nil, testutil.NewFakeBackend(), db, cfg)
	if !cloud.OrgRegistriesAvailable() || !cloud.KnowledgeEmbeddingEnabled() {
		t.Errorf("cloud: orgs %v, knowledge %v", cloud.OrgRegistriesAvailable(), cloud.KnowledgeEmbeddingEnabled())
	}
	if db.export == nil {
		t.Fatal("the export hook was not installed")
	}
	if got, err := db.export(context.Background(), "u1", []string{"abcdefghij"}); err != nil || len(got) != 0 {
		t.Errorf("export of a missing credential = %v, %v", got, err)
	}

	// The knowledge service embeds through the registry when wired.
	ks := ProvideKnowledgeService(nil, nil, nil, cfg, cloud, nil, nil, nil)
	if !ks.AzureEmbedderEnabled() {
		t.Error("the knowledge service does not embed through the registry")
	}
	if ProvideKnowledgeService(nil, nil, nil, cfg, nil, nil, nil, nil).AzureEmbedderEnabled() {
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
			audit.Provide,
		),
		noDatabase(),
		fakeKeyStore(),
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
