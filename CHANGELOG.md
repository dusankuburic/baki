# Changelog

All notable changes to PAD Analyzer will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added — Analyzer
- 8 new rules: `parse-error`, `circular-subflow-dependency`, `hardcoded-ip`,
  `high-cyclomatic-complexity`, `uncalled-subflow`, `duplicate-subflow-name`,
  `duplicate-label`, `switch-no-default`, `empty-subflow`, `todo-in-comment`,
  `wait-zero` (40 total)
- 9 new auto-fixers: `hardcoded-filepath`, `hardcoded-url`, `dead-data`,
  `empty-branch`, `slow-pattern`, `hardcoded-ip`, `sensitive-exposure`,
  `switch-no-default`, `empty-subflow`, `uncalled-subflow`, `wait-zero`,
  `subflow-no-error-handler` (30 total with auto-fix)
- `insert-delay-in-loop` fixer variant for `slow-pattern`
- `insert-default` fixer for `switch-no-default`
- `append-output` fixer for `subflow-mismatch` (uncaptured-output pattern)
- `mask-sensitive-variable` fixer for `sensitive-exposure`
- `CustomRule.autoFix` field for user-defined rules with validated fixTypes
- Case-insensitive Wait/Delay detection in `slow-pattern` rule
- Parse-error fallback emits findings even when no blocks are parseable

### Added — Export
- JUnit XML export format (`bakicli -format junit`)
- CSV export format (`bakicli -format csv`)

### Added — CLI
- `bakicli rules` / `bakicli rules <rule-id>` — list or describe all rules
- `bakicli --version` / `-v` — print version (inject via ldflags)
- `bakicli init` — generate a starter `.bakirc.json` config file
- `.bakirc.json` config file support (auto-discovered; CLI flags override)
- `-format` validation (rejects unknown format values)
- Multi-file and folder support in `bakicli fix` (recursive `.txt`/`.pad` walk)
- `fix` subcommand accepts multiple file arguments

### Added — Frontend
- In-app Rule Reference page (Ctrl+P → "Rule Reference")
- Onboarding tour replay from Settings → General
- `showSourceEditor` / `showHelpOverlay` moved to uiStore (survive navigation)

### Fixed — Analyzer
- `slow-pattern` rule now detects uppercase `WAIT` actions (case-insensitive)

### Fixed — Frontend
- Removed dead settings from GeneralPanel (`checkForUpdates`, `openInNewWindow`)
- Replaced `window.confirm`/`window.prompt` with styled `useConfirm()` dialogs
  in HistoryTab, FindingsToolbar, and AccountDataCard
- Deleted orphaned chat components (`DemoModeBanner`, `ModelSelector`, `ProviderSelector`)

### Fixed — Infra
- GitLab CI template: fixed build path (`$CI_PROJECT_DIR`), Go version (1.25),
  and SARIF report type (`reports.sast` instead of `reports.codequality`)
- Removed `.gitignore` block that prevented committing `.md` files
