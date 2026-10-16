#!/bin/bash
# Creates the unprivileged role the application connects as.
#
# WHY THIS EXISTS
#
# Postgres Row-Level Security is ignored entirely by a superuser, and by any
# role with BYPASSRLS. FORCE ROW LEVEL SECURITY does not change that — it only
# removes the *table owner's* exemption. So connecting the app as `postgres`
# (which this compose file used to do by default) turns off every RLS policy in
# the schema at once, silently: queries still succeed, and the policies are
# still visible in the catalog.
#
# That matters because RLS is the second of two layers. Several storage methods
# rely on it as defense-in-depth behind an explicit WHERE clause. When it is
# off, any gap in the Go layer is the whole story rather than half of it.
#
# The app refuses to start in cloud mode when it detects a bypassing role, and
# logs a warning in local mode (checkRLSRole in
# internal/storage/database/postgres_storage.go; PAD_DB_ALLOW_BYPASSRLS=true
# turns the refusal into the warning).
#
# WHY THIS IS A SHELL SCRIPT AND NOT A .sql FILE
#
# It used to be 01-app-role.sql, with the role's password written into it as a
# literal: PASSWORD 'baki_app'. docker-compose.prod.yml inherits this mount from
# the base file, so a production stack initialised from an empty volume got an
# application role whose password is published in this repository — while
# .env.example told operators to put a real password in PAD_DATABASE_URL, which
# would then simply fail to authenticate. A .sql file cannot read the
# environment; a .sh file can, so the password is now an input.
#
# WHEN THIS RUNS
#
# Only on FIRST initialisation of an empty data volume — that is how the
# postgres image's /docker-entrypoint-initdb.d works. An existing deployment
# keeps its old superuser DSN and its postgres-owned tables; see "EXISTING
# DEPLOYMENTS" at the bottom for the manual steps.
set -euo pipefail

# Required, with no default. A default here would be the same published
# credential in a new costume: docker-compose.yml supplies a throwaway value for
# local development explicitly, and docker-compose.prod.yml makes the variable
# mandatory, so an operator who has set nothing gets this error rather than a
# silently weak role.
if [ -z "${BAKI_APP_PASSWORD:-}" ]; then
    echo "db-init: BAKI_APP_PASSWORD is not set — refusing to create the application role with a default password." >&2
    echo "db-init: set it on the db service (docker-compose.yml supplies a local-dev value; prod requires your own)." >&2
    exit 1
fi

# -v pw=... with :'pw' (and -v db=... with :"db") lets psql do the quoting, so a
# password containing a quote or a backslash cannot break out of the statement.
# Note these must be psql variables: psql does not expose shell environment
# variables to :name interpolation.
psql -v ON_ERROR_STOP=1 -v pw="$BAKI_APP_PASSWORD" -v db="$POSTGRES_DB" \
    --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-'EOSQL'
    -- NOSUPERUSER NOBYPASSRLS are the two that matter; NOCREATEDB/NOCREATEROLE
    -- are ordinary least privilege.
    CREATE ROLE baki_app
        LOGIN PASSWORD :'pw'
        NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;

    -- The app runs its own migrations, which CREATE TABLE and then
    -- ALTER TABLE ... FORCE ROW LEVEL SECURITY. FORCE requires table ownership,
    -- and the creating role owns what it creates — so baki_app must own the
    -- database and the schema it builds into.
    ALTER DATABASE :"db" OWNER TO baki_app;
    ALTER SCHEMA public OWNER TO baki_app;
    GRANT ALL ON SCHEMA public TO baki_app;

    -- pgvector backs knowledge-base similarity search. `vector` is NOT a
    -- trusted extension, so a non-superuser cannot install it — it has to
    -- happen here, while we are still the superuser. (pg_trgm, used by library
    -- content search, IS trusted as of PG13, so the migration installs that one
    -- itself.)
    --
    -- Migration v20 detects the extension and builds the HNSW index; without
    -- this line it no-ops and knowledge search falls back to loading chunks
    -- into Go and ranking them there — correct, but far slower, and nothing
    -- says so.
    CREATE EXTENSION IF NOT EXISTS vector;
EOSQL

echo "db-init: created unprivileged role baki_app and handed it ownership of $POSTGRES_DB"

# EXISTING DEPLOYMENTS
#
# This file does not run against a volume that is already initialised. To
# migrate one, connect as the superuser and run the equivalent by hand:
#
#   CREATE ROLE baki_app LOGIN PASSWORD '<a real password>'
#       NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
#   ALTER DATABASE baki OWNER TO baki_app;
#   \connect baki
#   CREATE EXTENSION IF NOT EXISTS vector;
#   ALTER SCHEMA public OWNER TO baki_app;
#   REASSIGN OWNED BY postgres TO baki_app;   -- hands over the existing tables
#
# then repoint PAD_DATABASE_URL at baki_app and restart. Verify with:
#
#   SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'baki_app';
#
# both must be false. Until they are, a cloud-mode app refuses to start (or,
# with PAD_DB_ALLOW_BYPASSRLS=true, warns about RLS being bypassed).
#
# A deployment already initialised from the OLD .sql file has the published
# password 'baki_app'. Rotate it:
#
#   ALTER ROLE baki_app PASSWORD '<a real password>';
#
# and update PAD_DATABASE_URL to match.
