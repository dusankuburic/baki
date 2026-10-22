#!/usr/bin/env bash
# Seeds the local compose database with an account that can always sign in.
#
# LOCAL DEVELOPMENT ONLY.
#
# The compose stack has no mail provider, so a sign-up stays unverified for ever and login answers
# `email_not_verified`; "forgot password" has nowhere to send a link either. This puts a verified
# admin account back into whatever database is running, and is safe to re-run: an existing account
# with the same address keeps its id and gets the password, the admin role and a verified address.
#
#   ./scripts/dev-seed.sh                       # the defaults in scripts/dev-seed/main.go
#   DEV_SEED_EMAIL=me@example.com DEV_SEED_PASSWORD=... ./scripts/dev-seed.sh
#
# Run it after the app container is healthy: the schema is created by the app's migrations at boot,
# not by the database image's init scripts.
set -euo pipefail

cd "$(dirname "$0")/.."

service="${DEV_SEED_DB_SERVICE:-db}"
dbuser="${DEV_SEED_DB_USER:-${POSTGRES_USER:-postgres}}"
dbname="${DEV_SEED_DB_NAME:-${POSTGRES_DB:-baki}}"

if ! docker compose ps --status running --services | grep -qx "$service"; then
  echo "dev-seed: the '$service' service is not running — start the stack first (docker compose up -d)" >&2
  exit 1
fi

if ! docker compose exec -T "$service" psql -U "$dbuser" -d "$dbname" -tAc "select to_regclass('public.users')" | grep -q users; then
  echo "dev-seed: the users table does not exist yet — wait for the app container to finish migrating" >&2
  exit 1
fi

go run ./scripts/dev-seed | docker compose exec -T "$service" psql -U "$dbuser" -d "$dbname" -v ON_ERROR_STOP=1
