# Disaster Recovery Runbook — baki (PAD Analyzer) cloud backend

**Scope:** recovery of the cloud deployment (Azure Container Apps + Azure Database
for PostgreSQL + Azure Blob Storage). Desktop/Tauri mode stores everything
locally and is out of scope.

**Source of truth:** the PostgreSQL database. Blob storage holds only offloaded
flow *content* (large JSON), keyed by row + version; the DB can reconstruct blob
references but blob cannot reconstruct the DB. **Recover the database first.**

---

## 1. Objectives (RPO / RTO)

| Target | Goal | Mechanism |
|--------|------|-----------|
| **RPO** (data loss) | ≤ 5 min of committed writes | PostgreSQL continuous backup + PITR |
| **RTO** (downtime) | ≤ 1 h for region-local restore; ≤ 4 h for geo-failover | PITR / geo-restore + ACA redeploy |
| **Retention** | 14 days point-in-time + geo-redundant copy | Flexible Server backup config |

These are targets, reviewed after each drill (§6). Adjust the retention in
`infra/postgres.bicep` (`backupRetentionDays`) and the matching runbook if the
compliance window changes.

---

## 2. What is backed up, and where

| Store | Contents | Backup mechanism | Configured in |
|-------|----------|------------------|---------------|
| **PostgreSQL** | users, orgs, flows (metadata), versions, analysis, triage, baselines, conversations, audit, tokens, settings | Azure DB for PostgreSQL Flexible Server automated backups (full + continuous incremental), optional geo-redundant | `infra/postgres.bicep` (`geoRedundantBackup`, `backupRetentionDays`) |
| **Blob Storage** | flow content JSON (`flows/{flowId}/content.v{N}.json`) | Blob **soft-delete** (14 d) + **versioning** | storage account — see [README §Azure Blob](../README.md) |
| **Container image** | the app binary | GHCR (immutable by digest) + `deploy.yml` | `.github/workflows/deploy.yml` |
| **IaC / secrets** | Bicep, GitHub Actions secrets → Container App secrets | Git history + GitHub secrets | `infra/`, GitHub secrets |

> The database schema is **not** separately backed up: `main()` runs
> `migrate()` on boot under an advisory lock and applies any pending
> `schema_migrations` (see `internal/storage/database/postgres_migrations.go`).
> A restored server therefore self-heals its schema on first connect — never
> restore a schema dump onto a production server.

---

## 3. Confirm backups are running (verification)

Before an incident, confirm the backup policy is effective:

```bash
RG=<resource-group>
SERVER=<postgres-server-name>

# Backup retention + geo-redundancy as deployed:
az postgres flexible-server show -g $RG -n $SERVER \
  --query "{retentionDays:backup.backupRetentionDays, geoRedundant:backup.geoRedundantBackup}" -o table

# Recent backup activity (look for completed full/incremental entries):
az postgres flexible-server backup list -g $RG --cluster-name $SERVER -o table
```

For blob soft-delete/versioning (cross-check the [README lifecycle section](../README.md)):

```bash
ACCT=<storage-account>
az storage account blob-service-properties show -n $ACCT --query "{deleteRetentionPolicy, isVersioningEnabled}"
```

Set an alert on `pad_blob_operations_total{status="error"}` (already exported)
for blob-side data-loss signals; the DB side surfaces via the Azure Service
Health + resource diagnostics for the Flexible Server.

---

## 4. Restore procedures

> All restores create a **new** server (Flexible Server restores are
> non-destructive — they never overwrite the source). Cutover by repointing the
> app's `PAD_DATABASE_URL`.

### 4a. Point-in-time restore (PITR) — accidental delete / bad write

Use when data was damaged within the retention window but the region is healthy.

```bash
SOURCE=<postgres-server-name>
TARGET=$SOURCE-pitr-$(date +%Y%m%d-%H%M)
RG=<resource-group>
# Restore to 5 minutes before the incident (max 35 days back, min ~10 min ago):
RESTORE_TIME="2026-06-25T12:55:00Z"

az postgres flexible-server restore \
  --resource-group $RG \
  --source-server-name $SOURCE \
  --name $TARGET \
  --restore-time "$RESTORE_TIME" \
  --yes
```

Wait for `Succeeded` (`az postgres flexible-server show -n $TARGET -g $RG --query state`).
The restored server keeps the source's AAD admin, firewall, and configurations.

### 4b. Geo-restore — regional outage

Use when the primary region is unavailable and geo-redundant backups were enabled
(`geoRedundantBackup: Enabled` in `infra/postgres.bicep`).

```bash
TARGET=$SOURCE-geo-$(date +%Y%m%d)
az postgres flexible-server geo-restore \
  --resource-group <recovery-rg> \
  --name $TARGET \
  --source-server-name <fully-qualified-source-server-id> \
  --location <paired-region> \
  --yes
```

### 4c. Deleted-server restore

A dropped server can be recovered from its geo-backup within the retention
window:

```bash
az postgres flexible-server deleted-server list -o table
az postgres flexible-server deleted-server restore \
  --resource-group <recovery-rg> --name $TARGET \
  --deleted-server-name <source> --location <region> --yes
```

---

## 5. Application cutover after a DB restore

1. **Point the app at the restored server.** Update `PAD_DATABASE_URL` in Key
   Vault (secret `pad-database-url`) and/or the GitHub secret, then re-run the
   Deploy workflow or restart the Container App revision:
   ```bash
   az containerapp revision restart -n <app> -g <rg>
   ```
2. **Schema self-heals on boot** — `migrate()` applies any pending
   `schema_migrations`. Watch the first boot logs for `schema migrations complete`.
3. **Blob needs its OWN restore — do not skip this.** The DB is the source of
   truth for *which* blob a flow points at, but not for the blob's existence.
   Superseded content blobs are hard-deleted about a minute after each successful
   save (`staleBlobCleanupDelay` in `internal/storage/database/postgres_flows.go`).

   So after a point-in-time restore to time *T*, every flow that was saved **after**
   *T* has a row pointing at `content.v{N}` that was deleted long ago. The rows
   restore; the content does not come back with them.

   Recover it from blob **soft-delete** (14-day window, `infra/main.bicep`) or from
   blob versioning, in the same maintenance window as the DB restore:
   ```bash
   # list soft-deleted blobs for the affected prefix
   az storage blob list --account-name <acct> --container-name flow-content \
     --prefix "flows/" --include d --auth-mode login -o table
   # undelete a flow's content
   az storage blob undelete --account-name <acct> --container-name flow-content \
     --name "flows/<flowId>/content.v<N>.json" --auth-mode login
   ```
   An earlier version of this step said "a restored DB never references a blob that
   doesn't exist for its version" and pointed at an age-based lifecycle rule. Both
   were wrong, and the combination was dangerous: it told the operator there was
   nothing to do, so the content aged out of the soft-delete window permanently.
   There is no age-based lifecycle rule — see [README](../README.md) step 4 for why
   one must not be added.
4. **Verify** end-to-end. `GET /readyz` is **not** sufficient on its own: it checks
   that the DB, blob and Redis are *reachable*, not that any particular flow's
   content still exists, so it returns 200 while flows saved after *T* are empty.
   Open a flow known to have been saved shortly before the incident and confirm it
   loads and analyzes, and watch `pad_blob_content_missing_total` — any increase
   means a row's content blob is gone.

---

## 6. Restore drill (quarterly)

Run this against a **non-production** resource group to prove the procedure
works and the RTO is real. Do not drill against production.

1. PITR the prod server to a dev RG per §4a, using a timestamp ~1 h old.
2. Deploy the app to the dev RG pointed at the restored DB (`infra/main.bicep`).
3. Smoke-test: register/login disabled or seeded; load one flow per tenant;
   run analysis; confirm findings/triage/baselines survived.
4. Time the full sequence (restore start → `/readyz` 200) and record it against
   the RTO target (§1). File an issue if it exceeds the target.
5. Tear down the dev RG.

Drill outcome goes into the post-mortum doc; update this runbook if any step was
wrong, missing, or slower than the target.

---

## 7. Break-glass

- **No admin can log in (zero-admin lockout):** the first registered user is
  auto-promoted to admin, and `PUT /api/admin/users/{id}/role` lets an existing
  admin promote others. If *zero* admins remain (no break-glass via the app),
  connect to the restored DB directly and set a user's `role = 'admin'`:
  ```sql
  UPDATE users SET role = 'admin' WHERE email = '<known-owner>';
  ```
- **Lost JWT signing secret (`PAD_AUTH_SECRET`):** all outstanding access/refresh
  tokens become unverifiable; every user must re-login. Rotate the
  `PAD_AUTH_SECRET` GitHub secret and re-run `deploy.yml` (it becomes a
  Container App secret). The AES keystore key is the *same*
  secret — rotating it invalidates encrypted provider keys (users must re-enter
  AI provider keys); see `internal/storage/database/keystore.go`.

### 7a. Secret rotation impact (H19)

The code paths below are affected by secret rotation in ways that may not be
obvious from the break-glass notes. **Read this table before rotating.**

| Secret rotated | Immediate user impact | Recovery action |
|---|---|---|
| `PAD_AUTH_SECRET` | Every outstanding JWT (access + refresh) is invalid → all users force-logged-out, all in-flight chat streams abort, all SSE channels drop. | Users re-authenticate. **Cascading impact:** when `PAD_ENCRYPTION_KEY` is UNSET (the legacy default) the keystore uses the auth secret as its AES key, so provider API keys ALSO become undecryptable — see next row. |
| `PAD_ENCRYPTION_KEY` | Every stored provider API key (Settings → Providers) becomes undecryptable. Users see "provider key missing" until they re-enter credentials. | Each user re-enters provider credentials. |
| `PAD_SSO_CLIENT_SECRET` | New SSO logins fail until the running process restarts (the secret is read at boot). Existing sessions unaffected. | Update the GitHub secret and redeploy (the value is baked into the revision). |
| Managed Identity (Postgres / Blob) | Workload-token cache flushes on next request — brief latency spike, no user-visible failure. | None. |

### 7b. Zero-downtime rotation (forward path)

The keystore does **not** currently support dual-key decryption. To rotate
without bricking users, the path is:

1. Ship a code change that prefixes ciphertext with a key-version byte and
   supports decrypting with a "previous key" in-memory.
2. Deploy with both old + new keys configured.
3. Wait for an organic re-encryption sweep (or trigger one via an admin
   endpoint).
4. Deploy again with only the new key.

Tracked separately as a Phase 4 larger refactor.

---

## 8. Related

- Backup/IaC config: [`infra/postgres.bicep`](../infra/postgres.bicep),
  [`infra/main.bicep`](../infra/main.bicep).
- Schema migration behaviour:
  [`internal/storage/database/postgres_migrations.go`](../internal/storage/database/postgres_migrations.go).
- Blob soft-delete/versioning/lifecycle: [README §Azure Blob Storage](../README.md).
