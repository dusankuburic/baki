# Release Notes: v48 / v49 Schema Upgrades and Operational Resilience

## Summary
This release introduces migrations 0048 (`integrity_constraints`) and 0049 (`index_upkeep`), adds automated preflight checks and drift reconciliation tooling, optimizes migration lock acquisition against autovacuum workers, formalizes database connection pool sizing floors, and documents the first 400-day data retention sweep.

---

### 1. Integrity Constraints & Preflight Checks (Migration 0048)
- **Case-Insensitive Email & Invite Uniqueness:** Enforces uniqueness on `lower(email)` for user accounts and `token_hash` for organization invitations.
- **Foreign Key Cascades:** Replaces manual cascade logic with database-enforced foreign keys (`ON DELETE CASCADE`) for `knowledge_documents`, `policies`, `org_channels`, `org_custom_rules`, and `refresh_tokens`.
- **Pre-Deploy Preflight:** Operators should run `bakicli migrate preflight [--dsn ...]` prior to deploying. If duplicate email accounts or colliding invite tokens exist, migration 0048 safely refuses boot (exits non-zero without modifying the database). Resolving duplicates is performed via the documented idempotent SQL recipe in [docs/deployment.md](deployment.md#before-deploying-v48).

### 2. Autovacuum Displacement & Migration Lock Slicing
- **Worker Cancellation:** The all-or-none table lock blocks in migrations 0048 and 0049 detect active `ShareUpdateExclusiveLock` holders in `current_database()` (such as PostgreSQL autovacuum workers). When detected, lock wait slices extend to `deadlock_timeout + 200ms` (~1.2 s), prompting PostgreSQL to cancel the autovacuum task and grant the migration lock.
- **Sign-In Latency Bound:** Sign-in transactions queued behind migration lock acquisition wait at most 200 ms under normal conditions, and up to ~1.2 s only when autovacuum displacement occurs.

### 3. Database Connection Pool Floor
- **Floor of 3 Connections:** `PAD_DB_MAX_OPEN_CONNS` must be set to at least 3. The post-boot schema upkeep job and the HNSW vector index build each pin a connection; pools smaller than 3 skip these maintenance tasks with a warning at boot.

### 4. Migration Rollback & Tooling
- **Rollback Binary Requirement:** Down migrations (`bakicli migrate down <target>`) must be executed using the release artifact or `go run ./cmd/bakicli` of the release that added the migration. The distroless application container image ships without `bakicli`.
- **Checksum Drift Adoption:** Environments that applied pre-release migration drafts with matching schema shapes can reconcile checksums using `bakicli migrate adopt-checksum <version>`.

### 5. Data Lifecycle & Retention Sweep
- **400-Day Retention Sweep:** In cloud mode, `PAD_AUDIT_RETENTION_DAYS` defaults to 400 days. The initial post-upgrade sweep purges historical audit records, analysis history, usage metrics, and alerts older than 400 days in bounded batches of 5,000 rows.
- **Orphan Cleanup Notices:** Migration 0048 cleans up existing orphan policies, channels, and custom rules, reporting counts as PL/pgSQL notices in the PostgreSQL server log (`docker compose logs db`), rather than the application log.
