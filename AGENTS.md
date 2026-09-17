# AGENTS.md — Build & Test Commands

## Quick Reference

```bash
# Backend (Go)
#
# BOTH module patterns, always. This is a go.work workspace with two modules:
# the root `pad-analyzer` and `core/` = `pad-core`. `./...` resolves within the
# current module only, so `go test ./...` alone runs ZERO core packages —
# core/parser, core/export, core/models, core/search, core/ai/scrubber,
# core/cache and core/logger are silently not run. Verify with:
#   go list ./... | grep -c '^pad-core'   # 0
# This file used to say `go test ./...  # Run all tests`, which meant anyone
# following it — human or agent — got a green they had not earned. CI has
# always used both patterns; the documented workflow had drifted from it.
go build ./... ./core/...               # Build everything
go test ./... ./core/...                # Run all tests
go test -race ./... ./core/...          # Race detector (what CI runs)
golangci-lint run                       # Lint (must be 0 issues)

# Frontend (React/TS)
cd frontend
npx tsc --noEmit                        # Type check (must pass)
npm test                                # Vitest unit/component tests
npm run lint                            # ESLint (must be 0 errors)
npm run e2e                             # Playwright E2E (needs dev server)

# Full verification (run before declaring done)
#
# DATABASE_URL / AZURITE_CONNECTION_STRING gate the Postgres and blob suites —
# without them ~170 storage tests SKIP rather than fail, so an unset variable
# looks identical to a pass. See "Integration test harness" below.
go build ./... ./core/... && \
go vet ./... ./core/... && \
go test -race -count=1 ./... ./core/... && \
test -z "$(gofmt -l . | grep -v node_modules)" && \
golangci-lint run && \
cd frontend && npx tsc --noEmit && npm test && npm run lint
```

## Integration test harness

The DB- and blob-gated suites skip silently when their environment variables are
unset. A green `go test` with them unset is weak evidence — it is the state in
which the worst storage bugs have historically hidden. Bring both up first:

```bash
podman run -d --name baki-pg -e POSTGRES_PASSWORD=postgres -e POSTGRES_USER=postgres \
  -e POSTGRES_DB=baki -p 55432:5432 docker.io/pgvector/pgvector:pg16
# NOT the postgres superuser: superusers and BYPASSRLS roles ignore every RLS
# policy, so every negative RLS assertion would skip. This mirrors CI.
podman exec -e PGPASSWORD=postgres baki-pg psql -U postgres -d postgres \
  -c "CREATE ROLE baki_app LOGIN PASSWORD 'baki_app' NOSUPERUSER NOBYPASSRLS;" \
  -c "CREATE DATABASE baki_app OWNER baki_app;"
podman exec -e PGPASSWORD=postgres baki-pg psql -U postgres -d baki_app \
  -c "CREATE EXTENSION IF NOT EXISTS vector;"   # untrusted ext, needs superuser
podman run -d --name baki-azurite -p 10000:10000 \
  mcr.microsoft.com/azure-storage/azurite \
  azurite-blob --blobHost 0.0.0.0 --blobPort 10000 --skipApiVersionCheck

export DATABASE_URL="postgres://baki_app:baki_app@localhost:55432/baki_app?sslmode=disable"
export AZURITE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"
```

## Project Structure

```
core/analyzer/     45 rules + 19 auto-fixers (18 distinct Patch fixers + "suppress") + patch model (5 op kinds: insert/wrap/append/replace/remove)
core/parser/       PAD text format parser (tokenizer → classifier → parser)
core/models/       Domain models (Block, Finding, Patch, AnalysisReport)
core/export/       SARIF 2.1.0 + report serializers
internal/api/      HTTP handlers (chi router), ~190 endpoints over ~175 paths
internal/service/  Business logic (FlowService, AnalysisService, ChatService)
internal/storage/  StorageBackend interface + Postgres + filesystem implementations
cmd/bakicli/       CLI with -format sarif/json/text, -baseline, -policy gates
frontend/src/      React 18 + TypeScript + Zustand + Tailwind + Tauri
ci/                Azure DevOps + GitLab CI templates
action.yml         GitHub Action (composite)
```

## Testing Conventions

- **Go**: standard `testing` + table-driven tests. Round-trip gate tests for fixers prove faithful (re-parse succeeds) + effective (finding resolved).
- **Frontend**: Vitest + jsdom + React Testing Library. All tests in `src/**/*.test.ts(x)`.
- **E2E**: Playwright in `frontend/e2e/`. Run separately (`npm run e2e`).
- **Backend integration**: `internal/api/*_test.go` uses a real chi router + fake backend (`testutil.FakeBackend`).

## Key Patterns

- **Apply-fix loop**: `FlowService.ApplyFix` dispatches on `fixType` → `analyzer.*Patch(block)` → `analyzer.ApplyPatch(source, patch)` → re-parse → re-analyze.
- **Round-trip gate**: every fixer has a test that parses source → analyzes → applies fix → re-parses → re-analyzes → asserts finding is gone.
- **Storage**: single `StorageBackend` interface, two impls (Postgres + filesystem). Test stubs embed `testutil.FakeBackend`.
- **Frontend state**: 14 Zustand stores, coordinated by `storeRegistry` (logout teardown). Settings persist via `/api/system/settings`.
- **Suppression**: source-level `# pad-ignore[rule]` (travels with the file) vs. UI-level (in-memory + cloud triage).

## Never Commit

- `.env`, credentials, API keys
- `frontend/node_modules/`
- Binary artifacts (`bakicli`, `*.exe`)
