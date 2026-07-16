# Contributing to PAD Analyzer

Thank you for your interest in contributing! This guide covers the basics.

## Development Setup

### Prerequisites
- **Go 1.25+** — backend
- **Node 20+** — frontend
- **Rust + Tauri CLI** (optional) — desktop build

### Getting Started

```bash
# Clone and build the backend
git clone <repo-url> && cd baki
go build ./...

# Run the Go tests
go test ./...

# Frontend
cd frontend
npm install
npm test          # Vitest unit/component tests
npm run lint      # ESLint
npx tsc --noEmit  # TypeScript check

# Full verification (run before PRs)
go test ./... && go test -race ./core/analyzer/... ./internal/api/... ./internal/service/... && golangci-lint run
cd frontend && npx tsc --noEmit && npm test && npm run lint
```

### Project Structure

```
core/analyzer/     Rules, auto-fixers, patch model, metrics
core/parser/       PAD text format parser
core/models/       Domain models
core/export/       SARIF, HTML, PDF, Markdown, JUnit, CSV exporters
internal/api/      HTTP handlers (chi router)
internal/service/  Business logic
internal/storage/  StorageBackend interface + Postgres + filesystem
cmd/bakicli/       CLI tool
frontend/src/      React 18 + TypeScript + Zustand + Tailwind
```

## Adding a New Analyzer Rule

1. Create `core/analyzer/rule_your_rule.go` implementing the `Rule` interface:
   ```go
   type YourRule struct{}
   func (r *YourRule) ID() string   { return "your-rule" }
   func (r *YourRule) Name() string { return "Human name" }
   func (r *YourRule) Description() string { return "What it checks" }
   func (r *YourRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
   func (r *YourRule) Category() string { return "Reliability" }
   func (r *YourRule) Check(block *models.Block, ctx *RuleContext) []models.Finding { ... }
   func init() { registerRule(&YourRule{}) }
   ```

2. Add an entry to `DefaultSettings()` in `core/models/settings.go`.

3. (Optional) Add an auto-fixer: create a patch function in `autofix.go`, add a
   case in `fix_dispatch.go`, and register the fixType in `ruleAutoFix` in
   `engine.go`.

4. Write a round-trip gate test (see `autofix_test.go` for the pattern).

## Adding a Frontend Component

1. Follow existing patterns in `frontend/src/components/`.
2. Use Zustand stores for state (see `stores/`).
3. Add tests in `src/**/*.test.tsx`.
4. Use the shared components (`Button`, `Input`, `Modal`, `Toast`, `useConfirm`).

## Pull Request Checklist

- [ ] `go test ./...` passes
- [ ] `go test -race ./core/analyzer/... ./internal/api/... ./internal/service/...` passes
- [ ] `golangci-lint run` reports 0 issues
- [ ] `npx tsc --noEmit` passes
- [ ] `npm test` passes
- [ ] `npm run lint` reports 0 errors
- [ ] No secrets or credentials committed

## Code Style

- Go: `gofmt` + `golangci-lint` are authoritative
- Frontend: ESLint + Prettier, functional components with hooks
- No commented-out code
- Tests alongside source files
