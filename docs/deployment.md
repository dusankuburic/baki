# Deployment and operations

The production compose overlay, the configuration reference, encryption-key rotation, the Azure Container Apps runbook and the deploy workflow's secrets. Day-to-day operations: [architecture/operations.md](architecture/operations.md).

## Before deploying v48

Migration 0048 enforces uniqueness on lower-cased user emails and invite token hashes. If colliding accounts or invitations exist, migration 0048 refuses to boot (exiting non-zero and changing nothing in the database).

Run preflight before deploying:
```bash
bakicli migrate preflight [--dsn "$PAD_DATABASE_URL"]
```
Or inspect the database directly as the `baki_app` role:
```sql
SELECT lower(email) AS address, string_agg(id, ', ' ORDER BY created_at, id) AS ids
FROM users GROUP BY lower(email) HAVING count(*) > 1 ORDER BY 1;

SELECT string_agg(id, ', ' ORDER BY created_at, id) AS ids
FROM org_invites GROUP BY token_hash HAVING count(*) > 1;
```
If duplicate emails exist, compare accounts in the group to decide which account to keep:
```sql
SELECT u.id, u.email, u.email_verified, u.password <> '' AS has_password, u.created_at,
       (SELECT max(t.created_at) FROM refresh_tokens t WHERE t.user_id = u.id) AS last_sign_in,
       (SELECT count(*) FROM identity_links l WHERE l.user_id = u.id) AS sso_links,
       (SELECT count(*) FROM api_tokens a WHERE a.user_id = u.id) AS api_tokens,
       (SELECT count(*) FROM flows f WHERE f.owner_id = u.id) AS flows,
       (SELECT count(*) FROM org_members m WHERE m.user_id = u.id) AS organisations
FROM users u WHERE lower(u.email) = '<address>' ORDER BY u.email_verified DESC, u.created_at, u.id;
```
Prefer a verified account before an unverified one, then sign-ins, SSO links, API tokens, and flows. Then resolve the duplicate group in one transaction (replace `<kept>` with the chosen ID, and `<others>` with a comma-separated list of quoted IDs like `'other-1', 'other-2'`):
```sql
BEGIN;
UPDATE users SET email = 'duplicate+' || id || '@example.invalid', password = '', updated_at = NOW() WHERE id IN (<others>);
UPDATE refresh_tokens SET revoked = TRUE WHERE user_id IN (<others>) AND NOT revoked;
DELETE FROM api_tokens WHERE user_id IN (<others>);
DELETE FROM user_tokens WHERE user_id IN (<others>);
UPDATE identity_links SET user_id = '<kept>' WHERE user_id IN (SELECT id FROM users WHERE id IN (<others>) AND email_verified) AND (SELECT email_verified FROM users WHERE id = '<kept>');
DELETE FROM identity_links WHERE user_id IN (<others>);
UPDATE users SET email = lower(email), updated_at = NOW() WHERE id = '<kept>';
COMMIT;
```
Run the transaction while the previous release is idle, or rerun it a minute later (it is idempotent); a sign-in straddling the commit can otherwise leave a rolling session. For duplicate invite tokens, delete all but one per group (`DELETE FROM org_invites WHERE id IN (<others>);`).

**Outcome if deployed without resolving duplicates:**
- **Docker Compose:** The new container exits non-zero and enters a crash loop under `restart: unless-stopped`. Set `PAD_VERSION` back to the previous tag and run `docker compose up -d`, or execute the resolution SQL above; the next container restart will apply the migration.
- **Azure Container Apps:** In Single revision mode, the previous revision continues serving 100% of traffic while the new revision crash-loops. The deploy workflow fails at the Deploy step after the provisioning timeout (~10 min). The failed revision continues restarting and will automatically apply the migration and take traffic once the data is resolved; to stop that, deactivate the failed revision (`az containerapp revision deactivate`) or redeploy the serving image.

## Production web deploy

```bash
BAKI_SITE_ADDRESS=https://baki.example.com   # public origin; port 80 reachable for ACME
ACME_EMAIL=ops@example.com                   # certificate expiry notices
PAD_ALLOWED_ORIGINS=https://baki.example.com
PAD_AUTH_SECRET=...                          # openssl rand -hex 32
PAD_ENCRYPTION_KEY=...                       # a DIFFERENT openssl rand -hex 32
PAD_METRICS_TOKEN=...                        # openssl rand -hex 32
PAD_DATABASE_URL=postgres://baki_app:...@db:5432/baki?sslmode=require
BAKI_APP_PASSWORD=...                        # must match the password in PAD_DATABASE_URL
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

- The overlay adds a Caddy TLS terminator (automatic ACME, `http://` → `https://`; only ports 80/443 published) and 404s `/metrics` and `/debug/pprof` at the edge — scrape `http://app:8080/metrics` inside the compose network with `PAD_METRICS_TOKEN`. `PAD_BEHIND_PROXY=true` is honest only because Caddy terminates TLS; never set it with nothing in front. Behind your own front end, skip the overlay's proxy and set `PAD_TRUSTED_PROXIES` to that front end yourself.
- `PAD_TRUSTED_PROXIES` must list the real proxy: empty, every client collapses into one rate-limit bucket (1 rps general, 5 logins/minute deployment-wide) and audits record the proxy address.
- The overlay's Postgres is tuned to its 2G/1 CPU limit (`shared_buffers=512MB`, `effective_cache_size=1536MB`, `work_mem=16MB`, `random_page_cost=1.1`, `jit=off`, 256m `/dev/shm`); scale with the memory limit. On an existing volume run `CREATE EXTENSION IF NOT EXISTS pg_stat_statements;` once as superuser. The `app` service gets 2 CPUs — Go 1.25 floors `GOMAXPROCS` at 2; a half-core quota is CFS-throttled.
- Frontend only: `npm run build --prefix frontend` → `frontend/dist/`; set `VITE_API_URL` (backend origin, no trailing `/api`) and `PAD_ALLOWED_ORIGINS`; unset, the bundle uses its own origin (one image works at any hostname).

## Configuration reference

| Variable | Purpose | Default |
|---|---|---|
| `PAD_MODE` | `local` (Tauri sidecar) or `cloud` (web server) | `local` |
| `PAD_CONFIG` | A JSON config file instead of the `PAD_*` variables, which are then ignored | — |
| `PAD_HOST` / `PAD_PORT` | Bind address / port (cloud mode) | `localhost` / ephemeral |
| `PAD_ALLOWED_ORIGINS` | CORS / WebSocket origin allowlist (comma-separated) | — |
| `PAD_AUTH_ENABLED` | Enable JWT auth (required for browser use) | `false` |
| `PAD_AUTH_SECRET` | JWT signing secret (required when auth enabled) | — |
| `PAD_ENCRYPTION_KEY_PREVIOUS` | Key rotation: the keys `PAD_ENCRYPTION_KEY` replaced, comma-separated, newest first | — |
| `PAD_STORAGE` | `local` (desktop, no server store) or `database` (PostgreSQL) | `local` |
| `PAD_DATABASE_URL` | Postgres DSN (required for `database`); `sslmode=verify-full` across a network | — |
| `PAD_DB_ALLOW_BYPASSRLS` | Let a RLS-bypassing role boot in cloud mode, with a warning | `false` |
| `PAD_DB_IDLE_TX_TIMEOUT` | `idle_in_transaction_session_timeout`; must exceed `PAD_REQUEST_TIMEOUT`; `0s` disables | `PAD_REQUEST_TIMEOUT`+1m (`90s`) |
| `PAD_AUDIT_RETENTION_DAYS` | Days the retention sweep keeps audit/history/usage/alerts; `0` keeps forever | `400` cloud, else `0` |
| `PAD_AZURE_STORAGE_ACCOUNT` / `PAD_AZURE_STORAGE_CONTAINER` | Offload flow content to Azure Blob Storage (both required together) | — (off) |
| `PAD_TRUSTED_PROXIES` | Trusted proxy IPs for `X-Forwarded-For` | — |
| `PAD_SCAN_INTERVAL` | Periodic flow re-scan interval (e.g. `1h`); enables drift/regression alerts | — (off) |
| `PAD_NOTIFY_WEBHOOK_URL` | Webhook for governance alerts, signed with `PAD_NOTIFY_WEBHOOK_SECRET` | — |
| `PAD_NOTIFY_TEAMS_URL` / `PAD_NOTIFY_TEAMS_WORKFLOW_URL` | Teams webhook for alerts (incoming-webhook connector is retiring; use Workflows) | — |
| `PAD_NOTIFY_ALLOWED_HOSTS` | Hosts organisation channels may point at: exact hosts or `*.suffix` globs | — (any public host) |
| `PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE` | Show the host allowlist to organisation admins | `false` |
| `PAD_SMTP_HOST` / `PAD_SMTP_PORT` | SMTP relay (587 STARTTLS or 465 TLS) | — / `587` |
| `PAD_SMTP_USERNAME` / `PAD_SMTP_PASSWORD` | SMTP auth credentials | — |
| `PAD_SMTP_MAX_CONNECTIONS` | SMTP sessions open at once (the relay's limit) | `0` (queue bound 32) |
| `PAD_EMAIL_FROM` / `PAD_APP_BASE_URL` | From address for outbound email / public origin for email links | — |
| `PAD_INSTANCE_ID` | This replica's name in logs, traces, `application_name` and job locks | ACA replica name, else host name |
| `PAD_KNOWLEDGE_LEASE_CAP` | Orgs whose knowledge re-index lease one replica may hold; beyond it `409 KNOWLEDGE_BUSY` | `4` |
| `PAD_REDIS_URL` | Shared Redis for more than one replica; unset keeps everything in-process | — (off) |
| `PAD_MAX_REPLICAS` | Max replicas sharing the database; above 1 requires `PAD_REDIS_URL` | `0` (counts as one) |
| `PAD_SHUTDOWN_DRAIN_DELAY` | How long the server answers "draining" before ending open chat streams; at most `5s` | `3s` cloud, else `0s` |

**Azure Blob Storage (optional).** With `PAD_AZURE_STORAGE_ACCOUNT`+`_CONTAINER` set, flow content goes to blobs keyed `flows/{id}/content.v{N}.json` (a rolled-back save leaves an orphan blob, never corruption); superseded blobs are deleted after each save, `DeleteFlow` removes the flow's whole prefix, version snapshots are capped at 50 per flow. Auth is `DefaultAzureCredential` — grant **Storage Blob Data Contributor**; the container must exist (unreachable blob keeps the instance out of rotation via `/readyz` while boot still succeeds). **Never add an age-based blob lifecycle rule: it cannot tell a stale orphan from the live blob of a flow nobody edited recently and would silently destroy flow content.** Azurite / non-MI: `PAD_AZURE_BLOB_CONNECTION_STRING`. Every variable with its default is annotated in `.env.example`.

## Rotating the encryption key

`PAD_ENCRYPTION_KEY` seals provider keys, organisation credentials, channel URLs and signing secrets, CI credentials and stored idempotent responses. Rotate in this order:

1. Rolling deploys only: keep `PAD_ENCRYPTION_KEY`, set `PAD_ENCRYPTION_KEY_PREVIOUS` to the new key, deploy — every replica can open what the new key seals.
2. Set `PAD_ENCRYPTION_KEY` to the new key, `PAD_ENCRYPTION_KEY_PREVIOUS` to the old one, deploy. Reads fall back to the old key, writes use the new; each boot re-seals, one replica at a time.
3. Wait for the log line `secret re-seal complete: every sealed value opens with PAD_ENCRYPTION_KEY` from a post-rollout boot. `remaining` > 0 → restart one replica; `unreadable` → add the missing key or have the owners re-enter those secrets.
4. Remove `PAD_ENCRYPTION_KEY_PREVIOUS` and deploy.

Keys are ≥ 32 characters in cloud mode, no placeholder, no comma (GitHub secret: no spaces around commas). **Rollback:** after step 2 never return to the old key alone — what the new key sealed becomes unreadable; keep the new key as `PAD_ENCRYPTION_KEY`. To undo a rotation, rotate back (old current, new previous), wait for step 3's line, then deploy the older release.

## Azure Container Apps deployment (ops runbook)

`infra/` Bicep provisions the stack in three steps, each producing the next's input (`az group create -n rg-baki-prod -l westeurope` once; the Container App pulls from ACR with its managed identity, so no registry credentials are stored; `.github/workflows/deploy.yml` deploys step 3 automatically — digest-pinned after CI, smoke-tests `/readyz`, rolls back on failure unless the release changed migrations (this release does; see below)):

```bash
az deployment group create -g rg-baki-prod -f infra/registry.bicep --parameters registryName=bakiprod
az deployment group create -g rg-baki-prod -f infra/postgres.bicep \
  --parameters serverName=baki-pg adminPassword="$(openssl rand -base64 24)"
az deployment group create -g rg-baki-prod -f infra/main.bicep \
  --parameters @infra/main.parameters.json \
  --parameters image=bakiprod.azurecr.io/baki-backend@sha256:... authSecret=... databaseUrl=...
```

- **Postgres:** `allowAzureServices` is load-bearing (a public Flexible Server without firewall rules accepts no connections), as is `allowedExtensions` (`PG_TRGM`, `VECTOR`; migration v13 runs `CREATE EXTENSION pg_trgm` unguarded). Run the bootstrap SQL at the bottom of `infra/postgres.bicep`: a dedicated `baki_app` role (`NOSUPERUSER NOBYPASSRLS`, owning the database and `public` schema) — never connect as the administrator. Keep its ctype UTF-8 (`en_US.utf8`, as `infra/postgres.bicep` and compose create it): under `C` pg_trgm counts ASCII letters and digits only, so search terms in other scripts answer `QUERY_TOO_SHORT` or match a name prefix.
- **Blob:** container `flow-content` (`flows/` is a key prefix, not a container); on a bring-your-own account grant **Storage Blob Data Contributor** and create the container yourself. Soft delete, versioning and PITR are provisioned as code; verify with `az storage account blob-service-properties show --account-name <acct> --query "{versioning:isVersioningEnabled, softDelete:deleteRetentionPolicy, pitr:restorePolicy}"`. No lifecycle policy, by design — see the age-based-rule warning above; an orphan backstop must reconcile against the `flows` table's current version, not age.
- **DR:** restore Postgres with `az postgres flexible-server restore` (`geo-restore` for a regional outage) and **restore blob storage too** — superseded blobs are hard-deleted about a minute after each save, so recover post-restore-point saves from blob soft delete (`az storage blob undelete`); `/readyz` does not check content existence. Watch `pad_blob_content_missing_total` (any increase is possible data loss) and `pad_blob_operations_total{status="error"|"throttled"}`.
- **Runtime:** liveness `GET /healthz` (deliberately DB-free), readiness `GET /readyz` (Postgres, blob, Redis when configured; generous threshold for cold-start migrations). In Single revision mode, a revision failing its startup probe (due to a refused migration or lock timeout) fails the Deploy step while the old revision continues serving 100% of traffic; the failed revision retries at each restart and takes traffic if the condition clears. `trustedProxies` is set and matters — empty, every client shares one rate-limit bucket and audits record the ingress; check a real `remote_ip` and narrow the CIDR. One replica, no Redis: `maxReplicas` above 1 without `provisionRedis` is refused at boot and by the deploy workflow. Default size `1.0`/`2.0Gi` — memory must be 2 × cpu in Gi (Go floors `GOMAXPROCS` at 2; `GOMEMLIMIT` derives from `memory`).
- The ACA ingress caps every request at 240 s (WebSocket/SSE excepted), no knob. Knowledge re-indexes chunk around it; uploads do not (one request embeds up to 2 500 chunks) — split large documents.

## GitHub secrets for the deploy workflow

`.github/workflows/deploy.yml` authenticates with OIDC federated credentials (no client secret): an app federated to this repository with **Contributor** on the resource group and **AcrPush** on the registry. The image is built once and promoted (staging and production deploy the same digest), so the build job is not bound to a GitHub Environment and the secrets split in two. Repository-level (read by the build job): `AZURE_CLIENT_ID` / `AZURE_TENANT_ID` / `AZURE_SUBSCRIPTION_ID` (OIDC login for `azure/login`) and `AZURE_CONTAINER_REGISTRY` (ACR **name**, not login server). Environment-level (`staging`, `production`; the deploy job also reads the repository-level three):

| Secret | Purpose |
|---|---|
| `AZURE_RESOURCE_GROUP` | Target resource group (must exist; the Bicep is RG-scoped) |
| `AZURE_CONTAINER_APP_NAME` | `appName` for the template |
| `PAD_AUTH_SECRET` | JWT signing key, ≥ 32 chars |
| `PAD_ENCRYPTION_KEY` | AES-256-GCM keystore key; unset it falls back to `PAD_AUTH_SECRET`, and rotating that then makes stored provider keys unreadable |
| `PAD_ENCRYPTION_KEY_PREVIOUS` | Only during a rotation (see above); delete when done |
| `PAD_DATABASE_URL` | DSN for the dedicated `baki_app` role |
| `PAD_METRICS_TOKEN` | Gates `GET /metrics` and `/debug/pprof` |
| `ALERT_EMAILS` | Comma-separated recipients; **empty provisions no alerting at all** |
| `PAD_ALLOWED_ORIGINS` | Only for a custom domain; defaults to the ingress FQDN |
| `PAD_SMTP_HOST` / `PAD_SMTP_USERNAME` / `PAD_SMTP_PASSWORD` / `PAD_EMAIL_FROM` / `PAD_APP_BASE_URL` | Transactional email; without SMTP, password-reset links go only to the log |
| `PAD_SSO_ISSUER` / `PAD_SSO_CLIENT_ID` / `PAD_SSO_CLIENT_SECRET` / `PAD_SSO_REDIRECT_URL` | OIDC SSO; issuer + client-id + redirect required together or SSO stays off |
| `PAD_CI_WEBHOOK_SECRET` | Deprecated deployment-wide CI key; leave unset (org CI credentials / `ci`-scope API tokens instead) |
| `AZURE_STORAGE_ACCOUNT_ID` | Bring your own storage account; grant the blob role and create `flow-content` yourself |

Repository / environment **variables** (not secrets; empty leaves the app default): `PAD_AZURE_FOUNDRY_ALLOWED_HOSTS` (extra Foundry endpoint allow-list hosts, exact or `*.suffix`; globs over a public suffix or multi-tenant zone refuse boot), `PAD_AI_ALLOW_PRIVATE_EGRESS` (`true` allows private-address AI endpoints; needs VNet integration the template does not provision), `PAD_EMBEDDING_PROVIDER` (deployment-wide embedding provider: `openai`, `gemini`, `glm`, `github-models` or `azure-foundry`; anything else refuses to boot), `PAD_EMBEDDING_DIM` (default 1536, never above the model's native width; changing it on a populated knowledge base needs a re-index). Preview an infrastructure change: `az deployment group what-if -g rg-baki-scratch -f infra/main.bicep --parameters @infra/main.parameters.json`; CI compiles every `infra/*.bicep` on each push and fails on warnings.
