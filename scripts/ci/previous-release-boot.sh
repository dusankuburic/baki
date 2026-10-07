#!/usr/bin/env bash
# Boots the previous release's backend on the schema this revision migrates to, the way an image rollback would,
# and fails when it does not come up (docs/adr/0002-migration-compatibility-and-rollback.md). CI runs it in the
# previous-release-boot job of .github/workflows/ci.yml; it runs the same way on a workstation.
#
# Usage: scripts/ci/previous-release-boot.sh PREVIOUS_BINARY NEW_BINARY
#
#   BOOT_DATABASE_URL  required: an EMPTY database owned by the unprivileged app role, with the vector extension
#                      installed (recipe in docs/architecture/safety-nets.md). Refused when it already holds a
#                      schema or is named baki, the compose database.
#   BOOT_PORT          the boots listen on 127.0.0.1, ports BOOT_PORT to BOOT_PORT+2 (default 18080)
#   BOOT_TIMEOUT       seconds a boot may take to answer /readyz (default 120)
#   BOOT_LOG_DIR       where the boot logs go (default: a new temporary directory)
#
# Needs curl and psql on PATH. The binaries may be Windows .exe files.
#
# Three boots against the one database, in the order production meets them:
#   1. the previous binary migrates the empty database: the schema before the deploy;
#   2. the new binary migrates it forward: the deploy;
#   3. the previous binary boots again: the rollback, which must answer /readyz within BOOT_TIMEOUT.
# Boot 1 also shows, by behaviour rather than by reading its source, whether the previous binary records
# schema_migrations.compat. A binary that predates the column (Phase 0a) refuses every newer schema, so a schema
# that moved ahead of it is reported as nothing to check.
#
# Exit status: 0 when the rollback boots or there is nothing to check, 1 when it does not, 2 on a usage or
# environment error.
set -euo pipefail

usage() {
    echo "usage: $0 PREVIOUS_BINARY NEW_BINARY (with BOOT_DATABASE_URL set; see the header of this script)" >&2
    exit 2
}

in_actions() { [ "${GITHUB_ACTIONS:-}" = true ]; }

# annotate LEVEL WORDS...: one line, a GitHub annotation in Actions and a plain line elsewhere.
annotate() {
    local level=$1
    shift
    if in_actions; then
        printf '::%s::%s\n' "$level" "$*"
    else
        printf '%s: %s\n' "$(printf '%s' "$level" | tr '[:lower:]' '[:upper:]')" "$*"
    fi
}

die() {
    annotate error "$@"
    exit 2
}

group() { if in_actions; then echo "::group::$1"; else echo "----- $1"; fi; }
endgroup() { if in_actions; then echo "::endgroup::"; else echo "-----"; fi; }

[ $# -eq 2 ] || usage
previous_bin=$1
new_bin=$2
for bin in "$previous_bin" "$new_bin"; do
    [ -f "$bin" ] || die "no binary at $bin"
done
[ -n "${BOOT_DATABASE_URL:-}" ] ||
    die "BOOT_DATABASE_URL is not set: point it at an empty database (docs/architecture/safety-nets.md)"
command -v psql >/dev/null || die "psql is not on PATH"
command -v curl >/dev/null || die "curl is not on PATH"

port=${BOOT_PORT:-18080}
timeout=${BOOT_TIMEOUT:-120}
# A boot that answered /readyz must still be up this many seconds later, after the work it starts at boot (the
# retention sweep, the encryption backfill, the vector index build) has queried the schema.
settle=3
if [ -n "${BOOT_LOG_DIR:-}" ]; then
    log_dir=$BOOT_LOG_DIR
    mkdir -p "$log_dir"
else
    log_dir=$(mktemp -d "${TMPDIR:-/tmp}/previous-release-boot.XXXXXX")
fi
log1=$log_dir/1-previous-on-empty-database.log
log2=$log_dir/2-new-migrating-forward.log
log3=$log_dir/3-previous-on-new-schema.log
rm -f "$log1" "$log2" "$log3"

# sql QUERY: prints the rows, one per line, columns separated by a space.
sql() {
    psql "$BOOT_DATABASE_URL" -X -q -A -t -F ' ' -v ON_ERROR_STOP=1 -c "$1" | tr -d '\r'
}

# The schema version, and whether schema_migrations has the compat column (Phase 0a on).
schema_state="SELECT (SELECT COALESCE(MAX(version), 0) FROM schema_migrations),
    EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('schema_migrations')
        AND attname = 'compat' AND NOT attisdropped)"

# Refuse anything but an empty, throwaway database: the boots migrate it, and boot 1 must start from nothing.
db_state=$(sql "SELECT current_database(), to_regclass('schema_migrations') IS NOT NULL") ||
    die "cannot query BOOT_DATABASE_URL with psql (see the error above)"
read -r db_name db_has_schema <<<"$db_state"
[ "$db_name" != baki ] ||
    die "refusing the database named baki, the compose database: give the check a throwaway one"
[ "$db_has_schema" = f ] ||
    die "database $db_name already holds a schema (schema_migrations exists): the check needs an empty database"

# The boots get production's settings (infra/main.bicep) minus what this environment cannot have: TLS to Postgres,
# blob storage and Redis. Inherited PAD_* variables are dropped so a workstation's own settings cannot leak in, and
# the secrets are made up per run but shared by the three boots, as one deployment's are.
for name in $(compgen -e); do
    case $name in PAD_*) unset "$name" ;; esac
done
random_secret() { od -An -N24 -tx1 /dev/urandom | tr -d ' \n'; }
PAD_AUTH_SECRET=$(random_secret)
PAD_ENCRYPTION_KEY=$(random_secret)
export PAD_AUTH_SECRET PAD_ENCRYPTION_KEY
export PAD_MODE=cloud PAD_HOST=127.0.0.1 PAD_STORAGE=database PAD_AUTH_ENABLED=true PAD_BEHIND_PROXY=true \
    PAD_DB_REQUIRE_SSL=false PAD_AUDIT_SPILL_DIR=off PAD_DATABASE_URL="$BOOT_DATABASE_URL"

pid=
# stop: stops the running binary, if any (SIGTERM, which fx drains within 25 s; SIGKILL after 30 s).
stop() {
    [ -n "$pid" ] || return 0
    kill -TERM "$pid" 2>/dev/null || true
    local waited=0
    while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt 30 ]; do
        sleep 1
        waited=$((waited + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid" 2>/dev/null || true; fi
    wait "$pid" 2>/dev/null || true
    pid=
}
trap stop EXIT

ready() { curl -fs --noproxy '*' -o /dev/null --max-time 2 "http://127.0.0.1:$1/readyz"; }

# boot LABEL BINARY PORT LOG: starts BINARY and waits until it answers /readyz. Returns 0 with it still running, or
# 1 when it exits, fails to answer within the timeout, or goes away right after answering (stopped either way).
boot() {
    local label=$1 bin=$2 port=$3 log=$4
    if curl -s --noproxy '*' -o /dev/null --max-time 2 "http://127.0.0.1:$port/"; then
        die "something already answers on port $port: set BOOT_PORT to a free range"
    fi
    echo "$label: starting $bin on port $port (log: $log)"
    local started=$SECONDS
    PAD_PORT=$port "$bin" >"$log" 2>&1 &
    pid=$!
    until ready "$port"; do
        if ! kill -0 "$pid" 2>/dev/null; then
            local status=0
            wait "$pid" || status=$?
            pid=
            echo "$label: exited with status $status before answering /readyz"
            return 1
        fi
        if [ $((SECONDS - started)) -ge "$timeout" ]; then
            echo "$label: no answer from /readyz within ${timeout}s"
            stop
            return 1
        fi
        sleep 1
    done
    sleep "$settle"
    if ! kill -0 "$pid" 2>/dev/null || ! ready "$port"; then
        echo "$label: answered /readyz, then went away within ${settle}s"
        stop
        return 1
    fi
    echo "$label: ready after $((SECONDS - started))s"
    schema_lines "$log"
}

# schema_lines LOG: the binary's own account of the schema it found, for the record.
schema_lines() {
    grep -E 'schema (up to date|migrations complete|is ahead|is at)|newer than' "$1" | sed 's/^/    /' || true
}

# show_logs: the boot logs without fx's dependency-graph trace (its errors stay); the full files are in $log_dir.
fx_trace='^\[Fx\] ((PROVIDE|DECORATE|SUPPLY|REPLACE|INVOKE|BEFORE RUN|RUN|RUNNING)([[:space:]]|$)'
fx_trace+='|HOOK On(Start|Stop).*(executing|ran successfully))'
show_logs() {
    local f
    for f in "$log1" "$log2" "$log3"; do
        [ -s "$f" ] || continue
        group "$(basename "$f")"
        grep -v -E "$fx_trace" "$f" | tail -n 300 || true
        endgroup
    done
    echo "Full logs: $log_dir"
}

echo "Previous release: $previous_bin"
echo "This revision:    $new_bin"
echo "Database:         $db_name (empty)"

if ! boot "1/3 previous release on the empty database" "$previous_bin" "$port" "$log1"; then
    show_logs
    annotate error "The previous release does not boot on an empty database here, so the rollback cannot be" \
        "checked. That points at the environment scripts/ci/previous-release-boot.sh sets up, not at the new" \
        "schema: see its log."
    exit 1
fi
stop
state=$(sql "$schema_state") || die "cannot read schema_migrations after the previous release's boot"
read -r prev_version prev_has_compat <<<"$state"

if ! boot "2/3 this revision migrating forward (the deploy)" "$new_bin" "$((port + 1))" "$log2"; then
    show_logs
    annotate error "This revision does not boot on the schema the previous release left (v$prev_version)," \
        "so deploying it would fail: see its log."
    exit 1
fi
stop
state=$(sql "$schema_state") || die "cannot read schema_migrations after this revision's boot"
read -r new_version new_has_compat <<<"$state"
compat=compat
[ "$new_has_compat" = t ] || compat="'breaking'" # steps recorded without the column count as breaking, as in the guard
newer=$(sql "SELECT version, name, $compat FROM schema_migrations WHERE version > $prev_version ORDER BY version") ||
    die "cannot list the steps newer than v$prev_version"
breaking=$(printf '%s\n' "$newer" | awk 'NF && $3 != "expand"')

echo "Schema: v$prev_version after the previous release, v$new_version after this revision."
if [ -n "$newer" ]; then
    printf '%s\n' "$newer" | awk '{ printf "    v%s %s: %s\n", $1, $2, $3 }'
fi

if [ "$new_version" -gt "$prev_version" ] && [ "$prev_has_compat" != t ]; then
    annotate notice "The previous release predates the compat guard (Phase 0a," \
        "docs/adr/0002-migration-compatibility-and-rollback.md): it refuses every schema newer than its own" \
        "v$prev_version whatever the markers say, so there is nothing to check against v$new_version." \
        "Rolling back past this revision needs bakicli migrate down $prev_version."
    exit 0
fi

if boot "3/3 previous release on this revision's schema (the rollback)" "$previous_bin" "$((port + 2))" "$log3"; then
    stop
    if [ -n "$breaking" ]; then
        show_logs
        annotate error "The previous release booted on a schema with steps recorded as breaking: its boot guard" \
            "(appliedStepsAfter/breakingSteps in internal/storage/database/postgres_migrations.go) did not refuse them."
        exit 1
    fi
    echo "PASS: the previous release (v$prev_version) boots on this revision's schema (v$new_version)."
    exit 0
fi

show_logs
if [ -n "$breaking" ]; then
    while read -r version step marker; do
        file=$(printf 'internal/storage/database/migrations/%04d_%s.up.sql' "$version" "$step")
        annotate error "v$version $step is marked \`-- baki:compat $marker\` ($file), so the previous release" \
            "refuses this schema and an image rollback after this deploy would crash-loop until" \
            "\`bakicli migrate down $prev_version\`. Ship it as expand/contract instead: an expand step plus code" \
            "that reads both shapes, then the contract step one release later. If the previous release really" \
            "does run on it, the marker should say expand (criteria in" \
            "internal/storage/database/migrations/README.md). See docs/adr/0002-migration-compatibility-and-rollback.md."
    done <<<"$breaking"
else
    annotate error "The previous release (v$prev_version) does not boot on this revision's schema" \
        "(v$new_version) although every newer step is marked \`-- baki:compat expand\`: a step marked expand is" \
        "not, or this revision changed the schema outside a migration step. See its log, the expand criteria in" \
        "internal/storage/database/migrations/README.md and docs/adr/0002-migration-compatibility-and-rollback.md."
fi
exit 1
