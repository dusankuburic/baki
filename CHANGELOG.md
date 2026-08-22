# Changelog

All notable changes to PAD Analyzer will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added — Frontend tests & hygiene (Phase C)
- Component tests for the previously untested core editor surface:
  `BlockCard` (keyboard interaction, aria-pressed state, context menu,
  conditional AI actions), `MainPaneToolbar` (export/compare/reimport happy
  and failure paths, busy states), `OrganizationsPanel` (create org, invite
  with role, role change, member removal incl. confirm/cancel), and
  `useCytoscapeGraph` (instance lifecycle + destroy, element loading,
  subflow-neighborhood filtering, error surfacing, stale-response race
  guard, search highlight counts — via a cytoscape mock with chainable
  collections)
- E2E coverage for the previously untested critical flows: real login form
  submission, logout (session clear + login return), refresh-failure boot,
  share-link happy path (valid token renders report, invalid token errors,
  no authenticated chrome), and the chat send path (composer → thread
  streaming state) with SSE envelope mocks
- Type-aware `@typescript-eslint/no-floating-promises` lint rule enabled
  (63 latent violations fixed: fire-and-forget calls now awaited, void-ed,
  or caught — including the WebAdapter notification permission rejection)
- `flow:loaded` SSE payloads now boundary-validated with a zod envelope
  schema before entering the editor store (malformed events dropped +
  logged instead of hijacking the editor); regression-tested
- Removed dead code: the never-imported `components/sharing/` directory,
  unused `mapSet`/`mapDelete`/`mapUpdate` helpers, and the
  `findBlockInTree` export; eliminated the remaining non-null assertions
  and the stale `useAutoAnalyze` cast
- E2E harness: service workers blocked (SW-initiated fetches bypass
  `page.route`, silently poisoning stubs with vite's HTML fallback) and the
  shared helper serves a full settings stub so first-run onboarding can't
  overlay spec interactions


### Improved — Frontend accessibility (Phase B)
- `shared/Input` + `shared/Textarea`: error/hint text now linked to the field
  (generated id + `aria-describedby`) and invalid fields carry `aria-invalid`
  — lifts every form in the app at once (WCAG 3.3.1/3.3.2)
- Global `prefers-reduced-motion` rule neutralizes decorative keyframes and
  transitions for users who opt out of animation (WCAG 2.3.3)
- `shared/Dropdown`: trigger announces `aria-haspopup`/`aria-expanded`,
  arrow keys rove real DOM focus (Home/End supported), and closing restores
  focus to the trigger instead of dropping it
- Default dark theme contrast: `--text-tertiary` 3.5:1 → 5.2:1 and the
  placeholder token 2.2:1 → 3.2:1 on surface-2 (AA for small text)
- Labels associated (`htmlFor`/`id` or `aria-label`) on the placeholder-only
  inputs in auth and settings; AI behavior switches carry accessible names
- Change-password form: inline field error with `role="alert"` + live
  mismatch checking replaces the vanishing toast

### Fixed — Frontend startup performance (Phase A)
- **Charts chunk (472 kB recharts) was eagerly loaded**: a static barrel
  re-export of MetricsTab linked it into the entry graph, and rollup had also
  parked shared modules (react-dom/client, scheduler, clsx) inside the charts
  chunk, so the entry statically imported it regardless. Fixed by removing the
  barrel re-export and switching `manualChunks` to function form that claims
  the react ecosystem first
- **Graph chunk (529 kB cytoscape) was eagerly loaded**: the default
  inspector tab's DetailsTab statically imported the lineage graph;
  it is now lazy and mounts only when lineage data exists
- Eager JS payload: ~1.66 MB → **692 kB** (index 548 + react-vendor 144);
  the entry chunk now statically imports only react-vendor
- markdown family co-located with its sole importer (AITab chunk now
  320 kB total vs the previous 285 + 586 kB split fetched at tab-open)
- Rare chat overlays (context-preview modal, help popover) lazy;
  removed the circular AITab barrel self-export
- Settings panels load per-section (cloud-only panels never reach desktop);
  settings modal spinner applies to panel switches
- JetBrains Mono converted TTF → WOFF2 (544 kB → 187 kB) and its
  preload dropped; Tailwind content scanning excludes test files;
  analysis-progress SSE writes quantized to integer-percent/rule changes


### Added — Frontend i18n (Phase 3, foundation + first waves)
- i18next + react-i18next scaffold: synchronous bundled-resource init (no
  Suspense required), typed keys via `CustomTypeOptions` augmentation (a
  typo'd key is a compile error), namespaced resources (`common`, `shell`,
  `findings`, `auth`) with conventions documented in `src/i18n/en.ts`
  (concept-named keys, exact-match English for test continuity, `_one`/
  `_other` plurals, no mixed leaf/nesting levels — the type-flattener
  silently drops those)
- Language handling: `navigator.language` detection with `en` fallback,
  persistence via `pad-language`, `<html lang>` kept in sync, `setLanguage`
  helper ready for a future settings UI
- Wave 1 (shell): system nav labels, toolbar labels and export/compare/
  reimport toasts, drop overlay
- Wave 2 (findings + auth): search placeholder, empty states, severity
  badges, selection-bar strings, "{{count}} finding(s)" pluralization,
  health-score aria label, login/register/forgot-password forms
- Vitest setup initializes the real i18n (English resources) so existing
  string assertions pass unchanged; 6 scaffold unit tests cover init,
  interpolation, pluralization, persistence, html-lang sync, and fallback


### Improved — Frontend (Phase 2 performance)
- Chat message list virtualized (`react-virtuoso`, matching the findings/
  block lists): long threads now mount only visible bubbles instead of every
  MessageBubble (react-markdown + Prism each). Open-at-bottom, follow-on-
  append, streaming auto-scroll with user-scroll respect, scroll-to-bottom
  button, and `role="log"`/`aria-live` semantics all preserved; the list
  remounts per thread so switching opens at the new thread's bottom
- Tauri window controls (`@tauri-apps/api/window`, which also pulls dpi.js/
  image.js) dynamically imported inside `TauriAdapter` — previously ~70 kB
  of Tauri-only code shipped in the eagerly-loaded web bundle where those
  controls never run
- Command palette, global search overlay, and settings modal now mount only
  when opened, so their lazy chunks fetch on first open (a user gesture)
  instead of at startup — `React.lazy` fetches on first render, so
  always-rendering a closed modal defeated the existing SettingsModal split
- Eager `index` chunk: 528 kB → 504 kB minified (141 kB gzip)

### Improved — Frontend (Phase 1 polish)
- Findings search debounced (150ms): typing no longer re-runs the
  filter/regroup/reflatten pipeline per keystroke (mirrors the library
  workspace's debounce pattern); regression-tested
- AITab block context resolved via the O(1) `buildBlockLookup` index
  (extended with `rawType`) instead of a full block-tree DFS on every
  selection change
- Keyboard accessibility: block cards and library list rows are now
  focusable (`role="button"`, `tabIndex`, Enter/Space select, Enter opens,
  `focus-visible` rings) — previously mouse-only
- Health scores announce their band (`Good/Fair/Poor/Critical`) to screen
  readers via `aria-label` in FindingsSummary, MetricsTab, and the analytics
  Avg Health card (was color-only)
- FindingsList bulk-fix visibility derived from subscribed store state
  instead of render-time `getState()` reads that could go stale
- Settings/admin/profile panels converted from whole-store destructuring to
  atomic Zustand selectors (9 components)
- Lazy SettingsModal now shows a spinner fallback instead of a dead click on
  slow networks

### Fixed — Security
- **Service worker no longer persists API responses** (`no-store` violation):
  the SW cached `/api/*` GETs into Cache Storage despite the backend stamping
  every API response `Cache-Control: no-store, private`. Cached auth/me,
  flows, findings, and chat data survived logout on a shared device. API
  requests are now network-only (offline → the existing 503 JSON error);
  removing the API cache also purges any already-cached sensitive responses
  on upgrade via the cache-version bump
- **Logout purges service-worker caches**: the app posts `PURGE_CACHES` to
  the worker on logout so shell/offline artifacts don't outlive a session on
  a shared device (regression-tested in authStore.test.ts)

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

### Added — Backend / OpenAPI
- Rebuilt the swagger docs from real source annotations: every chi route
  (162 paths) now carries `@Router`/`@Summary`/params/responses on its
  handler — previously the committed docs were hand-maintained and only
  covered 99 of 165 registered endpoints
- Reconciled path drift: annotations use the registered route paths
  (`{id}`/trailing-slash mismatches normalized away); dangling `$ref`s to
  nonexistent definitions replaced with inline object schemas
- Restored the general API info block (`@title`/`@description`) on `main.go`
  that the stale docs had lost
- CI "OpenAPI Freshness" gate: regenerates `docs/` via
  `swag init -g main.go -o docs --parseDependencyLevel 1` and fails on drift,
  so the spec can never silently rot again
- `/api/events` (SSE) and `/ws` documented (text/event-stream, 101 upgrade)

### Added — Export
- JUnit XML export format (`bakicli -format junit`)
- CSV export format (`bakicli -format csv`)
- HTML report export format (`bakicli -format html`) — self-contained styled
  report (inline CSS, dark-mode aware) with severity summary cards and
  per-finding detail; all finding content HTML-escaped
- In-app HTML export: `/api/export/html` joins the markdown/PDF export family
  (base64 response, desktop file-write, cloud-mode path drop) and an
  "Export HTML" toolbar button; findings-view export and CLI share the same
  `export.ReportToHTML` renderer (health-score badge, category, light/dark)
- Retired the duplicate `/api/analysis/export/html` endpoint and
  `analyzer.GenerateHTMLReport` in favor of the unified renderer

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

### Added — Web integration / PWA
- Service worker rewrite: offline fallback page for navigations, cache-first
  for immutable `/assets/*`, split shell/assets/API caches with versioning,
  and SSE streams (`/api/events`) are never intercepted/cached
- `/offline.html` — themed, dependency-free offline page precached at install
- Share links unfurl properly: the backend injects the flow name into
  `<title>` + `og:`/`twitter:` meta on `/shared?token=…` (best-effort;
  invalid tokens serve the plain shell)
- PWA metadata: manifest `id`/`lang`/`scope`/`categories`, app shortcuts
  (Library, Analytics), separate any/maskable icons, iOS home-screen meta,
  and static `og:`/`twitter:`/description tags on the shell
- `?view=<system-view>` deep links honored on boot (powers the PWA shortcuts)

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
