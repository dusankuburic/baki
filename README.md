# PAD Analyzer

Parse, visualize, and analyze **Power Automate Desktop (PAD)** flow exports — static security auditing, variable lineage, execution graphs, AI-assisted review, PDF/Markdown export.

One React UI + Go backend, two ways to run:

- 🖥️ **Desktop (Tauri)** — single-user, fully offline, secrets in the OS keyring.
- 🌐 **Web server** — multi-user, JWT login, PostgreSQL, for teams.

## Quick start

Needs **Node.js 20+**, **Go 1.23+**: `git clone <repo-url> && cd baki && (cd frontend && npm install)`

### Desktop (Tauri)

Extra prerequisites: [Rust](https://rustup.rs) + WebView2 & VS C++ Build Tools (Windows) / `xcode-select --install` (macOS) / `libwebkit2gtk-4.1-dev libappindicator3-dev librsvg2-dev patchelf` (Linux).

```bash
npm install      # Tauri CLI (root)
npm run dev      # build Go sidecar → start Vite → open the desktop window
npm run build    # installers in src-tauri/target/release/bundle/ (first build is slow: Cargo)
```

### Web server

Easy path: `docker-compose up --build` → `http://localhost:8080`. By hand — PostgreSQL with a role of its own (cloud mode refuses a superuser/`BYPASSRLS` role, which would make row-level security inert; `vector` is optional, for server-side knowledge search):

```bash
docker run -d --name baki-pg -e POSTGRES_PASSWORD=postgres -p 127.0.0.1:5432:5432 pgvector/pgvector:pg16
docker exec baki-pg psql -U postgres \
  -c "CREATE ROLE baki_app LOGIN PASSWORD 'baki_app' NOSUPERUSER NOBYPASSRLS;" \
  -c "CREATE DATABASE baki OWNER baki_app;"
docker exec baki-pg psql -U postgres -d baki -c "CREATE EXTENSION IF NOT EXISTS vector;"
```

Local dev is plain HTTP, which cloud mode refuses by default: `PAD_DB_REQUIRE_SSL=false` accepts the TLS-less database and `PAD_BEHIND_PROXY=true` the browser's plain connection. In production serve TLS and use `sslmode=verify-full` for a database across a network ([TLS to the database](docs/architecture/operations.md#tls-to-the-database)).

```bash
PAD_MODE=cloud PAD_PORT=8080 PAD_AUTH_ENABLED=true PAD_STORAGE=database \
PAD_AUTH_SECRET=$(openssl rand -hex 32) \
PAD_DATABASE_URL='postgres://baki_app:baki_app@localhost:5432/baki?sslmode=disable' \
PAD_DB_REQUIRE_SSL=false PAD_BEHIND_PROXY=true \
PAD_ALLOWED_ORIGINS=http://localhost:5173 go run .

cd frontend && echo 'VITE_API_URL=http://localhost:8080' > .env.local && npm run dev
```

Open `http://localhost:5173` and register — **the first user becomes admin**. Production, key rotation, Azure runbook: [docs/deployment.md](docs/deployment.md).

## Configuration

Every variable with its default: [`.env.example`](.env.example). Full reference: [docs/deployment.md](docs/deployment.md). Essentials: `PAD_MODE` (`local`/`cloud`) · `PAD_HOST`/`PAD_PORT` · `PAD_ALLOWED_ORIGINS` · `PAD_AUTH_ENABLED` + `PAD_AUTH_SECRET` · `PAD_STORAGE=database` + `PAD_DATABASE_URL` · `PAD_TRUSTED_PROXIES`. Frontend: `VITE_API_URL` (backend origin, no `/api`; web mode only).

## Features

- **Parsing** — `.txt` exports or full flow folders; implicit subflow detection.
- **Static analysis** — 45 rules + JSON custom rules + inline `# pad-ignore` suppression: [docs/custom-rules.md](docs/custom-rules.md).
- **Visualization** — virtualized block view, variable lineage, execution graph.
- **AI review** — streaming chat with Claude, OpenAI, Gemini, Copilot, Grok, GLM, GitHub Models, Azure AI Foundry: [docs/ai-foundry.md](docs/ai-foundry.md).
- **Export** — PDF/Markdown reports; history with regression diffing.
- **CI/CD** — headless `bakicli` (severity/policy gates, baselines, SARIF/JUnit/CSV/HTML) + a GitHub Action.
- **Web mode** — orgs with RBAC, triage & baselines, drift alerts, channels, real-time collaboration, SSO, API tokens, email.

## CI/CD

`bakicli` exit codes: `0` passed, `1` gate failed, `2` usage/config/parse error (no report). Flags go before paths.

```yaml
steps:
  - uses: actions/checkout@v4
  - uses: dusankuburic/baki@main   # pin a release tag; needs security-events + pull-requests write
    with: {flow-path: ./flows, fail-on: error}   # error | warning | info | none
```

GitLab / Azure DevOps templates in [`ci/`](ci/). `bakicli report` records a run on a server (org CI credential or CI-scoped API token — [docs/deployment.md](docs/deployment.md#github-secrets-for-the-deploy-workflow)).

## Tests

```bash
go test ./...               # Go backend
cd frontend && npm run test:run && npx tsc --noEmit && npm run lint
```

## Structure

`main.go` server entry (app graph in `internal/app`) · `core/` pad-core (parser, analyzer, export) · `internal/` server packages (api, auth, service, storage, ai, …) · `cmd/bakicli/` headless CLI · `frontend/` React + TS · `src-tauri/` desktop host · `infra/` Azure Bicep · `ci/` pipeline templates.

## Docs

[Deployment & operations](docs/deployment.md) · [Azure AI Foundry accounts](docs/ai-foundry.md) · [Architecture](docs/architecture/) (ADRs, conventions, operations) · [Custom rules](docs/custom-rules.md) · [Webhooks](docs/webhooks.md) · [Testing](docs/testing.md)

## Troubleshooting

| Problem | Fix |
|---|---|
| "sidecar not found" | `npm run build:sidecar`, retry. |
| Browser 401 | `PAD_AUTH_ENABLED=true`, `PAD_STORAGE=database`, `VITE_API_URL` matches backend origin. |
| CORS errors | Add the frontend origin to `PAD_ALLOWED_ORIGINS`. |
| First Tauri build slow | One-time Cargo cost. |
