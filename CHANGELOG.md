# Changelog

All notable changes to PAD Analyzer will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Feature completeness — controls that did nothing, and features with no way in

An audit traced every advertised feature through every layer (routes, storage,
rules, CLI, CI, settings, both runtime modes). What it found, and what changed:

**Cloud sign-up and sharing**

- Registration always ended in "Registration failed": the response was
  validated against the login schema, which requires tokens the register
  handler does not return. It now reports "check your inbox", or signs the
  first (bootstrap) admin straight in.
- An unverified account could never sign in on a deployment without SMTP:
  the verification mail was sent once, fire-and-forget, and there was no way
  to ask for another. `POST /api/auth/resend-verification` was added, the 403
  login state offers "Resend", and the bootstrap admin is created verified.
- A flow's owner could not manage its sharing: the permission check read the
  collaborator list, which never contains the owner, so the invite form and
  share-link section were hidden on every flow anyone created.
- Share links were created and then lost — the one-time token was discarded
  and nothing built a URL. The link is now shown once, with a copy button.
- Org invites return their token when email is not configured; the store
  dropped it. Pending invites now offer a copyable invite link.
- SSO login told existing users to "link <provider> from your account
  settings". No such flow existed. Account settings now has a Linked sign-in
  card over `POST /api/auth/sso/link/start`, `GET /api/auth/sso/links` and
  `DELETE /api/auth/sso/links/{provider}`; unlinking the last sign-in method
  is refused, and an identity already linked elsewhere is never re-pointed.
- Personal access tokens are scope-enforced, but the UI could not select or
  show scopes, so every token minted in the app was unscoped.

**Settings that changed nothing**

- "Custom instructions" had no reader on the server; the only consumer was a
  per-request field the client never set. The setting is now merged into the
  system prompt server-side. In cloud mode chat read the server's settings
  file rather than the signed-in user's own settings, so a user's custom
  instructions and history preference could not reach their chat at all.
- "Save conversation history" had no reader: turns were stored whichever way
  it was set. It now gates both the client's saves and the server's own
  start-of-stream persist, and the client sends history with each turn when
  storage is off.
- A cloud user who had never saved settings received an empty settings object,
  so every default of true ("Save conversation history", "Auto-analyze on
  open") showed as off. A missing settings row now returns the defaults.
- Removed: the Parser panel's "Preserve comments", "Treat tabs as spaces" and
  "Spaces per indent" (persisted, never read by the parser), and the
  `checkForUpdates`, `openInNewWindow`, `lastUsedVersion`, `codeFont` and
  `uiFont` fields (no UI, no consumer, no updater).

**Desktop app**

- Settings were write-only: the load waited for an authenticated session,
  which the desktop never has, so theme, density, provider, prompts and
  layout reset on every restart — and the welcome tour, which waits for
  settings, never appeared.
- The welcome tour could not be dismissed: Skip, X and Escape jumped to the
  last step, and only "Load sample" completed it. Its "Set one up" button
  opened a settings section that does not exist.
- Titlebar maximize and F11 fullscreen failed silently: the granted Tauri
  permissions did not match the calls the code makes.
- After three failed backend respawns the host emitted events nothing
  listened for, leaving a dead shell. A blocking banner now offers a restart
  through the `restart_backend` command that was already registered.
- The home "Findings" tile pointed at the portfolio, which panicked without a
  storage backend (500). It returns an empty portfolio, and the tile links
  only where there is a library.
- Cross-flow search returned a clean empty result on desktop, which reads as
  "no matches"; it now answers 503 like the other cloud-only routes.
- Hidden or labelled where the desktop has no backend for them: finding
  comments (the list was always empty and adding answered 503 after typing),
  the Sharing and History inspector tabs, the Cloud Library nav, re-import,
  reveal and reload file actions, and triage, which is session-only there.
- The source editor read and wrote `Main.txt` whichever file was selected, so
  editing a subflow file overwrote Main. It now edits the selected member of a
  folder flow, shows its name, and keeps the loaded file while edits are
  unsaved.
- Batch apply-fix on a folder flow patched `Main.txt` only and silently
  skipped findings in subflow files. It now patches every member file, with
  the per-file never-degrade gate and one undo snapshot.

**Features with no way in**

- "Find Usages" set the search query without running the search, leaving the
  sidebar in a loading state that never resolved.
- Apply fix on a `sensitive-exposure` finding was a silent no-op: the variable
  name was passed only for `init-variable`, so the fixer returned an empty
  patch, the preview showed identical text and a success toast fired. An empty
  patch is now refused as "fixer declined".
- Single apply-fix applied behaviour-changing fixers on a bare POST; the batch
  path and the CLI already required an opt-in. It now needs `unsafe: true`,
  which the UI sends only after showing the diff.
- Durable suppression (`# pad-ignore` written into the flow) had no caller,
  and the staleness audit had no route. The finding card's "Suppress in file"
  now writes the directive, and `POST /api/analysis/suppressions` backs a
  panel listing suppressions with stale ones marked.
- Suppressing a finding never asked for a reason (only hard-coded strings were
  stored) and stored reasons were never rendered. Suppression now prompts, and
  the triage chip shows the justification.
- The Policy Gate panel could create, save and delete policies but never
  evaluate one; the evaluate call had no caller. Each policy now evaluates
  against the open flow.
- Comment deletion, "leave organisation" and organisation rename were
  implemented on the server with nothing calling them. All three are now in
  the UI. Dismissed governance alerts could only be cleared within the same
  panel session; the list can now include them.
- The audit log offered 7 of 35 actions, with no user filter and no paging.
  Sensitive mutations emitted no audit event at all — share links, policies,
  comments, knowledge base, org create/update/delete, org rules and channels,
  scan-now, settings and key changes, exports and flow uploads now do.
- Baseline set and clear failures were unhandled promise rejections: nothing
  told the user the baseline had not changed.
- The Security Posture card rendered "No security issues detected" when its
  data source had failed or was stubbed, and the Postgres counts behind it
  swallowed per-query errors. Unavailable counts now render as unavailable.
- The rule catalog ignored an organisation's custom rules and overrides, so
  the reference and its counts did not describe the rules that run.
- The portfolio could not be scoped to an organisation or sorted; library
  flows could not be filtered by tag (and the API never returned tags);
  rename and duplicate ignored `canEdit`.
- WebSocket `error` frames (permission denied, unknown event) were dropped
  silently; they now surface as a toast.
- `errreport.CaptureError` had no production caller, so
  `pad_errors_reported_total` was always zero.
- The `settings:changed` event, the `disableSignUp` feature flag and four
  advertised chat shortcuts had no consumer. Alt+Left/Right now navigate
  flow history; the Tab shortcut was dropped rather than trap focus.
- Data-subject exports described themselves as everything stored about the
  user but omitted conversations, comments, triage decisions, organisation
  memberships and sessions, and capped audit history at 1000 rows silently.

**CLI, CI and reports**

- The reusable workflow pinned a release tag that does not exist, so every run
  failed to resolve it; both CI templates hard-failed when the release asset
  was missing. The workflow is pinned to a branch until a release is cut, and
  the templates fall back to a source build.
- The Azure template named a task that does not exist, and the GitLab
  template published SARIF as a GitLab SAST report, which ignores it.
- Job summaries and PR comments came from a second analysis run without the
  gate's flags, so their numbers disagreed with the gate. There is now one run.
- The policy verdict never left the terminal: it is now in the JSON report,
  SARIF run properties and the JUnit testsuite.
- SARIF locations were bare filenames, so folder imports produced no inline
  pull-request annotations — the stated benefit. Paths are now repo-relative,
  with rule help and automation details.
- `bakicli -h` printed only the analyze flags: no subcommands, no exit codes.
  `suppressions` ignored `.bakirc.json`, so `-fail-on-stale` could gate on a
  profile nobody configured. `fix --apply` has `-backup`. The new
  `bakicli report` signs the CI webhook that had no client.
- The periodic scanner had no cross-replica lock: every replica re-analysed
  every flow and delivered the same alert.

**Analyzer**

- `hardcoded-credential` never looked at property keys, so
  `Password: 'hunter2'` and a password inside a connection string went
  unreported while the matching auto-fix sat tested and unreachable.
- `resource-leak` checked six openers and ignored database, FTP and web
  automation connections its own description promises.
- `infinite-loop-risk` treated any property value containing "Exit" or
  "Break" as a loop exit.
- Document-level findings (parse errors) could not be suppressed inline;
  suppression now has file and subflow scope.
- The rule "dependencies" view was a hand-written list nothing enforced; it is
  now labelled as related rules.
- The health score formula and the custom-rule schema are documented.

**Deployment and documentation**

- The Azure template set no allowed origins, so the browser WebSocket upgrade
  was refused and presence and collaboration were dead on a shipped deploy.
- The template substituted the JWT secret for the encryption key, so the
  documented key separation never happened and the app's own warning never
  fired. `encryptionKey` is now a real deployment parameter.
- Compose published `PAD_PORT` without passing it to the container, so a
  custom port published a port nothing listened on. Cloud mode defaulted to
  debug logging against documented `info`.
- The admin migration panel read a status field the server never sends: no
  state was shown, a running migration was never polled, and Start stayed
  enabled where migration is not configured. The migration also now reports
  what it does not copy, and is described as the legacy filesystem-server
  upgrade it is.
- Corrected: `PAD_STORAGE=local` means no server-side store; the command
  palette is Ctrl/Cmd+K; the offline queue does not replay edits; the
  filesystem backend is a migration source and test double, not the desktop's
  store; the RLS middleware documents the routes and jobs that run without it;
  the OTLP exporter is HTTP; the auth rate limit is per minute; rule and
  endpoint counts; and the project tree lists `core/`, `cmd/`, `ci/` and
  `infra/`.

**Removed** (implemented with no caller, and no reason to add one): flow owner
transfer, the `cursor.move` collaboration event, conversation export, the
read-sources / open-from-system / server-side CSV / metrics / subflow-hashes /
rule-summary routes, the duplicate settings write routes and aliases, direct
org member add, single-org read, per-alert read marking, and four dead store
selectors and platform adapter methods. The desktop shell's URL-open
permission is now scoped to https.

### Production readiness — features that reported states they did not have

- `bakicli watch` analysed with default settings while loading `.bakirc.json`
  for everything else, so the per-rule `ruleConfig` profile was silently
  ignored in the local preview. It now runs the same profile `analyze` does.
- `.bakirc.json` had no field for rule thresholds (`maxDepth`, `minRepeats`,
  `maxBlocks`); a threshold in the file was dropped by the decoder and the rule
  kept its default. `ruleConfig.<rule>.options` is now honoured, `bakicli init`
  writes the defaults, and the org rule export carries them. Threshold rules
  accept int-typed options as well as JSON floats.
- The offline banner said "N changes queued" over a queue that holds
  cursor/presence updates only; edits made offline were never saved. It now
  says saves are disabled, and the API client refuses mutations while offline
  with the same message instead of failing after retries.
- The library detail panel rendered "No saved versions yet." when the versions
  call failed (desktop mode returns 503). It shows the error; on desktop the
  section is hidden.
- An older binary against a newer schema booted clean and logged "schema up to
  date" with its own version. Migration now refuses a schema newer than the
  binary knows, and logs the database's version.
- Admin "scan now" ran without a panic guard; a panic there took the process
  down. It now recovers, logs, and counts under
  `pad_panics_total{location="admin.scan-now"}`.
- Handler panics produced no access-log line and no `http_requests_total{5xx}`
  sample (Recovery sat outside AccessLog and Metrics). An inner Recovery now
  wraps the router.
- Nothing bounded a runaway query: `statement_timeout` and `lock_timeout` are
  set on every pooled connection (`PAD_DB_STATEMENT_TIMEOUT`, default 30s;
  migrations lift it for their own session). `*/stream` routes get a 15-minute
  outer deadline instead of none; `/ws` and `/api/events` stay unbounded.
- Concurrent chat streams each passed the same daily-budget read (usage rows
  land after the turn). Active streams now publish spend-so-far plus one
  reserved response, and every check counts the other streams'.
- Desktop hid nothing that did not work: the AI daily-budget setting (no usage
  store, never enforced) and the audit section (no backend) are hidden in
  local mode. Cloud apply-fix on a folder import is disabled with the reason
  instead of failing after the click.
- JUnit: info findings now carry the `<system-out>` body the format promised,
  and testcase names are unique per finding (`@fingerprint`) so N findings no
  longer collapse into one flapping test.
- `path-traversal-risk` keyed on property names PAD never emits and could not
  fire; it now matches the parser's real keys (File, Files, FolderPath, …).
  `Scripting.*` (PowerShell/VBScript/Python/JavaScript) is covered by
  `command-injection-risk`. The "Taint Paths" panel is labelled as the
  name-based hint it is.
- Shutdown: the audit flush is bounded (5s), the retention purge runs on the
  shared `TickerLoop` and drains on stop, and the HTTP drain no longer consumes
  the whole 25s budget.
- Access log lines carry `trace_id` when a span is present.
- Removed the experimental Dataverse / PAD-cloud connector
  (`internal/connector/padcloud`, the `/api/admin/powerplatform/*` and
  `/api/admin/ingester/ingest` routes, `PAD_PP_*` config, the admin
  "Ingest cloud flows" button). It was self-declared unvalidated against a real
  tenant and its device-code auth had no UI; it lives in git history.
- CI/infra: govulncheck and npm audit are advisory; the Bicep sets registry
  pull credentials (was `registries: []`), a `PAD_METRICS_TOKEN`, and defaults
  `provisionRedis` to false (single replica; a Redis blip was a hard boot
  failure); two log-query alerts (ERROR burst, readiness failing); `latest` is
  only tagged after the image passes the vulnerability scan; the reusable
  workflow and the GitLab/Azure DevOps templates use a released binary instead
  of building from the consumer's checkout; a Windows desktop build workflow.

### Cleanup — growth and honesty items

- Retention purge now also removes expired share links and ages out governance
  alerts (with `PAD_AUDIT_RETENTION_DAYS`), and runs by default every 24h in
  cloud mode (`PAD_RETENTION_PURGE_INTERVAL`, `0s` to disable). Conversations,
  knowledge documents and comments are user content and are never purged by age.
- Collaborator and knowledge-document listings apply their limit in SQL instead
  of fetching the whole set and slicing.
- `POST /api/system/log-error` is bounded at 64 KiB (413 above) and each field
  is truncated in the log; it used to accept 10 MiB into the ERROR log.
- `pad_chat_tool_results_total{tool}` is a closed label set: unregistered tool
  names the model invents collapse to `other`.
- Rule thresholds (`maxDepth`, `minRepeats`, `maxBlocks`) are editable in the
  Rules settings panel at deployment and org scope; the catalog reports them.
- `docs/CONFIGURATION.md` documents every environment variable with its
  default; forty of them were documented nowhere.
- Removed the Key Vault resource and loader path: nothing ever provisioned a
  secret into it, so every boot did six lookups, six 404s, six warnings, and
  used the Container App secrets anyway. Secrets are Container App secrets.
- Removed the demo AI provider (unreachable in every shipped build — its proxy
  URL was set only in tests — with a live rate limiter, endpoint and UI button
  attached to nothing) and the batch HTML report (no route, no consumer).
- The collaborator-list API doc says viewer access, which is what it enforces.

### Fixed — Unbounded fan-out

- The governance scanner built a new HTTP client (and connection pool, and DNS
  lookup) for every alert on every channel, and launched an unbounded detached
  goroutine per delivery that shutdown neither waited for nor cancelled. Notifiers
  are now built once per org per sweep, deliveries are capped at 32 concurrent,
  and Stop drains in-flight ones within 5s instead of dropping them.
- WebSocket connections had no inbound message rate limit. Each message is
  amplified into one marshal per room member plus a Redis publish every replica
  decodes, so a flooding collaborator caused room-sized CPU amplification. A
  per-connection token bucket (200/s, burst 512) now sheds before that work.

### Fixed — Background sweeps could crash or stall the server

- The PAD-cloud ingester guarded its incremental-sync map with a mutex declared
  inside the sweep function, so it protected nothing between sweeps. Triggering
  "ingest now" while the periodic sweep was running was a concurrent map
  read+write — a fatal error that `recover()` cannot catch, taking the process
  down. Overlapping sweeps are now refused outright rather than duplicating every
  Dataverse call and store write.
- Background loops (`governance scanner`, ingester) did not wait for an in-flight
  tick on shutdown, so the database pool could close underneath a running sweep.
  Stop now drains, bounded by 5s.
- Dataverse pagination followed `@odata.nextLink` to any host with the tenant
  access token attached, and had no page limit. Off-origin and plaintext hops are
  refused, page/flow counts are bounded, and the token is re-read per page so a
  long enumeration cannot fail halfway on an expired one.

### Security — SSRF via org notification channels (cloud mode)

- Any authenticated user can create an org and administer it, so a notification
  channel URL is tenant-supplied. The dial guard omitted loopback entirely and
  the `http://localhost` dev carve-out was not gated on deployment mode, so a
  channel could point at `127.0.0.1` and the synchronous channel-test endpoint
  would report whether the connection was refused, non-2xx or 2xx — a loopback
  port scanner, and a way to POST attacker-influenced JSON at internal services.
  Cloud deployments now refuse loopback, private, link-local and unspecified
  targets at both validation and dial time. Desktop/local keeps the localhost
  hatch, which is what makes local webhooks useful there.
- Notification deliveries no longer follow redirects. Only the configured URL was
  ever validated, and 307/308 replay the method and body, so an external endpoint
  could steer a signed governance payload to an address the operator never set.

### Security — Parser hardening against adversarial flow files

- A single long token in a flow file made the property scanners quadratic: they
  re-scanned each run of key characters on every byte instead of skipping it. At
  the 2 MB `/api/analysis/analyze-raw` limit that was tens of minutes of pinned
  CPU per request, which the handler timeout could not interrupt. Now linear;
  parsing behaviour is unchanged for valid input.
- Nested `ON BLOCK ERROR` bypassed the 200-level nesting cap — it was the only
  block-opening construct that did not check it — so a crafted upload could build
  a block tree thousands deep and drive every recursive walker to that depth.

### Fixed — Verification gates

- The documented test command (`AGENTS.md`) never ran the `core` module. This is a
  two-module `go.work` workspace and `./...` resolves within one module, so
  `core/parser`, `core/export`, `core/models`, `core/search`, `core/ai/scrubber`,
  `core/cache` and `core/logger` were silently skipped by anyone following the
  documented workflow. CI was always correct; the docs had drifted.
- `golangci-lint` was failing on 8 pre-existing `gosec` findings surfaced by newer
  rules — CI installs `version: latest`, so its lint job was already red. None were
  real defects, but the suppressions had rotted: one `#nosec` was attached to the
  wrong statement (twice), another named a rule that does not apply to its finding.
  All now sit on the reported line with the correct rule and a stated reason.
- `go build`/`go vet`/lint were processing a third-party Go package vendored inside
  `frontend/node_modules`; Go does not skip `node_modules` the way it skips `vendor`.
- `sqlclosecheck` and `rowserrcheck` are now enforced (both were already clean).

### Security — Org settings are no longer broadcast across tenants

- `POST /api/system/settings/org/{id}` emitted its `settings:changed` event to
  every connected SSE client in the deployment, so one org's rule profile
  (which rules it disabled, at what severity) reached other tenants' users. It
  is now delivered to that org's owner and members only.
- `scripts/db-init` takes the application role's password from
  `BAKI_APP_PASSWORD` instead of hardcoding `baki_app`. A compose-based
  production stack initialised from an empty volume previously came up with a
  credential published in this repository. **Existing deployments are not
  affected by the change and are not fixed by it** — rotate with
  `ALTER ROLE baki_app PASSWORD '<new>'` and update `PAD_DATABASE_URL`.

### Fixed

- `bakicli watch` validated `-fail-on` before merging `.bakirc.json`, so an
  invalid `failOn` in the config slipped through and silently fell back to
  error-gating — the local preview then showed a different gate than the one
  configured.

### Added

- Dependabot now covers the `core` Go module, which was never receiving
  dependency updates.

### Fixed — Auto-fix ran on a configuration nobody chose

- `bakicli fix` now reads `.bakirc.json` (new `-config` and `-custom-rules`
  flags, matching `analyze`). It previously ignored the config entirely, so a
  rule disabled in `ruleConfig` was not reported by `analyze` yet was still
  auto-fixed — the tool rewrote flows to resolve findings a team had switched
  off — and a custom rule carrying an `autoFix` was reported but never applied.
  This also restores the `GET /orgs/{id}/rules/export` → CI parity promise: the
  exported profile now governs fixing as well as gating.
- The server's batch apply-fix (`POST /api/flow/apply-fix-batch`) resolves the
  same per-org rule profile the analysis path resolves, so "fix all" can no
  longer apply a fix for a rule the org disabled, and org custom rules with an
  `autoFix` are applied. The UI was already safe (it sends the selected
  findings' rule ids); a direct API call with no `rules` was not.

### Security — Source maps are no longer published

- The production image shipped `dist/**/*.map` (~9.6 MB of a 14 MB bundle) with
  `sourcesContent` populated, and the static handler served anything under the
  static dir — so appending `.map` to a bundle URL returned the application's
  complete original TypeScript. vite's `sourcemap: 'hidden'` only omits the
  `//# sourceMappingURL` comment; it does not prevent a direct fetch. The
  handler now refuses `.map` requests regardless of what is on disk, and the
  Dockerfile ships a stripped `dist`.

### Security — Dependency updates

- `google.golang.org/grpc` 1.81.1 → 1.82.1 (GO-2026-6061) and
  `golang.org/x/text` 0.37.0 → 0.39.0 (GO-2026-5970, infinite loop on invalid
  input). Both were reachable from application code, so `govulncheck` — and
  therefore the CI gate that blocks deploys — was failing.

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
- 11 new rules: `parse-error`, `circular-subflow-dependency`, `hardcoded-ip`,
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
- In-app Rule Reference page (Ctrl/Cmd+K → "Rule Reference")
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
