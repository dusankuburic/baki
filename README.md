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

For a self-hosted box, the compose production overlay is the supported path. It
adds a Caddy TLS terminator, stops publishing the app's own port, and makes
every credential mandatory:

```bash
cat > .env <<'ENV'
BAKI_SITE_ADDRESS=https://baki.example.com   # public origin; port 80 must be reachable for ACME
ACME_EMAIL=ops@example.com                   # certificate expiry notices
PAD_ALLOWED_ORIGINS=https://baki.example.com
PAD_AUTH_SECRET=...                          # openssl rand -hex 32
PAD_ENCRYPTION_KEY=...                       # a DIFFERENT openssl rand -hex 32
PAD_METRICS_TOKEN=...                        # openssl rand -hex 32
PAD_DATABASE_URL=postgres://baki_app:...@db:5432/baki?sslmode=require
BAKI_APP_PASSWORD=...                        # must match the password in PAD_DATABASE_URL
ENV

docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

Caddy obtains and renews the certificate automatically and redirects `http://`
to `https://`. Only ports 80 and 443 are published; the app and the database are
reachable only on the compose network.

Two settings in that overlay are load-bearing rather than cosmetic:

- **`PAD_BEHIND_PROXY=true`** is the documented escape hatch from the cloud-mode
  check that otherwise refuses to start auth without TLS. It is only honest
  because something in the stack actually terminates TLS. Do not set it with
  nothing in front.
- **`PAD_TRUSTED_PROXIES`** is pinned to Caddy's single static address. The app
  honours `X-Forwarded-For` only from a listed proxy, so with an empty list
  every client collapses into one rate-limit bucket (1 rps general, 5 logins a
  minute for the whole deployment) and audit entries record the proxy instead of
  the user. Widening it to the private ranges would let any other container on
  the network declare its own client IP.

`/metrics` and `/debug/pprof` are 404'd at the edge; scrape them from inside the
compose network (`http://app:8080/metrics`) with `PAD_METRICS_TOKEN`.

**Terminating TLS somewhere else** (a cloud load balancer, an existing nginx)?
Then do not use this overlay's proxy. Run the app behind your own front end and
set `PAD_TRUSTED_PROXIES` to that front end's address yourself — the default in
the overlay is Caddy's address and will not match.

**Building the frontend separately** is still possible: `npm run build --prefix
frontend` bundles the static site to `frontend/dist/` for any static host. Set
`PAD_ALLOWED_ORIGINS` to your real web origin and point the build at the backend
with `VITE_API_URL`. Leaving `VITE_API_URL` unset makes the bundle use its own
origin at runtime, which is what the container image does — one image works at
any hostname.

---

## Configuration

Backend env vars (the common ones; `.env.example` is annotated and lists every variable with its default):

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

The `infra/` Bicep provisions the whole stack. Deploy it in three steps, because
each one produces an input the next needs.

**1. Container registry** — once.

```bash
az group create -n rg-baki-prod -l westeurope
az deployment group create -g rg-baki-prod -f infra/registry.bicep \
  --parameters registryName=bakiprod
```

The Container App pulls with its managed identity (`AcrPull`, granted by
`infra/main.bicep`), so no registry username or password is stored anywhere.
That is why the Azure path uses ACR rather than GHCR: managed identity cannot
authenticate to GHCR, so ACA would need a long-lived classic PAT present at
every restart and scale event. GHCR is still published for self-hosters using
`docker-compose.prod.yml`.

**2. PostgreSQL** — once.

```bash
az deployment group create -g rg-baki-prod -f infra/postgres.bicep \
  --parameters serverName=baki-pg adminPassword="$(openssl rand -base64 24)"
```

Two settings in that template are load-bearing, not hardening niceties:

- **`allowAzureServices`** adds the firewall rule that lets the Container Apps
  environment connect. A Flexible Server with `publicNetworkAccess: Enabled`
  and no firewall rules accepts *no* connections at all.
- **`allowedExtensions`** (`azure.extensions`) allow-lists `PG_TRGM` and
  `VECTOR`. Azure rejects `CREATE EXTENSION` for anything not on that list, and
  schema migration v13 runs an unguarded `CREATE EXTENSION IF NOT EXISTS
  pg_trgm` — so without it the app fails its migrations and never finishes
  booting. (`vector` is exception-guarded and merely degrades knowledge search
  to the in-Go ranker.)

Then run the one-time SQL bootstrap documented at the bottom of
`infra/postgres.bicep`: create the dedicated `baki_app` role
(`NOSUPERUSER NOBYPASSRLS`, owning the database and the `public` schema) and
`CREATE EXTENSION vector` as the administrator. **The app must not connect as
the administrator** — superusers and `BYPASSRLS` roles ignore every row-level
security policy, and migrations need to own their tables for
`ALTER TABLE … FORCE ROW LEVEL SECURITY` to apply. The server warns at boot if
it detects an RLS-bypassing role.

**3. The app** — every deploy.

```bash
az deployment group create -g rg-baki-prod -f infra/main.bicep \
  --parameters @infra/main.parameters.json \
  --parameters image=bakiprod.azurecr.io/baki-backend@sha256:... \
               authSecret=... databaseUrl=...
```

`.github/workflows/deploy.yml` does this automatically, pinning the image by
digest after it has passed the CI gate, the Trivy scan and SBOM generation, then
smoke-testing `/readyz` and rolling back to the previous image if that fails.

What the template sets up, and what still needs a human:

1. **Identity & RBAC** — a user-assigned managed identity gets
   **Storage Blob Data Contributor** on the storage account and **AcrPull** on
   the registry. Both role assignments are scoped to the resource, not the
   resource group. On a bring-your-own storage account (`storageAccountResourceId`)
   the template cannot create the role assignment — it may be in another
   resource group — so grant it yourself.
   Postgres uses password auth by default. The code also supports Entra tokens
   (`PAD_DATABASE_URL` with `password=managed-identity`, see
   `internal/storage/database/azure.go`); if you switch to it, give the app its
   own non-admin principal rather than making its identity the server
   administrator.
2. **Blob container** — provisioned as **`flow-content`**, with
   `PAD_AZURE_STORAGE_CONTAINER` set to match. `flows/` is a key *prefix inside*
   the container, not a container name. To create it by hand:
   ```bash
   az storage container create --account-name <acct> --name flow-content --auth-mode login
   ```
3. **Recoverability** — blob soft-delete, container soft-delete, versioning,
   change feed and point-in-time restore are provisioned as code on the
   `blobServices/default` resource (`blobRetentionDays`, default 14). Verify
   after a deploy:
   ```bash
   az storage account blob-service-properties show --account-name <acct> \
     --query "{versioning:isVersioningEnabled, softDelete:deleteRetentionPolicy, pitr:restorePolicy}"
   ```
4. **Lifecycle policy** — **there is none, and do not add an age-based rule
   under `flows/`.** Content is version-keyed
   (`flowContentKey` → `flows/{id}/content.v{N}.json`), so each save writes a
   *new* blob and the live one keeps the modification time of the last edit. A
   "delete blobs not modified in 30 days" rule therefore deletes the **live
   content of every flow nobody has edited in 30 days** — the destruction the
   note under *Azure Blob Storage* above warns about, and which the code comment
   in `internal/storage/database/postgres_flows.go` states outright: such a rule
   "cannot tell a stale v(N-1) from the LIVE blob of a flow that simply hasn't
   been edited lately".

   An earlier version of this template tiered blobs to **Archive** at 90 days,
   which has the same shape of bug: archive-tier blobs cannot be read without
   rehydration, so any flow untouched for a quarter became unreadable. That rule
   has been removed.

   Superseded blobs are already reclaimed by the application shortly after each
   successful save, and `DeleteFlow` removes the flow's whole prefix. If you want
   a backstop for orphans left by a rolled-back transaction, it must key on
   something that distinguishes an orphan from a live blob — e.g. reconcile
   against the `flows` table's current version — not on age.
5. **Probes** — `livenessProbe` is `GET /healthz` (unconditional 200; it
   deliberately does not touch the database, so a slow DB cannot restart the
   container) and `readinessProbe` is `GET /readyz` (Postgres, blob and, when
   configured, Redis). The readiness failure threshold is generous because a
   cold start runs schema migrations before it can serve.
6. **Client IP** — `trustedProxies` is set, and it matters. With it empty the
   app falls back to the immediate TCP peer, which behind the ACA ingress is the
   ingress itself: every client then shares **one** per-IP rate-limit bucket
   (1 rps general, 5 logins/minute for the entire deployment) and every audit
   entry records the proxy address. After the first deploy, check a real
   access-log line's `remote_ip` and narrow the CIDR list.
7. **Observability** — an Application Insights instance is provisioned and the
   environment's managed OpenTelemetry agent forwards traces to it; the app
   picks the endpoint up from the injected `OTEL_EXPORTER_OTLP_ENDPOINT` with no
   extra configuration. Structured logs go to Log Analytics, with two KQL alerts
   (ERROR burst, readiness failing) and a Container App restart alert — all four
   require `alertEmails`. The ~40 Prometheus metrics at `/metrics` are still
   scraped by nothing: alert on `pad_blob_operations_total{status="error"}` and
   `{status="throttled"}`, blob-op duration p99
   (`pad_blob_operation_duration_seconds`), and `pad_blob_content_missing_total`
   (a flow's content blob was absent though its metadata recorded content — data
   loss; any increase warrants investigation) once a scrape pipeline exists.
   Set `metricsToken`: `PAD_BEHIND_PROXY=true` makes the endpoint's private-IP
   allowlist pass for every request, so the token is the only real gate.
8. **Disaster recovery** — `infra/postgres.bicep` provisions geo-redundant
   automated backups with a pinned retention window, which is the PITR range;
   restore with `az postgres flexible-server restore` (or `geo-restore` for a
   regional outage) and repoint `PAD_DATABASE_URL` at the new server. **Restore
   blob storage too**: superseded content blobs are hard-deleted about a minute
   after each save, so every flow saved after the restore point has a row
   pointing at a blob that is gone — recover those from blob soft-delete
   (`az storage blob undelete`) in the same window. `/readyz` returning 200 does
   not prove this: it checks reachability, not that any particular flow's
   content still exists. Watch `pad_blob_content_missing_total`.

> **Request timeout.** The ACA ingress caps every HTTP request at 240 s
> (WebSocket and SSE connections excepted) with no configuration knob. A
> knowledge-base re-index is chunked around it: it stops itself well inside
> the cap and answers with a `nextCursor` the client resumes from — converted
> pages are never re-bought. An upload is not: it embeds the whole document
> (up to 2 500 chunks) in one request, so a large document on a low-TPM
> embedding deployment can outlast the cap — the client gets a gateway timeout
> for embeddings that were already paid for, and the document may not be
> stored. Split large documents into smaller files.

> **Single replica.** `minReplicas` and `maxReplicas` are both 1, and that is a
> correctness bound rather than a cost default. `internal/api/events.go` is an
> in-process SSE hub with no cross-replica backplane, so with a second replica a
> chat stream started on one replica never reaches an SSE client connected to
> the other, and the subscriber watchdog cancels it for lack of a local
> subscriber. Cancel-stream, AI budget accounting, the WebSocket ticket replay
> guard and the retention purge are per-process too. The WebSocket hub
> (`internal/websocket/backplane.go`) already has the Redis backplane the SSE hub
> needs; scale out after that pattern is applied there.

#### GitHub secrets for the deploy workflow

`.github/workflows/deploy.yml` authenticates to Azure with OIDC federated
credentials — there is no client secret. Register an app, federate it to this
repository, and grant it **Contributor** on the resource group plus **AcrPush**
on the registry.

The image is built once and promoted: staging and production deploy the same
digest. So the build job is deliberately not bound to a GitHub Environment —
otherwise a protection rule would gate the build as well as the deploy, asking
for two approvals per release. That splits the secrets in two:

**Repository-level** (read by the build job, before any environment gate):

| Secret | Purpose |
|---|---|
| `AZURE_CLIENT_ID` / `AZURE_TENANT_ID` / `AZURE_SUBSCRIPTION_ID` | OIDC federated login for `azure/login` |
| `AZURE_CONTAINER_REGISTRY` | ACR **name**, not login server |

**Environment-level** (`staging`, `production` — read by the deploy job):

| Secret | Purpose |
|---|---|
| `AZURE_RESOURCE_GROUP` | Target resource group (must already exist; the Bicep is RG-scoped) |
| `AZURE_CONTAINER_APP_NAME` | `appName` for the template |
| `PAD_AUTH_SECRET` | JWT signing key, ≥ 32 chars |
| `PAD_ENCRYPTION_KEY` | AES-256-GCM key for the provider keystore. Set it — unset, the keystore falls back to `PAD_AUTH_SECRET`, and rotating the JWT secret then makes every stored provider key unreadable |
| `PAD_DATABASE_URL` | DSN for the dedicated `baki_app` role |
| `PAD_METRICS_TOKEN` | Gates `GET /metrics` and `/debug/pprof` |
| `ALERT_EMAILS` | Comma-separated recipients. **Empty provisions no alerting at all** — all four alert resources are conditional on it |
| `PAD_ALLOWED_ORIGINS` | Only needed once a custom domain fronts the app; defaults to the ingress FQDN |
| `PAD_SMTP_HOST` / `PAD_SMTP_USERNAME` / `PAD_SMTP_PASSWORD` / `PAD_EMAIL_FROM` / `PAD_APP_BASE_URL` | Transactional email. Without SMTP the app uses a log-only mailer, which means password-reset and verification links are written into Log Analytics instead of delivered |
| `PAD_SSO_ISSUER` / `PAD_SSO_CLIENT_ID` / `PAD_SSO_CLIENT_SECRET` / `PAD_SSO_REDIRECT_URL` | OIDC SSO. All of issuer/client-id/redirect are required together or SSO stays off |
| `PAD_CI_WEBHOOK_SECRET` | Enables `POST /api/integrations/ci`; treat it as a read token for the whole flow library |
| `AZURE_STORAGE_ACCOUNT_ID` | Optional: bring your own storage account instead of letting the template provision one. The template cannot create the blob role assignment for an account it does not own — grant the managed identity **Storage Blob Data Contributor** yourself, and create the `flow-content` container |

**Repository / environment variables** (GitHub Actions *variables*, not
secrets — the deploy job passes them to the Bicep as app settings; empty
leaves the app default):

| Variable | Purpose |
|---|---|
| `PAD_AZURE_FOUNDRY_ALLOWED_HOSTS` | Extra hosts for the Azure AI Foundry endpoint allow-list — exact hosts or `*.suffix` globs (never over a public suffix or a multi-tenant Azure zone, which refuse to boot) |
| `PAD_AI_ALLOW_PRIVATE_EGRESS` | `true` allows AI endpoints that resolve to private addresses (private-link Foundry); needs VNet integration, which this template does **not** provision |
| `PAD_EMBEDDING_PROVIDER` | Deployment-wide knowledge-base embedding provider: `openai`, `gemini`, `glm`, `github-models` or `azure-foundry`; anything else refuses to boot |
| `PAD_EMBEDDING_DIM` | Knowledge-base embedding width (default 1536, never above the model's native width); changing it on a populated knowledge base needs a re-index |

The deploy job also uses the three `AZURE_CLIENT_ID` / `AZURE_TENANT_ID` /
`AZURE_SUBSCRIPTION_ID` secrets. Repository-level values are visible to
environment-scoped jobs, so define them once at the repository level and add
environment-level overrides only if staging and production use different
subscriptions.

To review an infrastructure change before it ships, run what-if by hand against
a scratch resource group:

```bash
az deployment group what-if -g rg-baki-scratch -f infra/main.bicep \
  --parameters @infra/main.parameters.json
```

CI compiles every `infra/*.bicep` on each push (the `bicep` job) and fails on
warnings, so schema mistakes cannot reach a deploy.

AI provider keys are set in-app under **Settings → AI Accounts** (`Ctrl + ,`). Desktop stores them in the OS keyring; web stores them server-side. Supported: GitHub Copilot (OAuth device flow or PAT), GitHub Models, Anthropic Claude, OpenAI, Google Gemini, xAI Grok, Zhipu GLM, and **Azure AI Foundry** (Microsoft Foundry / Azure OpenAI).

Azure AI Foundry support is **Azure public cloud only**: Azure Government and Azure China (21Vianet) endpoints and sign-in authorities are not supported, and the Microsoft Entra ID sign-in is pinned to `login.microsoftonline.com` whatever `AZURE_AUTHORITY_HOST` says. Under **Settings → AI Accounts → Azure AI Foundry** you add one or more **resources** (up to 10), each with its own endpoint (`https://<resource>.openai.azure.com`, `https://<resource>.services.ai.azure.com` or `https://<resource>.cognitiveservices.azure.com`; serverless `*.models.ai.azure.com` endpoints are refused — the provider speaks `/openai/v1`, which they do not serve: deploy the model to a Foundry resource) and its own credentials: the resource API key, a Microsoft Entra ID service principal (tenant ID — the directory GUID or a domain such as `contoso.onmicrosoft.com`, never `common`/`organizations` — client ID and client secret; the app registration needs the **Foundry User** role on the resource) or, in the desktop app only, your Azure CLI sign-in. A refused sign-in names its cause in the connection test: an expired client secret, the secret's ID pasted instead of its value, an unknown tenant or application, a disabled application, Conditional Access. Each **model** you want to chat with — any deployed model (GPT-4.1/4o/5, o-series, gpt-oss, DeepSeek, Grok, Llama, Mistral, Phi, the model router, a distilled model of your own), a Claude deployment or a Foundry agent — and each embedding deployment is added to its resource by deployment (or agent) name, with a display name, a **purpose** (General, Documentation, Fixes & debugging, Review & analysis, Refactoring, Testing, or one of your own), an optional description and, for small distilled models, an optional context window and **max output tokens** (the reasoning-model minimum below never goes above it); up to 50 models. The chat model picker shows exactly the models you added, grouped by purpose, and **each chat thread keeps its own model**: a pick in the chat header changes only that thread, and new chats start on the **Default for new chats** (Make default in AI Accounts, or the Default for new chats field in AI Behavior), else on the server's default — the model imported as an earlier configuration's chat deployment, else your first chat model, else your organisation's default for members. A thread whose model was removed says so and falls back to the default; a model whose resource needs its credentials entered again is shown disabled, and its threads return to it once they are. Azure offers no way to list a resource's deployments with an API key (`/openai/v1/models` is the model catalogue, not the deployments), so in API-key mode you add deployments by name; with Microsoft Entra ID (or the Azure CLI) and a project endpoint, AI Accounts also **suggests the project's deployments and agents**, annotated with the model behind them, to add in one click — nothing is added without you. A deployment named after its model (the portal's default, e.g. `gpt-4o` or `Phi-4`) is sized from the model catalogue up front. **Test** on a resource only checks the sign-in and the endpoint (no model call, no cost); on a chat, Claude or embedding model it sends one tiny request, which Azure bills; on an agent it only looks the agent up and never runs it. Reasoning models — o-series (codex-mini included), GPT-5 and its mini/nano/pro/codex variants (not gpt-5-chat), the GPT-6 family (gpt-6-astra, gpt-6-sol, gpt-6-luna, gpt-6.1-sol), gpt-chat-latest, gpt-oss, DeepSeek-R1 and MAI-DS-R1, DeepSeek-V3.2-Speciale and DeepSeek-V4-Pro, MAI-Thinking-1, Grok 4 and grok-code-fast-1, Phi-4-reasoning and Phi-4-mini-reasoning, Claude Opus/Sonnet 5.x, Fable and Mythos, a `claude:`-deployment whose model is not known to be a non-thinker, and a model-router deployment, which may route any call to a reasoning model — spend their hidden thinking from the per-turn output budget, so a turn on them (on every provider, not only Azure) is sent at least 16384 max tokens whatever **Max Tokens** (Settings → AI Behavior) is set to, still capped at the model's own output limit; a turn that uses it all before writing an answer fails with `PROVIDER_OUTPUT_LIMIT` instead of returning an empty reply. An explicit temperature of 0 is honoured (sent as 0 for deterministic sampling, not dropped as "provider default"); a DeepSeek-R1 `<think>…</think>` block is split off the answer and never shown, stored or sent back as history. An embedding model (e.g. `text-embedding-3-small`) is never offered in the chat, and it embeds the knowledge base only when the operator sets `PAD_EMBEDDING_PROVIDER=azure-foundry` and it is marked **Use for knowledge base** (one per user and one per organisation; an organisation's embeds its corpus, and an organisation with Azure resources but none marked does not fall back to a member's). Marking another model, or changing or removing the marked one, on a non-empty corpus answers `409 REINDEX_REQUIRED` until confirmed, after which the corpus needs a re-index: two embedding models in one corpus split its retrieval. Knowledge-base uploads and re-indexes whose embedding model differs from the indexed corpus's are refused (`EMBEDDING_MODEL_CHANGED`); an org admin can confirm a re-index that switches the whole corpus to their model. Embeddings are requested at `PAD_EMBEDDING_DIM` (default 1536, never above the model's native width), so a knowledge base indexed with 3072-wide `text-embedding-3-large` vectors needs a re-index. With a project endpoint (`https://<resource>.services.ai.azure.com/api/projects/<project>`) and Entra ID credentials, the project's prompt and workflow **Foundry agents** can be added as models of kind Agent (hosted, external and voice agents cannot be run through the shared Responses route and are not suggested). Suggesting them needs the **Foundry User** role on the project: the least-privilege **Foundry Agent Consumer** role can run an agent but not list the project's agents. Workflow agents are a Microsoft preview that retires on December 1, 2026, and are called with the `Foundry-Features: WorkflowAgents=V1Preview` opt-in header. Chatting with an agent runs it through the Responses API, its hosted tools (web search, file search, MCP, code interpreter, Azure AI Search, Bing grounding, OpenAPI, Azure Functions, SharePoint, …) show up in the tool trail, and an agent that declares function tools named like the app's flow tools (`search_flow`, `get_block`, `get_subflow_blocks`, `list_findings`, `propose_fix`, `apply_fix`, `apply_fixes`, `get_variable_lineage`, with the same JSON schemas) has them executed by the app. The per-thread **Tools** toggle does not apply to agents: an agent always runs with the tools it declares. The app cannot answer MCP tool approval requests, so an agent's MCP tools must set `require_approval` to `"never"` in the agent definition (the portal asks for approval by default); a run that stops for approval ends with a message naming the tool. Agent runs are stateless (`store:false` — nothing is stored server-side): the reasoning and hosted-tool items of one tool-loop step are replayed verbatim (encrypted) with the next request of the same answer, so the agent keeps its thinking across a tool step; once the run completes, nothing is retained. **Azure AI Foundry usage is not priced by baki:** token counts are recorded (Analytics shows them) but with no cost, the model picker shows no price for Azure models, and the daily AI budget neither counts nor gates Azure usage: an Azure chat turn, and a knowledge-base upload or re-index that embeds with Azure, is never refused because other, priced providers have spent the budget — Azure's own quotas are the limit. Read the cost in the Azure portal (this includes hosted tool charges such as Bing grounding, code interpreter sessions and file-search storage). Operators can extend the endpoint allow-list with `PAD_AZURE_FOUNDRY_ALLOWED_HOSTS` (exact hosts, or `*.suffix` globs — never over a public suffix or a multi-tenant zone such as `*.azure-api.net`, `*.azurewebsites.net` or `*.cloudapp.azure.com`, which refuse to boot) and open private-link resources' RFC 1918 / unique-local addresses with `PAD_AI_ALLOW_PRIVATE_EGRESS` (loopback, link-local — the metadata service — and the other internal ranges stay refused; see `.env.example`). A connection test that cannot reach the endpoint reports the kind of failure (DNS, proxy, TLS, timeout, internal address) and never the addresses involved, which go to the server log.

**Claude on Azure AI Foundry.** Claude models (Opus, Sonnet and Haiku 4.x, the 5.x models and Fable) are sold through Foundry with Azure Marketplace billing — a paid pay-as-you-go subscription on the resource is required; Version 1 deployments are hosted by Anthropic, Version 2 by Azure, and the Mythos tier accepts Microsoft Entra ID only (no API key). A Claude deployment does not answer on the OpenAI route, so add it as a model of kind **Claude** (with Entra ID and a project endpoint, AI Accounts suggests it from the project's `modelPublisher`); it carries a Claude badge in the picker, is called through the Anthropic Messages API (`/anthropic/v1/messages`) with streaming and tools, and is sized at its model's context window as Anthropic documents it for Foundry (1M for Opus/Sonnet 4.6 and later and the 5.x models, 200k for Sonnet 4.5 and Haiku 4.5; a custom-named deployment starts at 200k until its first answer names the model). A deployment named after its model (e.g. `claude-sonnet-4-6`) is sized from the Claude catalogue up front, and like every Azure deployment it is not priced by baki (see above). Claude deployments are always called on `<resource>.services.ai.azure.com` (where Foundry serves the Anthropic route), even when the configured endpoint is `<resource>.openai.azure.com`.

In the web deployment organisation admins can also curate **shared resources and models** for their organisation under **Settings → Organizations** (`/api/orgs/{id}/providers/azure-foundry/…`; credentials stored encrypted in `org_provider_keys`, the list in the `org_azure_*` tables, both removed with the organisation). Only the organisation's **admins and members** see and run them — viewers, guests and collaborators invited to a single flow need models of their own, even on the organisation's flows. On the organisation's flows the chat picker lists the shared models, marked **Shared**, next to the member's own (`GET /api/providers/list?orgId=<id>`), and AI Accounts shows them read-only under **Shared with you**; an admin's **Default for members** is where new chats start for members who have no Azure models or default of their own. Pre-registry model ids (`gpt-4.1`, `claude:x`, `agent:y`, accepted for one release) resolve member-first: through the member's own imported configuration, and through the organisation's only for a member without one. The knowledge base is the exception and goes org-first (with `PAD_EMBEDDING_PROVIDER=azure-foundry`, every upload and search of the org's corpus embeds with the shared resource, so one corpus is embedded by one deployment). The trust model is deliberately narrow: org admins are not trusted principals (anyone can create an organisation), and shared secrets are write-only — the organisation's admins and members can read the rest: each resource's endpoint, auth mode, Microsoft Entra ID tenant and client IDs and project endpoint, its models (names, descriptions and purposes are the admins' plain text), plus who shared it (the display name or email of the lending admin — the one who supplied the stored secret; another admin's edit that keeps the secret does not change it) and when it was last saved; the deprecated single-configuration GET still answers every member, as before. An omitted secret on save keeps the stored one only while the endpoint (and project endpoint) host is unchanged; pointing a resource at another host needs the secret re-entered. Saves are compare-and-swap: a resource save sends back the `updatedAt` (and, with new credentials, the `credentialUpdatedAt`) its GET returned, and a resource changed meanwhile by another admin answers `409 CONFLICT`. A shared credential is its lending admin's: when the admin who supplied a resource's secret (the lender — another admin's edit that keeps the secret does not make them one) is demoted, removed from the organisation or deletes their account, every shared credential they lent is disabled (deleted and audited as `keys.save` with `op: disable`, one row per resource); the resources and their models stay listed as needing credentials until a remaining admin enters them again. Changes are audited as `azure.connection_save`, `azure.connection_delete`, `azure.model_save` and `azure.model_delete` (ids, kind, deployment name and auth mode — never a secret or a description); the deprecated single-configuration routes keep auditing `keys.save` with the endpoint and project hosts, the auth mode, the Entra ID tenant and client IDs and whether the stored secret was carried over. Usage on shared credentials — chat, knowledge-base embeddings and an admin's tests (audited as `providers.test`) — is attributed to the organisation (token counts are recorded; the cost is shown in the Azure portal, and the organisation's daily AI budget neither counts nor gates it). Chat turns and knowledge-base uploads and re-indexes on Azure AI Foundry go ahead even once that budget has been spent by the organisation's other, priced providers; uploads and re-indexes that embed with a priced provider are then refused with `429 BUDGET_EXCEEDED`.

**Upgrading to the model registry (schema v25).** On first use after the upgrade, each existing Azure AI Foundry configuration (personal and shared) becomes a resource named after its endpoint host, with one model per deployment it named (and a saved default model, `agent:` ones included), so existing chats and defaults keep their model; a Microsoft Entra ID + project configuration whose deployments and agents used to be listed automatically shows a **Review** prompt (in AI Accounts and the chat picker) instead of listing them. API clients: the new routes are `/api/providers/azure-foundry/connections[/{id}[/discover]]`, `/api/providers/azure-foundry/models[/{id}]` and `POST /api/providers/azure-foundry/test` (one route for a draft or saved resource or model), the same under `/api/orgs/{id}/providers/azure-foundry/`; chat requests name a model by its `m/<id>` (from `GET /api/providers/list`), and one the caller cannot use is refused with `400 MODEL_NOT_AVAILABLE` on `POST /api/chat/stream` and `/api/chat/preview-context`. For one release the single-configuration routes (`GET/POST /api/providers/azure-foundry/config` and the organisation's `…/config` and body-less `…/test`) and raw deployment ids in chat requests keep working on the imported resource. Analytics records Azure usage by deployment name, so two resources with a same-named deployment share a row; Prometheus now labels the organisation's Azure routes `/api/orgs/:id/providers/azure-foundry/…` instead of `/api/orgs/:id`. **Rolling back** to an image from before schema v25 needs the migration undone first, because older images refuse to start on a newer schema: stop the app (`docker compose stop app` — a running new image would re-apply v25), run `go run ./cmd/bakicli migrate down --dsn "<DSN>" 24` from this checkout (it asks you to type 24), then deploy the older image. That deletes the model list, every resource but the imported one (with their stored credentials) and the knowledge-base and default designations; the imported configuration comes back with the deployments the single-configuration route last saved (models added or removed in the new AI Accounts are not carried back).

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
- **AI review** — streaming chat over your flow with GitHub Copilot, Anthropic Claude, OpenAI, Google Gemini, xAI Grok, Zhipu GLM, GitHub Models, or any Azure AI Foundry deployment and Foundry agent. Tool-augmented mode for autonomous analysis.
- **Export** — full analysis reports to PDF or Markdown (works in both desktop and browser); analysis history with regression diffing.
- **CI/CD** — headless `bakicli` runs static analysis and exits non-zero past a severity threshold (`-fail-on`) or a named, shareable **policy** (`-policy policy.json`), ratcheting on new findings only with `-baseline` or `-new-from-rev`. It emits `text`, `json`, **SARIF 2.1.0**, `junit`, `csv` or `html`; `-report-json` also writes the JSON report from the same run. A packaged **GitHub Action** uploads SARIF to the Security tab, writes a job summary and gates the build; GitLab and Azure DevOps templates live in `ci/`. See [CI/CD](#cicd).
- **Governance** *(web mode)* — persistent, team-shared finding triage (status/assignee/justification) and per-flow **baselines** so dashboards and CI can ratchet on *new* findings only; an org-wide **portfolio** view (Command Palette → "Flow Portfolio", or `GET /api/library/portfolio`) ranks every accessible flow worst-health-first; a periodic scanner re-analyzes stored flows and **alerts on drift or health regressions** via webhook / Microsoft Teams (`PAD_SCAN_INTERVAL` + `PAD_NOTIFY_*`).
- **Collaboration** *(web mode)* — organizations with role-based access (admin/member/viewer/guest); real-time presence indicators and block selection sync via WebSocket; flow sharing with per-flow collaborator permissions.
- **SSO** *(web mode)* — OIDC single sign-on with Microsoft Entra ID, Google, Okta, or any OIDC-compliant IdP. Account linking with automatic JIT provisioning.
- **Account email** *(web mode)* — transactional email for **password reset** (`/api/auth/forgot-password` → `/api/auth/reset-password`, single-use 1-hour token, all sessions revoked on reset), **email verification** (`/api/auth/verify-email`, sent on registration), and **org-invite delivery**. Configure an SMTP relay via `PAD_SMTP_*` + `PAD_EMAIL_FROM`; without it the app uses a log-only mailer so non-email deployments still work (links land in the server log). Reset endpoints never reveal whether an email exists.
- **Machine API tokens** *(web mode)* — scoped, revocable personal access tokens (managed under **Settings → API Tokens**, or `POST /api/auth/tokens`) so CI and automation can call the API as a user without an interactive login. Sent as `Authorization: Bearer pad_pat_…`; only the token hash is stored, the raw value is shown once, and the owner's current role applies (revoke = immediate). API change for chat-scoped tokens: `POST /api/chat/get` now answers an object, `{"messages": [...], "activeStreamId": "…"}` (`activeStreamId` only while the answer to a turn on that conversation is not stored yet — resume it with `POST /api/chat/resume`), instead of the bare message array it returned before; scripts that read the array must read `messages`.
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

Both headers list every variable or parameter. To record a run on a PAD Analyzer server's dashboard, use `bakicli report` — it signs the request with `PAD_CI_WEBHOOK_SECRET` and posts to `/api/integrations/ci`, which is disabled until that secret is set. Treat the secret as a read token for the whole flow library.

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
