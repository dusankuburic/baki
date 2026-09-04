-- Creates the unprivileged role the application connects as.
--
-- WHY THIS EXISTS
--
-- Postgres Row-Level Security is ignored entirely by a superuser, and by any
-- role with BYPASSRLS. FORCE ROW LEVEL SECURITY does not change that — it only
-- removes the *table owner's* exemption. So connecting the app as `postgres`
-- (which this compose file used to do by default) turns off every RLS policy in
-- the schema at once, silently: queries still succeed, and the policies are
-- still visible in the catalog.
--
-- That matters because RLS is the second of two layers. Several storage methods
-- rely on it as defense-in-depth behind an explicit WHERE clause. When it is
-- off, any gap in the Go layer is the whole story rather than half of it.
--
-- The app logs a warning at startup when it detects a bypassing role
-- (warnIfRLSBypassed in internal/storage/database/postgres_storage.go).
--
-- WHEN THIS RUNS
--
-- Only on FIRST initialisation of an empty data volume — that is how the
-- postgres image's /docker-entrypoint-initdb.d works. An existing deployment
-- keeps its old superuser DSN and its postgres-owned tables; see "EXISTING
-- DEPLOYMENTS" at the bottom for the manual steps.

-- NOSUPERUSER NOBYPASSRLS are the two that matter; NOCREATEDB/NOCREATEROLE are
-- ordinary least privilege.
CREATE ROLE baki_app
    LOGIN PASSWORD 'baki_app'
    NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;

-- The app runs its own migrations, which CREATE TABLE and then
-- ALTER TABLE ... FORCE ROW LEVEL SECURITY. FORCE requires table ownership, and
-- the creating role owns what it creates — so baki_app must own the database
-- and the schema it builds into.
ALTER DATABASE baki OWNER TO baki_app;

\connect baki

ALTER SCHEMA public OWNER TO baki_app;
GRANT ALL ON SCHEMA public TO baki_app;

-- pgvector backs knowledge-base similarity search. `vector` is NOT a trusted
-- extension, so a non-superuser cannot install it — it has to happen here,
-- while we are still the superuser. (pg_trgm, used by library content search,
-- IS trusted as of PG13, so the migration installs that one itself.)
--
-- Migration v20 detects the extension and builds the HNSW index; without this
-- line it no-ops and knowledge search falls back to loading chunks into Go and
-- ranking them there — correct, but far slower, and nothing says so.
CREATE EXTENSION IF NOT EXISTS vector;

-- EXISTING DEPLOYMENTS
--
-- This file does not run against a volume that is already initialised. To
-- migrate one, connect as the superuser and run the equivalent by hand:
--
--   CREATE ROLE baki_app LOGIN PASSWORD '<a real password>'
--       NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
--   ALTER DATABASE baki OWNER TO baki_app;
--   \connect baki
--   CREATE EXTENSION IF NOT EXISTS vector;
--   ALTER SCHEMA public OWNER TO baki_app;
--   REASSIGN OWNED BY postgres TO baki_app;   -- hands over the existing tables
--
-- then repoint PAD_DATABASE_URL at baki_app and restart. Verify with:
--
--   SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'baki_app';
--
-- both must be false, and the app's startup log must no longer warn about RLS
-- being bypassed.
