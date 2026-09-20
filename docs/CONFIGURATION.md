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
| `PAD_STORAGE` | `local` (no server-side store: the desktop app keeps flows in memory and settings/conversations in its app-data directory, and library, triage, comments, versions and sharing are unavailable) or `database` (PostgreSQL). Cloud mode with auth needs `database`. | `local` |
| `PAD_DATA_DIR` | Data directory of a legacy server that ran with filesystem storage. Cloud mode reads it only as the source of the admin data migration (flows, settings, conversations). | — (migration off) |
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
| `PAD_CI_WEBHOOK_SECRET` | HMAC key for inbound CI webhooks (`POST /api/integrations/ci`, see [CI webhook](#ci-webhook)). Empty = endpoint returns 503. | — |
| `PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP` | `1` accepts CI webhooks without `X-Baki-Timestamp` (replay-vulnerable; legacy senders only). | unset |
| `PAD_RETENTION_PURGE_INTERVAL` | How often expired tokens, invites, share links and blacklist rows are purged (cloud). `0s` disables. | `24h` |
| `PAD_AUDIT_RETENTION_DAYS` | Age-based purge window for audit events, analysis history, usage metrics and governance alerts. `0` keeps them forever. | `0` |
| `PAD_AUDIT_SPILL_DIR` | On-disk spill queue for audit events when the in-memory pool overflows. Empty = temp dir; `off` disables. | temp dir |

## CI webhook

`POST /api/integrations/ci` lets a pipeline ask the server to re-analyse a **stored** flow and record the run on the dashboard. The server never ingests numbers from the runner. It is off until `PAD_CI_WEBHOOK_SECRET` is set.

The secret is a deployment-level credential, not a user's: a caller holding it
can ask the server to analyse any stored flow by ID and read back its finding
counts (and, with `format: report`, the findings themselves). Treat it like a
read token for the whole library and scope it to CI.

The request body is `{"flowId": "<id>", "failOn": "error", "format": "summary"}`. `failOn` accepts `error`, `warning`, `info` or `none`; `format` accepts `summary`, `report` (adds findings) or `sarif`. Two headers authenticate it:

- `X-Baki-Timestamp` is the current Unix time in seconds. The server rejects values more than five minutes off its clock.
- `X-Baki-Signature` is `sha256=` followed by the hex HMAC-SHA256 of `<timestamp>.<raw body>`, keyed with `PAD_CI_WEBHOOK_SECRET`.

`bakicli report` does all of this and exits `0` when the server's gate passed, `1` when it failed and `2` on a request error:

```bash
export PAD_CI_WEBHOOK_SECRET=...        # keeps the key off the command line
bakicli report -endpoint https://baki.example.com -flow-id "$FLOW_ID" -fail-on error
```

The same request with curl:

```bash
body='{"flowId":"'"$FLOW_ID"'","failOn":"error"}'
ts=$(date +%s)
sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$PAD_CI_WEBHOOK_SECRET" -hex | sed 's/^.* //')
curl -fsS -X POST https://baki.example.com/api/integrations/ci \
  -H 'Content-Type: application/json' \
  -H "X-Baki-Timestamp: $ts" \
  -H "X-Baki-Signature: sha256=$sig" \
  --data "$body"
```

## `bakicli` exit codes

| Code | Meaning |
|---|---|
| `0` | Gate passed. |
| `1` | Gate failed: findings at or above `-fail-on`, policy violated, stale suppressions with `-fail-on-stale`, or the output could not be written. The report is complete. |
| `2` | Usage error, invalid config or policy, parse error, or file not found. No report is produced. |

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
| auth (login, refresh, password reset) | `PAD_RATE_LIMIT_AUTH_RPS` / `PAD_RATE_LIMIT_AUTH_BURST` | 0.083 (5 per minute) / 5 |
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
| `PAD_OTLP_ENDPOINT` | OTLP/HTTP endpoint (`host:port`) for traces. Empty = tracing off. Access-log lines carry `trace_id` when a span is present. | — |

## Frontend build

| Variable | Purpose |
|---|---|
| `VITE_API_URL` | Backend origin for the web build (no trailing `/api`). The desktop app discovers the sidecar itself. |

## Health score

Every analysis report carries `metrics.healthScore` (0–100; also on the
dashboard, in `bakicli -format json`, and as the input to drift / regression
alerts). It is computed in `core/analyzer/metrics.go` as

```
score = 100
      − 12 × errors − 4 × warnings − 1 × infos
      − 2   × max(0, maxCyclomatic − 20)
      − 1.5 × max(0, maxCognitive  − 30)
      − 10 if any circular subflow-call dependency exists
```

clamped to `[0, 100]` and truncated to an integer.

- The finding counts are the findings that appear in the report: after inline
  `pad-ignore` suppression and de-duplication, and after any per-rule
  `severity` override in the rule profile. Suppressed findings do not count.
- `maxCyclomatic` / `maxCognitive` are the highest per-subflow values in the
  flow (`metrics.maxCyclomatic`, `metrics.maxCognitive`), so one complex
  subflow lowers the whole flow's score.
- The scale saturates quickly: 9 errors (108 points) or 25 warnings alone
  reach 0, so scores near 0 do not distinguish "bad" from "much worse". Treat
  it as a trend signal for a flow over time; gate CI on findings and policies
  (`bakicli -fail-on`, `-policy`) rather than on a score threshold.

## Custom rules

Custom rules are JSON objects evaluated per block alongside the built-in rules.
They are loaded from:

- `bakicli -custom-rules <file>` (or `"customRules": "<file>"` in
  `.bakirc.json`), plus inline `"customRulesInline": [ … ]` entries in
  `.bakirc.json` — both sources are additive;
- `PAD_CUSTOM_RULES=<file>` for the server, read at boot;
- the organisation rule editor (`POST /api/orgs/{id}/rules`), applied to that
  organisation's analyses; `GET /api/orgs/{id}/rules/export` emits a
  `.bakirc.json` with those rules inline so CI gates on the same set.

The file is a JSON **array** of rule objects (`bakicli rules test`, which runs
one rules file against a flow, also accepts a single object). An invalid entry
is **skipped with a warning** that names the entry — on CLI stderr, in the
server log, or in the response of `POST /api/analysis/custom-rules/validate`
— and a skipped rule enforces nothing, so treat those warnings as errors in CI.

| Field | Matching | Purpose |
|---|---|---|
| `id` | — | Rule identifier (kebab-case). Findings, `pad-ignore[<id>]`, triage, baselines and SARIF are keyed by it, so it must be unique and stable; do not reuse a built-in ID. |
| `name` | — | Finding title. |
| `description` | — | Finding description. |
| `severity` | exact | `error`, `warning` or `info`. Default `warning`. Any other value rejects the entry (it used to be accepted and then silently dropped by every consumer). |
| `category` | — | Free text (`Security`, `Reliability`, …) shown in the catalog and reports. |
| `typeMatch` | exact, case-insensitive | Block type: `ACTION`, `LOOP`, `CONDITION`, `SUBFLOW`, `ERROR_HANDLER`, `COMMENT`, `VARIABLE`, `WAIT`, `ELSE`, `CASE`, `DEFAULT`, `BLOCK`, `SWITCH`, `UNKNOWN`. (`END` blocks are never checked.) |
| `rawTypeMatch` | **regex** (RE2, case-insensitive, unanchored) | Against the block's raw action type, e.g. `Database.Connect`, `WebAutomation.LaunchChrome`, `LOOP`. |
| `nameMatch` | **regex** (RE2, case-insensitive, unanchored) | Against the block's display name; for `COMMENT` blocks this is the comment text. |
| `propertyHas` | **substring** (case-insensitive), per key | `{"<property>": "<text>"}`. Every listed property must exist on the block **and** contain the text. |
| `propertyNot` | **substring** (case-insensitive), per key | `{"<property>": "<text>"}`. The rule is skipped when any listed property exists and contains the text; a missing property passes. |
| `suggestion` | — | Finding suggestion. Default: "Review this block for potential issues." |
| `autoFix` | exact | Optional fixer attached to the finding. One of `wrap-error-handler`, `insert-close`, `set-timeout`, `insert-delay`, `insert-delay-in-loop`, `insert-handler-log`, `init-variable`, `insert-error-log`, `replace-with-variable`, `wrap-in-retry`, `insert-exit-condition`, `remove-block`, `parameterize-sql`, `append-output`, `mask-sensitive-variable`, `insert-default`, `upgrade-to-https`, `sanitize-command-vars`, `suppress`. Any other value rejects the entry. The fixer must make sense for the matched block (e.g. `insert-close` needs an action that opens a resource); an inapplicable fixer produces an empty patch. |

All matchers are ANDed; a rule with no matcher at all fires on every block.

**Regex vs. substring — the trap.** `nameMatch` and `rawTypeMatch` are regular
expressions; `propertyHas` and `propertyNot` are plain substrings. So
`"propertyHas": {"Url": "^http://"}` never matches (the `^` is a literal
character) — write `"http://"`. Conversely `"rawTypeMatch": "Database.Connect"`
also matches `Database.ConnectX` and, because `.` is a regex wildcard,
`DatabaseXConnect`; anchor and escape when you mean an exact action:
`"^Database\\.Connect$"`.

Property keys are the names as written in the flow (`Url`, `ConnectionString`,
`Timeout`), plus parser-synthesised keys: `_output` is the variable after
`=>`, and `SET` blocks expose `_var` and `_value`.

```json
[
  {
    "id": "org-no-plain-http",
    "name": "Cleartext HTTP call",
    "description": "HTTP calls must use https://.",
    "severity": "error",
    "category": "Security",
    "rawTypeMatch": "^HTTPClient\\.",
    "propertyHas": {"Url": "http://"},
    "propertyNot": {"Url": "http://localhost"},
    "suggestion": "Switch the URL to https://.",
    "autoFix": "upgrade-to-https"
  }
]
```
