# PAD Analyzer

Parse, visualize, and analyze **Power Automate Desktop (PAD)** flow exports — with static security auditing, variable lineage, execution-graph visualization, AI-assisted review, and PDF/Markdown export.

The same React UI + Go backend run **two ways**:

- 🖥️ **Desktop app (Tauri)** — single-user, fully offline, secrets in the OS keyring. The easiest way to run it.
- 🌐 **In the browser (web server)** — multi-user, JWT login, PostgreSQL storage. For sharing flows with a team.

---

## Quick start

Both modes need **Node.js 20+** and **Go 1.23+**. Clone and install the frontend first:

```bash
git clone <repo-url> && cd baki
cd frontend && npm install && cd ..
```

Then pick a mode below.

---

## 🖥️ Run as a desktop app (Tauri)

**Extra prerequisites:** [Rust (stable)](https://rustup.rs) + Tauri system deps:

- **Windows** — [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/) (built into Win 11) + "Desktop development with C++" (VS Build Tools).
- **macOS** — `xcode-select --install`
- **Linux (Debian/Ubuntu)** — `sudo apt install libwebkit2gtk-4.1-dev libappindicator3-dev librsvg2-dev patchelf`

**Install the Tauri CLI (root deps) and run:**

```bash
npm install      # installs the Tauri CLI (root)
npm run dev      # build Go sidecar → start Vite → open the desktop window
```

`npm run dev` automatically:
1. Builds the Go backend into `src-tauri/bin/pad-backend-<target>` (the "sidecar").
2. Starts the Vite dev server on `http://localhost:5173` (with hot reload).
3. Opens the desktop window. Tauri spawns the sidecar on a random port with an ephemeral token — no login, no database.

> First run is slow (Cargo compiles Tauri). Later runs are incremental.

**Build installers for release:**

```bash
npm run build
```

Outputs to `src-tauri/target/release/bundle/` (`.msi`/`.exe` on Windows, `.dmg`/`.app` on macOS, `.AppImage`/`.deb` on Linux). Rebuild just the Go sidecar with `npm run build:sidecar`.

---

## 🌐 Run in the browser (web server)

Web mode runs the Go server standalone and serves the React app to a browser. It needs **PostgreSQL** (for users/auth/flow library) and JWT auth. **Rust/Tauri are not required.**

**1. Start PostgreSQL** (any instance works; Docker example):

```bash
docker run --name baki-pg -e POSTGRES_PASSWORD=baki -e POSTGRES_DB=baki \
  -p 5432:5432 -d postgres:16
```

Tables are created automatically on first connect.

---

## 🐳 Run with Docker (Full Stack)

The easiest way to run the entire stack (PostgreSQL + Backend + Frontend) in cloud mode is using Docker Compose:

```bash
docker-compose up --build
```

This will:
1. Build the React frontend.
2. Build the Go backend (including Swagger docs).
3. Start a PostgreSQL 16 database.
4. Launch the application at `http://localhost:8080`.

**Note:** The first registration becomes the admin account.

---

## 🖥️ Manual Setup (Go + Node)

**2. Start the Go backend**
 in cloud mode (see `.env.example` for all options):

```bash
PAD_MODE=cloud \
PAD_PORT=8080 \
PAD_AUTH_ENABLED=true \
PAD_AUTH_SECRET=$(openssl rand -hex 32) \
PAD_STORAGE=database \
PAD_DATABASE_URL='postgres://postgres:baki@localhost:5432/baki?sslmode=disable' \
PAD_ALLOWED_ORIGINS=http://localhost:5173 \
go run .
```

**3. Start the frontend** pointed at the backend (a separate terminal):

```bash
cd frontend
echo 'VITE_API_URL=http://localhost:8080' > .env.local   # bare origin, no /api suffix
npm run dev
```

**4. Open `http://localhost:5173`.** Register an account — **the first user becomes admin** — then log in. API keys and flows are stored server-side.

### Production web deploy

```bash
npm run build --prefix frontend     # bundles the static site to frontend/dist/
```

Serve `frontend/dist/` from any static host / reverse proxy, run the Go binary (`go build -o pad-analyzer . && ./pad-analyzer` with the same `PAD_*` env), and set `PAD_ALLOWED_ORIGINS` to your real web origin (e.g. `https://app.example.com`). Point the built frontend at the backend via `VITE_API_URL` at build time, or serve both behind one origin.

---

## Configuration

Backend env vars (the common ones; the complete reference with defaults is [docs/CONFIGURATION.md](docs/CONFIGURATION.md), and `.env.example` is annotated):

| Variable | Purpose | Default |
|---|---|---|
| `PAD_MODE` | `local` (Tauri sidecar) or `cloud` (web server) | `local` |
| `PAD_HOST` / `PAD_PORT` | Bind address / port (cloud mode) | `localhost` / ephemeral |
| `PAD_ALLOWED_ORIGINS` | Comma-separated CORS / WebSocket origin allowlist | — |
| `PAD_AUTH_ENABLED` | Enable JWT auth (required for browser use) | `false` |
| `PAD_AUTH_SECRET` | JWT signing secret (required when auth enabled) | — |
| `PAD_STORAGE` | `local` (no server-side store; desktop) or `database` (PostgreSQL) | `local` |
| `PAD_DATABASE_URL` | Postgres DSN (required when `PAD_STORAGE=database`) | — |
| `PAD_AZURE_STORAGE_ACCOUNT` / `PAD_AZURE_STORAGE_CONTAINER` | Offload flow content to Azure Blob Storage (both required together; database backend) | — (off) |
| `PAD_TRUSTED_PROXIES` | Trusted proxy IPs for `X-Forwarded-For` | — |
| `PAD_SCAN_INTERVAL` | Periodic flow re-scan interval (e.g. `1h`); enables drift/regression alerts (cloud) | — (off) |
| `PAD_NOTIFY_WEBHOOK_URL` | Generic webhook for governance alerts (raw JSON) | — |
| `PAD_NOTIFY_TEAMS_URL` | Microsoft Teams incoming-webhook URL for alerts | — |
| `PAD_SMTP_HOST` / `PAD_SMTP_PORT` | SMTP relay for transactional email (port 587 STARTTLS or 465 TLS) | — / `587` |
| `PAD_SMTP_USERNAME` / `PAD_SMTP_PASSWORD` | SMTP auth credentials | — |
| `PAD_EMAIL_FROM` | From address for outbound email (enables email when set with host) | — |
| `PAD_APP_BASE_URL` | Public origin used to build links in emails | — |

Frontend env var: `VITE_API_URL` — the backend origin (e.g. `http://localhost:8080`), **without** a trailing `/api`. Only needed in web mode; the desktop app discovers the sidecar automatically.

**Azure Blob Storage (optional).** When `PAD_AZURE_STORAGE_ACCOUNT` and `PAD_AZURE_STORAGE_CONTAINER` are both set, large flow content is stored in blob storage while Postgres keeps a placeholder + metadata. Auth uses `DefaultAzureCredential` (Managed Identity in Azure, `az` CLI locally); grant the identity the **Storage Blob Data Contributor** role on the account. The container must already exist — the server probes reachability at startup and **logs a warning but does not fail boot**; the readiness probe (`/readyz`) keeps the instance out of rotation until blob is reachable. Current content is keyed by the row's version (`flows/{id}/content.v{N}.json`), so a save whose transaction later rolls back leaves an unreferenced orphan blob rather than corrupting the live flow. Superseded content blobs are deleted automatically shortly after each successful save (and `DeleteFlow` removes the flow's whole prefix); version snapshots are capped at the 50 most recent per flow, with older snapshots (rows and blobs) pruned on each new snapshot. **Do not add an age-based storage lifecycle rule to the container**: it cannot distinguish a stale orphan from the live blob of a flow that simply hasn't been edited recently, and would silently destroy flow content. For the Azurite emulator or non-MI scenarios, set `PAD_AZURE_BLOB_CONNECTION_STRING` instead of the account name (Managed Identity remains the production default).

#### Azure Container Apps deployment (ops runbook)

1. **Identity & RBAC** — assign a (user-assigned) managed identity to the Container App and grant it **Storage Blob Data Contributor** on the storage account. For Postgres, set `PAD_DATABASE_URL` with `password=managed-identity` and grant the identity the AAD login role; the server injects a fresh Entra token per connection.
2. **Container** — the container must exist before the app starts; the app does not
   create it. The Bicep in `infra/` provisions it as **`flow-content`** and sets
   `PAD_AZURE_STORAGE_CONTAINER` to match, so on that path there is nothing to do.
   To create it by hand, use the same name — `flows/` is a key *prefix inside* the
   container, not a container name:
   ```bash
   az storage container create --account-name <acct> --name flow-content --auth-mode login
   ```
3. **Recoverability** — enable blob **soft-delete** and **versioning** on the account so an accidental/buggy delete is recoverable (the DB remains the source of truth):
   ```bash
   az storage account blob-service-properties update --account-name <acct> \
     --enable-delete-retention true --delete-retention-days 14 --enable-versioning true
   ```
4. **Lifecycle policy** — **do not add an age-based delete rule under `flows/`.** This
   step previously recommended one, on the premise that "the current `content.v{N}` is
   rewritten on every save". That premise is false: content is **version-keyed**
   (`flowContentKey` → `flows/{id}/content.v{N}.json`), so each save writes a *new*
   blob and the live one keeps the modification time of the last edit. A "delete blobs
   not modified in 30 days" rule therefore deletes the **live content of every flow
   nobody has edited in 30 days** — exactly the destruction the note under *Azure Blob
   Storage* above warns about, and which the code comment at
   `internal/storage/database/postgres_flows.go` states outright: such a rule "cannot
   tell a stale v(N-1) from the LIVE blob of a flow that simply hasn't been edited
   lately".

   Superseded blobs are already reclaimed by the application shortly after each
   successful save, and `DeleteFlow` removes the flow's whole prefix, so no age-based
   rule is needed. If you want a backstop for orphans left by a rolled-back
   transaction, it must key on something that distinguishes an orphan from a live
   blob — e.g. reconcile against the `flows` table's current version — not on age.

   Note the shipped Bicep also configures `tierToArchive` at 90 days after
   modification (`infra/main.bicep`). Archive-tier blobs cannot be read without
   rehydration, so on that IaC a flow untouched for 90 days becomes temporarily
   unreadable; narrow or remove that rule if you need every flow to stay hot.
 5. **Probes** — set the ACA `livenessProbe` to `GET /healthz` and `readinessProbe` to `GET /readyz` (the latter checks DB *and* blob reachability).
 6. **Observability** — alert on `pad_blob_operations_total{status="error"}` and `{status="throttled"}` (429s ⇒ throttling/scale), blob-op duration p99 (`pad_blob_operation_duration_seconds`), readiness failures, and `pad_blob_content_missing_total` (a flow's content blob was absent though its metadata recorded content — data loss; any increase warrants investigation).
 7. **Disaster recovery** — database backup/restore (PITR + geo-restore), RPO/RTO targets, a quarterly restore drill, and cutover steps are in [`docs/DR_RUNBOOK.md`](docs/DR_RUNBOOK.md). Provisioning a geo-redundant-backed-up Postgres via IaC is in [`infra/postgres.bicep`](infra/postgres.bicep).

AI provider keys are set in-app under **Settings → Providers** (`Ctrl + ,`). Desktop stores them in the OS keyring; web stores them server-side. Supported: GitHub Copilot (OAuth device flow or PAT), GitHub Models, Anthropic Claude, OpenAI, Google Gemini, xAI Grok, Zhipu GLM.

---

## How it works

```
        ┌─────────────────────────┐        ┌──────────────────────────┐
        │  React + Vite frontend  │  HTTP  │  Go backend              │
        │  (browser OR Tauri      │◄──────►│  REST API + SSE          │
        │   webview)              │  /SSE  │  parser · analyzer · AI  │
        └─────────────────────────┘        │  export · storage        │
                                           └──────────────────────────┘
   Desktop:  Tauri (Rust) hosts the webview and spawns the Go backend as a sidecar.
   Web:      the Go backend runs standalone; the browser loads the static frontend.
```

A **platform adapter** (`frontend/src/platform/`) abstracts everything platform-specific (file dialogs, clipboard, window controls, backend discovery), so the React code is shared verbatim between web and desktop. Tauri APIs are imported only inside the adapter (enforced by an ESLint rule).

---

## Features

- **Parsing** — single `.txt` exports or full flow folders (`Main.txt` + subflows); implicit subflow detection.
- **Static analysis** — 45 rules: hardcoded credentials, dead code, deep nesting, duplicate actions, empty error handlers, infinite loops, resource leaks, uninitialized variables, redundant actions, … with Error/Warning/Info severities and configurable thresholds.
- **Inline suppression** — silence a reviewed false-positive directly in the flow with a PAD comment placed immediately before the action: `# pad-ignore` (all rules on the next block) or `# pad-ignore[hardcoded-credential, deep-nesting]` (specific rules). Because it lives in the flow source it is honored everywhere — the app, `bakicli`, baselines, and CI gates — and suppressed counts surface in the report stats.
- **Visualization** — virtualized block view (react-virtuoso); variable lineage graph; execution-graph (DAG) view; flow tree sidebar with breadcrumbs.
- **AI review** — streaming chat over your flow with GitHub Copilot, Anthropic Claude, OpenAI, Google Gemini, xAI Grok, Zhipu GLM, or GitHub Models. Tool-augmented mode for autonomous analysis.
- **Export** — full analysis reports to PDF or Markdown (works in both desktop and browser); analysis history with regression diffing.
- **CI/CD** — headless `bakicli` runs static analysis and exits non-zero past a severity threshold (`-fail-on`) or a named, shareable **policy** (`-policy policy.json`), ratcheting on new findings only with `-baseline` or `-new-from-rev`. It emits `text`, `json`, **SARIF 2.1.0**, `junit`, `csv` or `html`; `-report-json` also writes the JSON report from the same run. A packaged **GitHub Action** uploads SARIF to the Security tab, writes a job summary and gates the build; GitLab and Azure DevOps templates live in `ci/`. See [CI/CD](#cicd).
- **Governance** *(web mode)* — persistent, team-shared finding triage (status/assignee/justification) and per-flow **baselines** so dashboards and CI can ratchet on *new* findings only; an org-wide **portfolio** view (Command Palette → "Flow Portfolio", or `GET /api/library/portfolio`) ranks every accessible flow worst-health-first; a periodic scanner re-analyzes stored flows and **alerts on drift or health regressions** via webhook / Microsoft Teams (`PAD_SCAN_INTERVAL` + `PAD_NOTIFY_*`).
- **Collaboration** *(web mode)* — organizations with role-based access (admin/member/viewer/guest); real-time presence indicators and block selection sync via WebSocket; flow sharing with per-flow collaborator permissions.
- **SSO** *(web mode)* — OIDC single sign-on with Microsoft Entra ID, Google, Okta, or any OIDC-compliant IdP. Account linking with automatic JIT provisioning.
- **Account email** *(web mode)* — transactional email for **password reset** (`/api/auth/forgot-password` → `/api/auth/reset-password`, single-use 1-hour token, all sessions revoked on reset), **email verification** (`/api/auth/verify-email`, sent on registration), and **org-invite delivery**. Configure an SMTP relay via `PAD_SMTP_*` + `PAD_EMAIL_FROM`; without it the app uses a log-only mailer so non-email deployments still work (links land in the server log). Reset endpoints never reveal whether an email exists.
- **Machine API tokens** *(web mode)* — scoped, revocable personal access tokens (managed under **Settings → API Tokens**, or `POST /api/auth/tokens`) so CI and automation can call the API as a user without an interactive login. Sent as `Authorization: Bearer pad_pat_…`; only the token hash is stored, the raw value is shown once, and the owner's current role applies (revoke = immediate).
- **Org invites** *(web mode)* — token-based email invites with configurable roles and expiry; pending invite management.
- **Optimistic concurrency** — version-tracked flow saves prevent silent overwrites; conflict detection with reload prompts.
- **Offline handling** — an offline banner, and the API client refuses saves while offline instead of queueing them (edits are not replayed on reconnect); cached data stays visible, and stale-response guards prevent out-of-order overwrites.
- **Global search** — fuzzy search across all subflows, block properties, and comments (`Ctrl+Shift+F`); command palette (`Ctrl/Cmd+K`).
- **Navigation** — breadcrumbs, go-to-definition for subflow calls (Ctrl+Click), navigation history (back/forward), block context menu (Explain with AI, Copy as Markdown, Find Usages, and Trace on blocks that produce an output variable).

---

## CI/CD

`bakicli` exit codes: `0` gate passed, `1` gate failed (findings at or above `-fail-on`, policy violated, or stale suppressions with `-fail-on-stale`), `2` usage, config, policy or parse error. Exit `2` produces no report, so do not upload the output file. `bakicli -h` lists every subcommand.

### GitHub Actions

The action (`action.yml`) downloads the checksum-verified `bakicli` release binary for the runner and builds it from source when no matching release asset exists. It runs the analysis **once**: the SARIF upload, the job summary and the pull-request comment all come from that run, so `rules`, `policy`, `baseline`, `new-from-rev`, `custom-rules` and `config` are reflected everywhere. Reference it by owner and ref, not `uses: ./` (a relative path resolves against the *calling* repository):

```yaml
permissions:
  contents: read
  security-events: write     # SARIF upload
  pull-requests: write       # PR summary comment
steps:
  - uses: actions/checkout@v4
  - uses: dusankuburic/baki@main   # pin a release tag once one is published
    with:
      flow-path: ./flows
      fail-on: error               # error | warning | info | none (report-only)
      # policy: ./pad-policy.json  # authoritative gate, overrides fail-on
      # baseline: ./pad-baseline.json
      # new-from-rev: origin/main  # needs fetch-depth: 0
      # custom-rules: ./rules.json
      # config: ./.bakirc.json
```

`.github/workflows/pad-analysis.yml` is a reusable workflow (`workflow_call`) that forwards every action input.

### Other CI systems

Both templates download the release binary (falling back to a source build), run one analysis that writes SARIF and JSON, publish the reports as artifacts and fail the job on the gate.

- **GitLab** — include `https://raw.githubusercontent.com/dusankuburic/baki/main/ci/.gitlab-ci.pad-analysis.yml` and extend `.pad-analysis`. The SARIF file is a plain job artifact. GitLab's `artifacts:reports:sast` expects its own schema and ignores SARIF, so the merge-request security widget stays empty.
- **Azure DevOps** — copy `ci/azure-pipelines.pad-analysis.yml` into your repository (or reference it through a GitHub repository resource) and use it as a step template.

Both headers list every variable or parameter. To record a run on a PAD Analyzer server's dashboard, use `bakicli report` (see [CI webhook](docs/CONFIGURATION.md#ci-webhook)).

## Tests

```bash
go test ./...                          # Go backend (parser, analyzer, API, storage, …)
go test -race ./...                    # with race detector
npm run gen:samples                    # regenerate the 10k-line synthetic benchmark flow (core/parser/testdata/samples/synthetic_10k.txt)

cd frontend
npm run test:run                       # Vitest (frontend unit/component tests)
npx tsc --noEmit                       # TypeScript type check
npm run lint                           # ESLint
```

---

## Project structure

```
baki/
├── main.go                # Go server entry point (reads PAD_* env / Tauri sidecar)
├── core/                  # pad-core module: engine code shared by the server and bakicli
│   ├── parser/            #   PAD export text parser (lexer + parser + tree builder)
│   ├── analyzer/          #   static-analysis rules, auto-fixers, suppression, policies
│   ├── export/            #   SARIF, JUnit, CSV, HTML, Markdown exporters
│   ├── ai/                #   context building and secret scrubbing for AI requests
│   └── models/            #   shared data types
├── internal/              # pad-analyzer module: server packages
│   ├── api/               #   HTTP router (chi), handlers, SSE events, middleware
│   ├── auth/              #   JWT, bcrypt, token blacklist, RBAC roles
│   ├── service/           #   FlowService, ChatService, AnalysisService, AuthzService
│   ├── collaboration/     #   Org CRUD, member management, invites
│   ├── ai/                #   AI provider abstraction (Claude, OpenAI, Gemini, Copilot…)
│   ├── websocket/         #   Hub/Client for real-time collaboration
│   ├── storage/           #   database (Postgres) backend; filesystem backend (migration source, tests)
│   ├── scanner/           #   periodic re-analysis, drift and regression alerts
│   ├── sso/               #   OIDC relying-party client for single sign-on
│   └── config/            #   config loading + validation
├── cmd/bakicli/           # headless CLI: analyze, gate, fix, diff, rules, suppressions, report
├── ci/                    # GitLab and Azure DevOps pipeline templates
├── infra/                 # Azure Container Apps deployment (Bicep)
├── frontend/              # React + TypeScript (Vite, Tailwind, Zustand)
│   └── src/
│       ├── api/           #   shared HTTP/SSE client + endpoint modules
│       ├── platform/      #   web vs Tauri adapter (the only place Tauri APIs are used)
│       ├── stores/        #   Zustand stores (flow, chat, auth, org, presence, sync…)
│       ├── services/      #   SyncManager (offline queue), CollaborationService (WS)
│       ├── components/    #   flow/, chat/, sidebar/, inspector/, search/, layout/
│       └── hooks/         #   useAppShortcuts, useFlowChangeSync, useStreamingMessage…
├── src-tauri/             # Tauri (Rust) desktop host + tauri.conf.json
└── scripts/build-sidecar/ # cross-platform Go sidecar build
```

---

## Troubleshooting

| Problem | Fix |
|---|---|
| `npm run dev` → "sidecar not found" | Run `npm run build:sidecar`, then retry. |
| Tauri window is blank | Vite wasn't ready yet — reload (`Ctrl + R`) or check port `5173` is free. |
| Browser shows 401 / can't log in | Ensure `PAD_AUTH_ENABLED=true`, `PAD_STORAGE=database`, and the DB is reachable; `VITE_API_URL` must match the backend origin. |
| Browser CORS errors | Add your frontend origin to `PAD_ALLOWED_ORIGINS`. |
| Windows Rust build: missing WebView2 | Install the [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/). |
| First `cargo`/`tauri` build is slow | One-time cost; Tauri crates compile once, then build incrementally. |
```
