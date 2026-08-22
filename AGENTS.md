# AGENTS.md — Build & Test Commands

## Quick Reference

```bash
# Backend (Go)
go build ./...                          # Build everything
go test ./...                           # Run all tests
go test -race ./core/analyzer/... ./internal/api/... ./internal/service/... ./internal/storage/...
golangci-lint run                       # Lint (must be 0 issues)

# Frontend (React/TS)
cd frontend
npx tsc --noEmit                        # Type check (must pass)
npm test                                # Vitest unit/component tests
npm run lint                            # ESLint (must be 0 errors)
npm run e2e                             # Playwright E2E (needs dev server)

# Full verification (run before declaring done)
go test ./... && go test -race ./core/analyzer/... ./internal/api/... ./internal/service/... && golangci-lint run && cd frontend && npx tsc --noEmit && npm test && npm run lint
```

## Project Structure

```
core/analyzer/     41 rules + 19 auto-fixers (18 distinct Patch fixers + "suppress") + patch model (5 op kinds: insert/wrap/append/replace/remove)
core/parser/       PAD text format parser (tokenizer → classifier → parser)
core/models/       Domain models (Block, Finding, Patch, AnalysisReport)
core/export/       SARIF 2.1.0 + report serializers
internal/api/      HTTP handlers (chi router), 170+ endpoints
internal/service/  Business logic (FlowService, AnalysisService, ChatService)
internal/storage/  StorageBackend interface + Postgres + filesystem implementations
internal/connector/padcloud/  PAD-cloud ingestion (auth + client + ingester + cloud-format Converter)
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
