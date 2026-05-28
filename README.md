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

Backend env vars (see `.env.example`):

| Variable | Purpose | Default |
|---|---|---|
| `PAD_MODE` | `local` (Tauri sidecar) or `cloud` (web server) | `local` |
| `PAD_HOST` / `PAD_PORT` | Bind address / port (cloud mode) | `localhost` / ephemeral |
| `PAD_ALLOWED_ORIGINS` | Comma-separated CORS / WebSocket origin allowlist | — |
| `PAD_AUTH_ENABLED` | Enable JWT auth (required for browser use) | `false` |
| `PAD_AUTH_SECRET` | JWT signing secret (required when auth enabled) | — |
| `PAD_STORAGE` | `local` (filesystem) or `database` (PostgreSQL) | `local` |
| `PAD_DATABASE_URL` | Postgres DSN (required when `PAD_STORAGE=database`) | — |
| `PAD_TRUSTED_PROXIES` | Trusted proxy IPs for `X-Forwarded-For` | — |

Frontend env var: `VITE_API_URL` — the backend origin (e.g. `http://localhost:8080`), **without** a trailing `/api`. Only needed in web mode; the desktop app discovers the sidecar automatically.

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
- **Static analysis** — 10+ rules: hardcoded credentials, dead code, deep nesting, duplicate actions, empty error handlers, infinite loops, … with Error/Warning/Info severities.
- **Visualization** — variable lineage across subflows; execution-graph (DAG) view.
- **AI review** — chat over your flow with the provider of your choice.
- **Export** — full analysis reports to PDF or Markdown (works in both desktop and browser).

---

## Tests

```bash
go test ./...                          # Go backend (parser, analyzer, API, storage, …)
go test -race ./...                    # with race detector

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
├── internal/              # Go backend packages
│   ├── api/               #   HTTP router + handlers (auth, library, sharing, org, …)
│   ├── parser/ analyzer/  #   PAD parsing + static-analysis rules
│   ├── ai/ export/        #   AI providers + PDF/Markdown export
│   ├── storage/           #   filesystem (local) + database (Postgres) backends
│   └── config/ manager/   #   config loading + app orchestration
├── frontend/              # React + TypeScript (Vite, Tailwind, Zustand)
│   └── src/
│       ├── api/           #   shared HTTP/SSE client (client.ts) + endpoint modules
│       ├── platform/      #   web vs Tauri adapter (the only place Tauri APIs are used)
│       ├── components/ hooks/ stores/
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
