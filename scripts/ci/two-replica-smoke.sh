#!/usr/bin/env bash
# Exercises the cross-replica mechanisms of a running two-replica stack over HTTP: the compose overlay
# (docker-compose.replicas.yml, replicas on :8081 and :8082) or any two replicas of one deployment. CI runs it in the
# two-replica job of .github/workflows/ci.yml; it runs the same way on a workstation:
#
#   docker compose -f docker-compose.yml -f docker-compose.replicas.yml up -d --build --wait
#   scripts/ci/two-replica-smoke.sh
#
# Usage: scripts/ci/two-replica-smoke.sh
#
#   SMOKE_A, SMOKE_B     the replicas' base URLs (default http://127.0.0.1:8081 and :8082)
#   SMOKE_LB             optional: the load balancer in front of both (default http://127.0.0.1:8080 when it answers);
#                        a request through it must work like one to a replica
#   SMOKE_EMAIL, SMOKE_PASSWORD
#                        the account the run signs in with, registered when it does not exist yet (the first account of
#                        a deployment is its admin, and the migration status scenario needs one)
#   SMOKE_REDIS_OUTAGE   1 (default) runs the last scenario, Redis stopped and started; 0 skips it
#   SMOKE_REDIS_STOP, SMOKE_REDIS_START
#                        the commands that stop and start Redis (default: docker compose ... stop|start redis against
#                        the overlay)
#   SMOKE_FANOUT_MS      how long a change on one replica may take to show on the other (default 3000; the cache it
#                        must beat lives 30 s)
#   SMOKE_TIMEOUT        seconds to wait for the replicas to be ready (default 180)
#
# Needs bash and curl. Each scenario prints PASS or FAIL lines; the exit status is 0 when none failed, 1 when one did,
# 2 on a usage or environment error.
#
# Scenarios (each is also proved in process by internal/di/two_replicas_test.go):
#   1. ready       both replicas answer /readyz without "degraded", under distinct instance ids
#   2. sign-in     a token from one replica is good on the other (one database, one signing key)
#   3. fan-out     a flow uploaded on A shows in B's home view at once, not after the view's 30 s cache
#   4. events      an event emitted on A reaches an event stream on B with an id, and a reconnect with Last-Event-ID
#                  gets what it missed (on the other replica)
#   5. conversation  two saves of one conversation at the same base version, one per replica: exactly one wins, the
#                  other is a 409 CONVERSATION_CONFLICT; repeated with the replicas swapped
#   6. undo        a source edit on A is undone through B's snapshot list
#   7. admin       the migration status answers the same on both replicas
#   8. outage      with Redis stopped both replicas stay ready and report it as degraded, edits still work, and once
#                  Redis is back they heal (the fan-out works again)
#
# Not covered here, because they need an AI provider the stack does not have: stopping a chat stream and deciding an
# approval from the other replica (internal/di/two_replicas_test.go, internal/service/chat_commands_test.go), the
# shared AI budget, and the knowledge-base lease.
set -euo pipefail

A=${SMOKE_A:-http://127.0.0.1:8081}
B=${SMOKE_B:-http://127.0.0.1:8082}
LB=${SMOKE_LB:-http://127.0.0.1:8080}
EMAIL=${SMOKE_EMAIL:-smoke-admin@example.com}
PASSWORD=${SMOKE_PASSWORD:-Sm0ke-Passw0rd-xyz!}
OUTAGE=${SMOKE_REDIS_OUTAGE:-1}
FANOUT_MS=${SMOKE_FANOUT_MS:-3000}
TIMEOUT=${SMOKE_TIMEOUT:-180}
COMPOSE=${SMOKE_COMPOSE:-docker compose -f docker-compose.yml -f docker-compose.replicas.yml}
REDIS_STOP=${SMOKE_REDIS_STOP:-$COMPOSE stop redis}
REDIS_START=${SMOKE_REDIS_START:-$COMPOSE start redis}

work=$(mktemp -d)
bg_pids=()
failures=0
cleanup() {
    local pid
    for pid in "${bg_pids[@]+"${bg_pids[@]}"}"; do kill "$pid" 2>/dev/null || true; done
    rm -rf "$work"
}
trap cleanup EXIT

in_actions() { [ "${GITHUB_ACTIONS:-}" = true ]; }

die() {
    if in_actions; then printf '::error::%s\n' "$*"; else printf 'ERROR: %s\n' "$*" >&2; fi
    exit 2
}

pass() { printf 'PASS  %s\n' "$*"; }

fail() {
    failures=$((failures + 1))
    if in_actions; then printf '::error::FAIL  %s\n' "$*"; else printf 'FAIL  %s\n' "$*"; fi
}

group() { if in_actions; then echo "::group::$1"; else echo "----- $1"; fi; }
endgroup() { if in_actions; then echo "::endgroup::"; fi; }

command -v curl >/dev/null 2>&1 || die "curl is required"

now_ms() { echo $(($(date +%s%N) / 1000000)); }

# req METHOD URL [BODY]: sets STATUS and BODY (the response, one line). The token, when set, is sent.
req() {
    local method=$1 url=$2 data=${3-}
    local args=(-s -o "$work/body" -w '%{http_code}' --max-time 30 -X "$method" "$url")
    [ -n "${TOKEN:-}" ] && args+=(-H "Authorization: Bearer $TOKEN")
    [ -n "$data" ] && args+=(-H 'Content-Type: application/json' -d "$data")
    STATUS=$(curl "${args[@]}" || echo 000)
    BODY=$(tr -d '\r\n' <"$work/body" 2>/dev/null || true)
}

# field NAME: the first string (escapes kept as sent) or number value of "NAME" in BODY.
JSON_STRING='"([^"\\]|\\.)*"'
field() {
    printf '%s' "$BODY" | grep -oE "\"$1\":($JSON_STRING|[0-9]+)" | head -n 1 | sed -E 's/^"[^"]*":"?//; s/"$//' || true
}

# until_ms LIMIT_MS DESCRIPTION COMMAND...: polls COMMAND every 200 ms; prints nothing, returns 0 with the elapsed
# milliseconds in ELAPSED when it succeeds within LIMIT_MS, 1 otherwise.
until_ms() {
    local limit=$1
    shift 2
    local start
    start=$(now_ms)
    while :; do
        if "$@" >/dev/null 2>&1; then
            ELAPSED=$(($(now_ms) - start))
            return 0
        fi
        if [ $(($(now_ms) - start)) -ge "$limit" ]; then
            ELAPSED=$(($(now_ms) - start))
            return 1
        fi
        sleep 0.2
    done
}

ready_body() { curl -s --max-time 5 -w ' %{http_code}' "$1/readyz" 2>/dev/null || true; }
is_ready() { case $(ready_body "$1") in *' 200') return 0 ;; esac; return 1; }
is_degraded() { case $(ready_body "$1") in *'"degraded"'*' 200') return 0 ;; esac; return 1; }
is_healthy() { is_ready "$1" && ! is_degraded "$1"; }

# A flow body: a one-block flow, named uniquely so the runs do not collide.
SRC='#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'"'''https://example.com/%s'''"' Method: HTTPClient.Method.GET\n#EndRegion\n'
# upload_flow BASE NAME: uploads a flow through BASE; sets FLOW_ID.
upload_flow() {
    local src=${SRC//%s/$2}
    req POST "$1/api/flow/upload" "{\"name\":\"$2\",\"files\":{\"Main.txt\":\"$src\"}}"
    [ "$STATUS" = 200 ] || return 1
    FLOW_ID=$(field id)
    [ -n "$FLOW_ID" ]
}

home_total() {
    req GET "$1/api/dashboard/home"
    [ "$STATUS" = 200 ] || return 1
    field totalFlows
}

stamp=$(date +%s)

# --- 1. ready -------------------------------------------------------------------------------------------------------
group "1. ready"
for url in "$A" "$B"; do
    start=$(date +%s)
    until is_healthy "$url"; do
        [ $(($(date +%s) - start)) -lt "$TIMEOUT" ] || die "$url is not ready after ${TIMEOUT}s: $(ready_body "$url")"
        sleep 2
    done
done
pass "both replicas ready, no degraded dependency"
if curl -s -o /dev/null --max-time 3 "$LB/readyz" 2>/dev/null; then HAVE_LB=1; else HAVE_LB=0; fi
endgroup

# --- 2. sign-in -----------------------------------------------------------------------------------------------------
group "2. sign-in"
TOKEN=""
req POST "$A/api/auth/register" "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"name\":\"Smoke\"}"
[ "$STATUS" = 200 ] || [ "$STATUS" = 201 ] || true # an existing account answers an error; the login below settles it
req POST "$A/api/auth/login" "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}"
[ "$STATUS" = 200 ] || die "sign-in on A answered $STATUS: $BODY"
TOKEN=$(field accessToken)
[ -n "$TOKEN" ] || die "no access token in the sign-in answer"
req GET "$B/api/auth/me"
if [ "$STATUS" = 200 ]; then pass "a token issued by A is accepted by B"; else fail "B refused A's token: $STATUS $BODY"; fi
if [ "$HAVE_LB" = 1 ]; then
    req GET "$LB/api/auth/me"
    if [ "$STATUS" = 200 ]; then pass "the load balancer forwards an authenticated request"; else fail "through the load balancer: $STATUS $BODY"; fi
fi
req GET "$A/api/admin/system/health"
admin_ok=0
if [ "$STATUS" = 200 ]; then
    id_a=$(field instanceId)
    req GET "$B/api/admin/system/health"
    id_b=$(field instanceId)
    if [ -n "$id_a" ] && [ -n "$id_b" ] && [ "$id_a" != "$id_b" ]; then
        pass "distinct instance ids ($id_a, $id_b)"
    else
        fail "the replicas share an instance id ('$id_a', '$id_b'): stream commands would be misrouted"
    fi
    admin_ok=1
else
    printf 'note: %s is not an admin (%s), the admin checks are skipped\n' "$EMAIL" "$STATUS"
fi
endgroup

# --- 3. fan-out -----------------------------------------------------------------------------------------------------
group "3. fan-out"
n0=$(home_total "$B") || die "home view on B: $STATUS $BODY"
home_total "$B" >/dev/null # B now holds the view in its cache
upload_flow "$A" "smoke-$stamp-home" || die "upload on A: $STATUS $BODY"
FIRST_FLOW=$FLOW_ID
home_is_next() { [ "$(home_total "$B")" = "$((n0 + 1))" ]; }
if until_ms "$FANOUT_MS" "home view" home_is_next; then
    pass "B's home view followed an upload on A in ${ELAPSED} ms"
else
    fail "B's home view still shows $n0 flows ${ELAPSED} ms after an upload on A (its cache lives 30 s): the invalidation did not cross"
fi
endgroup

# --- 4. events ------------------------------------------------------------------------------------------------------
group "4. events"
sse() { # sse BASE FILE [LAST_EVENT_ID]: an event stream into FILE, in the background (pid in SSE_PID)
    local args=(-sN --max-time 40 "$1/api/events" -H "Authorization: Bearer $TOKEN")
    [ -n "${3-}" ] && args+=(-H "Last-Event-ID: $3")
    curl "${args[@]}" >"$2" 2>/dev/null &
    SSE_PID=$!
    bg_pids+=("$SSE_PID")
}
has_flow_event() { grep -q "\"name\":\"flow:loaded\".*$2" "$1"; }

sse "$B" "$work/sse_b" # B holds a stream
sleep 2
upload_flow "$A" "smoke-$stamp-sse-1" || die "upload on A: $STATUS $BODY"
if until_ms 5000 "event on B" has_flow_event "$work/sse_b" "smoke-$stamp-sse-1"; then
    id1=$(grep -B1 "smoke-$stamp-sse-1" "$work/sse_b" | sed -n 's/^id: //p' | head -n 1 | tr -d '\r')
    if printf '%s' "$id1" | grep -Eq '^[0-9]+-[0-9]+$'; then
        pass "an event emitted on A reached a stream on B with the id $id1 (${ELAPSED} ms)"
    else
        fail "the event reached B without a usable id (got '$id1')"
        id1=""
    fi
else
    fail "no flow:loaded event for smoke-$stamp-sse-1 on B's stream after 5 s"
    id1=""
fi
kill "$SSE_PID" 2>/dev/null || true
if [ -n "$id1" ]; then
    upload_flow "$A" "smoke-$stamp-sse-2" || die "upload on A: $STATUS $BODY"
    sleep 1
    sse "$A" "$work/sse_replay" "$id1"
    if until_ms 5000 "replayed event" has_flow_event "$work/sse_replay" "smoke-$stamp-sse-2"; then
        pass "a reconnect with Last-Event-ID $id1 (to the other replica) replayed what it missed"
    else
        fail "the reconnect with Last-Event-ID $id1 did not replay smoke-$stamp-sse-2"
    fi
    if grep -q "smoke-$stamp-sse-1" "$work/sse_replay"; then
        fail "the replay repeated the event the client had already seen"
    fi
    kill "$SSE_PID" 2>/dev/null || true
fi
endgroup

# --- 5. conversation ------------------------------------------------------------------------------------------------
group "5. conversation"
conv_version() { # conv_version BASE KEY: the conversation's current version
    req POST "$1/api/chat/get" "{\"flowId\":\"$FIRST_FLOW\",\"provider\":\"$2\"}"
    field version
}
save_status() { # save_status BASE KEY BASE_VERSION OUT_FILE
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 -X POST "$1/api/chat/save" \
        -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
        -d "{\"flowId\":\"$FIRST_FLOW\",\"provider\":\"$2\",\"baseVersion\":$3,\"messages\":[{\"id\":\"m-$RANDOM\",\"role\":\"user\",\"content\":\"from $1\"}]}" || echo 000)
    echo "$code" >"$4"
}
conv_ok=1
for round in 1 2 3 4 5 6; do
    if [ $((round % 2)) = 1 ]; then X=$A; Y=$B; else X=$B; Y=$A; fi
    key="smoke-$stamp"
    v=$(conv_version "$X" "$key")
    [ -n "$v" ] || { fail "round $round: could not read the conversation version ($STATUS $BODY)"; conv_ok=0; break; }
    save_status "$X" "$key" "$v" "$work/save_x" &
    px=$!
    save_status "$Y" "$key" "$v" "$work/save_y" &
    py=$!
    wait "$px" "$py"
    codes=$(cat "$work/save_x" "$work/save_y" | sort | tr '\n' ' ')
    if [ "$codes" != "200 409 " ]; then
        fail "round $round: two saves at version $v answered '$codes', want one 200 and one 409"
        conv_ok=0
    fi
done
[ "$conv_ok" = 1 ] && pass "six rounds of two concurrent saves, one per replica: exactly one winner each"
endgroup

# --- 6. undo --------------------------------------------------------------------------------------------------------
group "6. undo"
req GET "$A/api/flow/source?flowId=$FIRST_FLOW"
original=$(field source)
src=${SRC//%s/smoke-$stamp-home}
req POST "$A/api/flow/save-source" "{\"flowId\":\"$FIRST_FLOW\",\"source\":\"${src}\\n\\n\"}"
if [ "$STATUS" != 200 ]; then
    fail "an edit on A answered $STATUS: $BODY"
else
    req GET "$B/api/flow/snapshots?flowId=$FIRST_FLOW"
    snap=$(field id)
    if [ "$STATUS" != 200 ] || [ -z "$snap" ]; then
        fail "B lists no undo snapshot of an edit made on A: $STATUS $BODY"
    else
        req POST "$B/api/flow/snapshots/restore" "{\"flowId\":\"$FIRST_FLOW\",\"snapshotId\":\"$snap\"}"
        if [ "$STATUS" != 200 ]; then
            fail "restoring the snapshot on B answered $STATUS: $BODY"
        else
            req GET "$A/api/flow/source?flowId=$FIRST_FLOW"
            if [ "$(field source)" = "$original" ]; then
                pass "an edit on A was undone through B's snapshot list"
            else
                fail "after the undo on B, A serves a different source than before the edit"
            fi
        fi
    fi
fi
endgroup

# --- 7. admin -------------------------------------------------------------------------------------------------------
group "7. admin"
if [ "$admin_ok" = 1 ]; then
    req GET "$A/api/admin/migration/status"
    sa=$BODY
    sta=$STATUS
    req GET "$B/api/admin/migration/status"
    if [ "$sta" = 200 ] && [ "$STATUS" = 200 ] && [ "$sa" = "$BODY" ]; then
        pass "the migration status is the same on both replicas ($BODY)"
    else
        fail "migration status differs: A $sta $sa, B $STATUS $BODY"
    fi
else
    printf 'skipped: the account is not an admin\n'
fi
endgroup

# --- 8. outage ------------------------------------------------------------------------------------------------------
group "8. outage"
if [ "$OUTAGE" = 0 ]; then
    printf 'skipped (SMOKE_REDIS_OUTAGE=0)\n'
else
    both_degraded() { is_degraded "$A" && is_degraded "$B"; }
    both_healthy() { is_healthy "$A" && is_healthy "$B"; }
    bash -c "$REDIS_STOP" >/dev/null 2>&1 || die "could not stop Redis with: $REDIS_STOP"
    if until_ms 45000 "degraded" both_degraded; then
        pass "Redis stopped: both replicas stay ready and report degraded (${ELAPSED} ms)"
    else
        fail "after stopping Redis the replicas answer: A $(ready_body "$A"), B $(ready_body "$B")"
    fi
    v=$(conv_version "$A" "smoke-$stamp-outage")
    save_status "$A" "smoke-$stamp-outage" "${v:-0}" "$work/save_out"
    if [ "$(cat "$work/save_out")" = 200 ]; then
        pass "a conversation save works with Redis down (the compare-and-swap is the database's)"
    else
        fail "a conversation save with Redis down answered $(cat "$work/save_out")"
    fi
    if upload_flow "$A" "smoke-$stamp-outage"; then
        pass "an upload works with Redis down"
    else
        fail "an upload with Redis down answered $STATUS: $BODY"
    fi

    bash -c "$REDIS_START" >/dev/null 2>&1 || die "could not start Redis with: $REDIS_START"
    if until_ms 90000 "healed" both_healthy; then
        pass "Redis back: both replicas report healthy again (${ELAPSED} ms)"
    else
        fail "90 s after starting Redis the replicas answer: A $(ready_body "$A"), B $(ready_body "$B")"
    fi
    # The resubscribe resyncs the caches (spaced 5 s apart); the next fan-out must work again.
    upload_flow "$A" "smoke-$stamp-healed" || die "upload on A: $STATUS $BODY"
    n1=$(home_total "$B")
    upload_flow "$A" "smoke-$stamp-healed-2" || die "upload on A: $STATUS $BODY"
    home_is_next() { [ "$(home_total "$B")" = "$((n1 + 1))" ]; }
    if until_ms 30000 "fan-out healed" home_is_next; then
        pass "the fan-out works again after the outage (${ELAPSED} ms)"
    else
        fail "B's home view did not follow A after Redis came back"
    fi
fi
endgroup

if [ "$failures" -gt 0 ]; then
    if in_actions; then printf '::error::%d scenario check(s) failed\n' "$failures"; else printf '%d check(s) failed\n' "$failures" >&2; fi
    exit 1
fi
echo "two-replica smoke passed"
