# Baki: working notes for contributors and coding agents

Baki is a static analyzer and AI assistant for Power Automate Desktop flows. It has three parts:

- **Go backend:** the root module, `main.go` and `internal/*`, wired with fx and chi, on Postgres with RLS and
  optional Redis.
- **`core`:** the parser, analyzer and export, in the separate `pad-core` module.
- **React frontend** (`frontend/`) and a **Tauri** desktop shell (`src-tauri/`, which runs the backend as a
  sidecar in local mode).

Read [docs/architecture/README.md](docs/architecture/README.md) before changing backend structure. The
refactoring roadmap is
[docs/architecture/backend-refactoring-proposal.md](docs/architecture/backend-refactoring-proposal.md), and
decisions are recorded in [docs/adr/](docs/adr/).

## Commands

```
go build ./... && go vet ./... ./core/...
go test ./... ./core/...        # CI adds -race and a Postgres DATABASE_URL
golangci-lint run ./...
cd frontend && npm test -- --run && npm run lint && npm run typecheck   # e2e: npm run e2e
```

Postgres-backed tests skip without `DATABASE_URL`. Point it at a throwaway container, never the live compose
database; the recipe is in [docs/architecture/safety-nets.md](docs/architecture/safety-nets.md).

## Rules the tests enforce

- Layering, transactions, direct storage calls in handlers, `os.Getenv` outside `internal/config`, and caches
  built without a `cache.Declare` spec are ratcheted in `internal/archtest`. Don't add exceptions; lower the pinned
  numbers when you remove one.
- Migrations from v32 on are `.sql` files in `internal/storage/database/migrations/`, each with a
  `-- baki:compat expand|breaking` line. Never edit a shipped step.
- Ask `capability.Of(cfg)` what a deployment can do instead of checking `cfg.Mode` or a nil backend.

## Goldens: refresh only for intended changes, and review the diff

```
go test ./internal/api/ -run TestRoutePolicy_Golden -update-route-policy
go test ./internal/api/ -run TestRoutePolicyDeclared_Golden -update-route-policy-declared
go test ./internal/api/ -run TestAuthzMatrix_Golden -update-authz-matrix
go test ./internal/storage/database/ -run TestMigrationChecksums_Golden -update-migration-checksums
go test ./internal/di/ -run TestCacheRegistry_Golden -update-caches
go test ./core/analyzer/ -run TestFindingsGolden -update-findings
go test ./core/export/ -run TestExportGolden -update-export-golden   # also after analyzer output changes
```

## Windows checkouts

The tree is checked out CRLF (`core.autocrlf=true`), so `gofmt -l` lists every file. Check changed files on LF
copies instead (`tr -d '\r' < f | gofmt -l`), and preserve CRLF when editing existing files. `.sql` and
`.golden` files are LF by `.gitattributes`.
