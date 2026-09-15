# Configuration reference

Every environment variable the server and CLI read, in one place. The server
reads `PAD_*` only when `PAD_MODE` or `PAD_PORT` is set (or `PAD_CONFIG` names a
file); otherwise it runs with local/desktop defaults. Durations are Go strings
(`30s`, `5m`, `24h`); booleans accept `true/false`, `1/0`, `yes/no`.

Validation runs at boot and refuses to start on the important mistakes:
unknown mode, cloud mode without auth, a weak JWT secret (32-character floor,
placeholder blocklist, entropy check), a wildcard CORS origin, pprof without a
metrics token, an insecure `sslmode` in cloud mode.

## Core

| Variable | Purpose | Default |
|---|---|---|
| `PAD_CONFIG` | Path to a JSON config file. When set it is loaded first and `PAD_*` variables are ignored. | — |
| `PAD_MODE` | `local` (desktop sidecar) or `cloud` (web server). Setting it enables env-var loading. | `local` |
| `PAD_HOST` / `PAD_PORT` | Bind address / port. `PAD_PORT=0` picks an ephemeral port (desktop). | `localhost` / `0` |
| `PAD_STATIC_DIR` | Directory served at `/` (the built frontend). Empty = no static files. | — |
| `PAD_ALLOWED_ORIGINS` | Comma-separated CORS / WebSocket origin allowlist. Wildcards are rejected in cloud mode. | — |
| `PAD_TRUSTED_PROXIES` | Comma-separated proxy IPs/CIDRs whose `X-Forwarded-For` is trusted. Wildcards rejected. | — |
| `PAD_BEHIND_PROXY` | Declare a TLS-terminating reverse proxy in front (skips the plaintext-credentials check). Set `PAD_METRICS_TOKEN` too — behind a proxy every peer address is private. | `false` |
| `PAD_TLS_CERT` / `PAD_TLS_KEY` | PEM cert/key paths to serve HTTPS directly. Both or neither. | — |
| `PAD_REQUEST_TIMEOUT` | Per-request deadline. `/ws` and `/api/events` are unbounded; `*/stream` routes get a fixed 15-minute outer deadline instead. | `30s` |
| `PAD_LOG_LEVEL` | `debug`, `info`, `warn`, `error`. | `info` (`debug` in local mode) |
| `PAD_LOCAL_CONFIG_ENDPOINT` | Serve the local-mode session token from `GET /api/local-config`. Browser-based local development only; never for the desktop build. | `false` |
| `PAD_CUSTOM_RULES` | Path to a JSON file of custom analyzer rules loaded at boot. | — |
| `PAD_FEATURE_DISABLE_SIGNUP` | Block self-registration (closed deployment). Exposed via `GET /api/system/features`. | `false` |

## Auth and SSO

| Variable | Purpose | Default |
|---|---|---|
| `PAD_AUTH_ENABLED` | Enforce JWT auth. Required for any browser/multi-user use. | `false` |
| `PAD_AUTH_SECRET` | JWT signing secret. Required when auth is enabled; ≥ 32 chars in cloud mode. Rotating it logs every user out. | — |
| `PAD_ENCRYPTION_KEY` | Dedicated AES-256-GCM key for stored provider API keys. Falls back to `PAD_AUTH_SECRET` with a deprecation warning. Rotating it makes stored provider keys undecryptable. | — |
| `PAD_SSO_ISSUER` | OIDC issuer URL (discovery at `/.well-known/openid-configuration`). SSO is on when issuer, client ID and redirect URL are all set. | — |
| `PAD_SSO_CLIENT_ID` / `PAD_SSO_CLIENT_SECRET` | OAuth2 client credentials. Secret is optional (PKCE-only public clients). | — |
| `PAD_SSO_REDIRECT_URL` | Absolute callback URL registered with the IdP, e.g. `https://app.example.com/api/auth/sso/callback`. | — |
| `PAD_SSO_PROVIDER_NAME` | Display label and identity-link key. Changing it after users have linked breaks their links. | `sso` |

## Storage

| Variable | Purpose | Default |
|---|---|---|
| `PAD_STORAGE` | `local` (filesystem) or `database` (PostgreSQL). Cloud mode with auth needs `database`. | `local` |
| `PAD_DATA_DIR` | Data directory for the filesystem backend. | OS app-data dir |
| `PAD_DATABASE_URL` | Postgres DSN. Connect as a dedicated role (`NOSUPERUSER NOBYPASSRLS`) — superusers bypass Row-Level Security. `password=managed-identity` uses Azure Entra tokens. Also read by `bakicli migrate`. | — |
| `PAD_DB_REQUIRE_SSL` | Force/allow TLS on the DSN (`true`/`false`). | required in cloud, optional in local |
| `PAD_DB_MAX_OPEN_CONNS` / `PAD_DB_MAX_IDLE_CONNS` | Pool sizing. | `25` / `5` |
| `PAD_DB_CONN_MAX_LIFETIME` / `PAD_DB_CONN_MAX_IDLE_TIME` | Pool connection recycling. | `1h` / `5m` |
| `PAD_DB_STATEMENT_TIMEOUT` | Postgres `statement_timeout` and `lock_timeout` on every pooled connection. Migrations lift it for their own session. `0s` disables (maintenance). | `30s` |
| `PAD_EMBEDDING_DIM` | Knowledge-base embedding width (deploy-time contract; must match the embedding provider). | `1536` |
| `PAD_AZURE_STORAGE_ACCOUNT` / `PAD_AZURE_STORAGE_CONTAINER` | Offload flow content to Azure Blob (both together; database backend). Managed Identity auth. | — |
| `PAD_AZURE_BLOB_CONNECTION_STRING` | Blob auth via connection string instead of Managed Identity (Azurite / non-MI). | — |

## Redis backplane (multi-replica only)

| Variable | Purpose | Default |
|---|---|---|
| `PAD_REDIS_URL` | Shared backplane for rate limits, presence and chat-stream resume. **When set and unreachable the server refuses to boot** — leave unset on a single replica. | — |
| `PAD_REDIS_POOL_SIZE` / `PAD_REDIS_MIN_IDLE_CONNS` | go-redis pool overrides. `0` = library default. | `0` |

## Governance, notifications, retention

| Variable | Purpose | Default |
|---|---|---|
| `PAD_SCAN_INTERVAL` | Periodic re-scan of the library; enables drift/health-regression alerts (cloud). Empty = off. | — |
| `PAD_NOTIFY_WEBHOOK_URL` / `PAD_NOTIFY_WEBHOOK_SECRET` | Generic JSON webhook for alerts, optional HMAC signing key. | — |
| `PAD_NOTIFY_TEAMS_URL` | Microsoft Teams incoming webhook. | — |
| `PAD_NOTIFY_SLACK_URL` / `PAD_NOTIFY_SLACK_SECRET` | Slack-compatible incoming webhook (also Discord/Mattermost), optional HMAC key. `PAD_WEBHOOK_URL` / `PAD_WEBHOOK_SECRET` are legacy aliases read when these are unset. | — |
| `PAD_NOTIFY_EMAIL_TO` | Recipient for alert emails (needs the SMTP settings below). Deployment-global. | — |
| `PAD_NOTIFY_JIRA_BASE_URL` / `PAD_NOTIFY_JIRA_EMAIL` / `PAD_NOTIFY_JIRA_API_TOKEN` / `PAD_NOTIFY_JIRA_PROJECT` | Create a Jira Cloud issue per alert when all four are set. Deployment-global. | — |
| `PAD_CI_WEBHOOK_SECRET` | HMAC key for inbound CI webhooks (`POST /api/integrations/ci`). Empty = endpoint returns 503. | — |
| `PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP` | `1` accepts CI webhooks without `X-Baki-Timestamp` (replay-vulnerable; legacy senders only). | unset |
| `PAD_RETENTION_PURGE_INTERVAL` | How often expired tokens, invites, share links and blacklist rows are purged (cloud). `0s` disables. | `24h` |
| `PAD_AUDIT_RETENTION_DAYS` | Age-based purge window for audit events, analysis history, usage metrics and governance alerts. `0` keeps them forever. | `0` |
| `PAD_AUDIT_SPILL_DIR` | On-disk spill queue for audit events when the in-memory pool overflows. Empty = temp dir; `off` disables. | temp dir |

## Email (SMTP)

| Variable | Purpose | Default |
|---|---|---|
| `PAD_SMTP_HOST` / `PAD_SMTP_PORT` | SMTP relay (587 STARTTLS or 465 TLS). | — / `587` |
| `PAD_SMTP_USERNAME` / `PAD_SMTP_PASSWORD` | SMTP credentials. | — |
| `PAD_EMAIL_FROM` | From address; email is enabled when set together with the host. | — |
| `PAD_APP_BASE_URL` | Public origin used in email links. | — |

## Rate limits (cloud mode)

Token buckets per client IP per group; `*_RPS` is the refill rate, `*_BURST`
the bucket size.

| Group | Variables | Default RPS / burst |
|---|---|---|
| general | `PAD_RATE_LIMIT_GENERAL_RPS` / `PAD_RATE_LIMIT_GENERAL_BURST` | 1.0 / 20 |
| auth (login, refresh, password reset) | `PAD_RATE_LIMIT_AUTH_RPS` / `PAD_RATE_LIMIT_AUTH_BURST` | 5.0 / 5 |
| analysis (expensive) | `PAD_RATE_LIMIT_EXPENSIVE_RPS` / `PAD_RATE_LIMIT_EXPENSIVE_BURST` | 2 / 5 |
| chat | `PAD_RATE_LIMIT_CHAT_RPS` / `PAD_RATE_LIMIT_CHAT_BURST` | 3 / 10 |
| upload | `PAD_RATE_LIMIT_UPLOAD_RPS` / `PAD_RATE_LIMIT_UPLOAD_BURST` | 1 / 3 |
| per user (all writes) | `PAD_RATE_LIMIT_PER_USER_RPS` / `PAD_RATE_LIMIT_PER_USER_BURST` | 10 / 20 |

## AI providers and resilience

| Variable | Purpose | Default |
|---|---|---|
| `PAD_CB_FAILURES` / `PAD_CB_OPEN_DURATION` | Provider circuit breaker: consecutive failures to open, and how long it stays open. | `5` / `30s` |
| `PAD_RETRY_MAX_ATTEMPTS` / `PAD_RETRY_BASE_DELAY` | Provider retry on rate-limit / outage responses (exponential backoff from the base delay). Retries are counted in `pad_ai_retry_attempts_total`. | `3` / `500ms` |
| `PAD_AI_MAX_CONNS_PER_HOST` | Cap on concurrent connections per provider host (each stream holds one). `0` disables. | `256` |

## Observability

| Variable | Purpose | Default |
|---|---|---|
| `PAD_METRICS_TOKEN` | Bearer token required on `GET /metrics` and `/debug/pprof/*` in addition to the private-IP allowlist. Set it whenever `PAD_BEHIND_PROXY=true`. | — (public; warning at boot) |
| `PAD_PPROF_ENABLED` | Register `/debug/pprof/*`. Requires `PAD_METRICS_TOKEN`. | `false` |
| `PAD_OTLP_ENDPOINT` | OTLP/gRPC endpoint for traces and the error sink. Empty = tracing off. Access-log lines carry `trace_id` when a span is present. | — |

## Frontend build

| Variable | Purpose |
|---|---|
| `VITE_API_URL` | Backend origin for the web build (no trailing `/api`). The desktop app discovers the sidecar itself. |
