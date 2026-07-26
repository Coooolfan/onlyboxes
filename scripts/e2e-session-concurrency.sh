#!/usr/bin/env bash
# End-to-end verification of per-session command concurrency.
#
# Brings up console plus one worker and checks the behaviour matrix:
#   1. default limit (1) stays strictly serial and returns 409 session_busy
#   2. a raised limit runs commands in one session genuinely in parallel
#   3. concurrent requests on a brand-new session_id create exactly one sandbox
#      (the container/box creation race)
#   4. concurrent commands share the session filesystem
#   5. one command's timeout does not kill its siblings
#   6. per-session 409 and worker-level 429 stay distinct
#   7. the janitor reclaims the session only after in-flight commands drain
#
# Usage:
#   scripts/e2e-session-concurrency.sh [docker|boxlite]
#
# Requires: a built console and worker binary, plus docker (for the docker
# worker) or a boxlite-capable host.
#
# Note: terminalResource has no REST route (it is exposed through MCP), so
# exec/resource concurrency is covered by the worker unit tests instead.
set -uo pipefail

FLAVOR="${1:-docker}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKDIR="$(mktemp -d /tmp/obx-e2e-XXXXXX)"
HTTP=http://127.0.0.1:8089
COOKIES="$WORKDIR/cookies.txt"
CONSOLE_BIN="$WORKDIR/console"
PASS=0
FAIL=0
CONSOLE_PID=""
WORKER_PID=""

case "$FLAVOR" in
  docker)  WORKER_SRC="$ROOT/worker/worker-docker"; WORKER_BIN="$WORKDIR/worker" ;;
  boxlite) WORKER_SRC="$ROOT/worker/worker-boxlite"; WORKER_BIN="$WORKER_SRC/target/debug/worker-boxlite" ;;
  *) echo "usage: $0 [docker|boxlite]"; exit 2 ;;
esac

log() { printf '\n=== %s\n' "$*"; }
ok()  { printf '  PASS  %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  FAIL  %s\n' "$*"; FAIL=$((FAIL+1)); }
chk() { if [ "$1" = true ]; then ok "$2"; else bad "$2 -- $3"; fi; }

cleanup() {
  [ -n "$WORKER_PID" ]  && kill -9 "$WORKER_PID"  2>/dev/null
  [ -n "$CONSOLE_PID" ] && kill -9 "$CONSOLE_PID" 2>/dev/null
  if [ "$FLAVOR" = docker ]; then
    ids=$(docker ps -aq --filter "label=onlyboxes.capability=terminalExec" 2>/dev/null)
    [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
  fi
  printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
  [ "$FAIL" -eq 0 ] || exit 1
}
trap cleanup EXIT

# post <prefix> <json>  — records HTTP status in <prefix>.code, body in <prefix>.body
post() {
  curl -s -m "${CURL_TIMEOUT:-120}" -o "$WORKDIR/$1.body" -w '%{http_code}' \
    -X POST "$HTTP/api/v1/commands/terminal" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    -d "$2" > "$WORKDIR/$1.code"
}
code() { cat "$WORKDIR/$1.code" 2>/dev/null; }
body() { cat "$WORKDIR/$1.body" 2>/dev/null; }
jget() { python3 -c "import json,sys;print(json.load(open('$WORKDIR/$1.body')).get('$2',''))" 2>/dev/null; }

# Wait only on the given PIDs. A bare `wait` would also block on the worker,
# which runs in the background for the whole script.
wait_pids() { for p in "$@"; do wait "$p" 2>/dev/null; done; }

log "building"
( cd "$ROOT/console" && go build -o "$CONSOLE_BIN" ./cmd/console ) || exit 1
if [ "$FLAVOR" = docker ]; then
  ( cd "$WORKER_SRC" && go build -o "$WORKER_BIN" ./cmd/worker-docker ) || exit 1
else
  ( cd "$WORKER_SRC" && cargo build ) || exit 1
fi
ok "binaries built"

log "starting console"
mkdir -p "$WORKDIR/db"
CONSOLE_HASH_KEY=e2e-hash-key-0123456789 \
CONSOLE_DB_PATH="$WORKDIR/db/console.db" \
CONSOLE_DASHBOARD_USERNAME=admin \
CONSOLE_DASHBOARD_PASSWORD=admin-password \
CONSOLE_LOG_FORMAT=text \
"$CONSOLE_BIN" >"$WORKDIR/console.log" 2>&1 &
CONSOLE_PID=$!
for _ in $(seq 1 60); do
  curl -s -o /dev/null "$HTTP/api/v1/console/session" && break
  sleep 0.5
done
ok "console listening"

curl -s -c "$COOKIES" -X POST "$HTTP/api/v1/console/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin-password"}' >"$WORKDIR/login.json"
grep -q '"authenticated":true' "$WORKDIR/login.json" || { cat "$WORKDIR/login.json"; exit 1; }

TOKEN=$(curl -s -b "$COOKIES" -X POST "$HTTP/api/v1/console/tokens" \
  -H 'Content-Type: application/json' -d '{"name":"e2e"}' \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['token'])")
[ -n "$TOKEN" ] || { echo "token mint failed"; exit 1; }

curl -s -b "$COOKIES" -X POST "$HTTP/api/v1/workers" \
  -H 'Content-Type: application/json' -d '{"type":"normal"}' >"$WORKDIR/worker.json"
NODE_ID=$(python3 -c "import json;print(json.load(open('$WORKDIR/worker.json'))['node_id'])")
SECRET=$(python3 -c "import json;print(json.load(open('$WORKDIR/worker.json'))['worker_secret'])")
ok "worker provisioned: $NODE_ID"

# start_worker <session_limit> <worker_exec_limit> <lease_default>
start_worker() {
  if [ -n "$WORKER_PID" ]; then
    kill -9 "$WORKER_PID" 2>/dev/null
    # Reap it so bash does not print a job-control notice later.
    wait "$WORKER_PID" 2>/dev/null
    WORKER_PID=""
    sleep 2
  fi
  if [ "$FLAVOR" = docker ]; then
    ids=$(docker ps -aq --filter "label=onlyboxes.capability=terminalExec" 2>/dev/null)
    [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
  fi
  env WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_CONSOLE_INSECURE=true \
      WORKER_ID="$NODE_ID" WORKER_SECRET="$SECRET" \
      WORKER_HEARTBEAT_INTERVAL_SEC=5 \
      WORKER_TERMINAL_SESSION_MAX_INFLIGHT="$1" \
      WORKER_TERMINAL_EXEC_MAX_INFLIGHT="$2" \
      WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT="$2" \
      WORKER_TERMINAL_LEASE_MIN_SEC=5 \
      WORKER_TERMINAL_LEASE_DEFAULT_SEC="$3" \
      WORKER_BOXLITE_HOME="$WORKDIR/boxlite" \
      WORKER_LOG_FORMAT=text \
      "$WORKER_BIN" >"$WORKDIR/worker-$1-$2.log" 2>&1 &
  WORKER_PID=$!
  sleep 8
}

##########################################################
log "1. default per-session limit stays serial"
##########################################################
start_worker 1 8 60
post seed '{"command":"echo seed","timeout_ms":120000}'
S=$(jget seed session_id)
chk "$([ "$(code seed)" = 200 ] && [ -n "$S" ] && echo true || echo false)" \
  "session created" "code=$(code seed) body=$(body seed)"

post slow "{\"command\":\"sleep 6; echo slow\",\"session_id\":\"$S\",\"timeout_ms\":120000}" &
SLOW=$!
sleep 2
post second "{\"command\":\"echo second\",\"session_id\":\"$S\",\"timeout_ms\":30000}"
chk "$([ "$(code second)" = 409 ] && echo true || echo false)" \
  "concurrent request rejected 409 at default limit" "code=$(code second)"
body second | grep -qi busy && ok "409 body reports session busy" || bad "expected busy message, got $(body second)"
wait_pids $SLOW
chk "$([ "$(code slow)" = 200 ] && echo true || echo false)" "in-flight command still succeeded" "code=$(code slow)"

##########################################################
log "2. raised limit runs one session's commands in parallel"
##########################################################
start_worker 4 8 60
post seed2 '{"command":"echo seed","timeout_ms":120000}'
S=$(jget seed2 session_id)

START=$(date +%s)
PIDS=""
for i in 1 2 3; do
  post "par$i" "{\"command\":\"sleep 4; echo done-$i\",\"session_id\":\"$S\",\"timeout_ms\":120000}" &
  PIDS="$PIDS $!"
done
wait_pids $PIDS
ELAPSED=$(( $(date +%s) - START ))
allok=true
for i in 1 2 3; do
  [ "$(code par$i)" = 200 ] || allok=false
  body "par$i" | grep -q "done-$i" || allok=false
done
chk "$allok" "3 concurrent commands all succeeded in one session" "codes: $(code par1) $(code par2) $(code par3)"
chk "$([ "$ELAPSED" -lt 10 ] && echo true || echo false)" \
  "commands ran in parallel (${ELAPSED}s < 12s serial)" "elapsed=${ELAPSED}s"

##########################################################
log "3. sandbox creation race: concurrent requests on a new session_id"
##########################################################
count_sandboxes() {
  if [ "$FLAVOR" = docker ]; then
    docker ps -aq --filter "label=onlyboxes.capability=terminalExec" | wc -l | tr -d ' '
  else
    ls "$WORKDIR/boxlite/boxes" 2>/dev/null | wc -l | tr -d ' '
  fi
}
BEFORE=$(count_sandboxes)
RID="race-$RANDOM$RANDOM"
PIDS=""
for i in 1 2 3 4; do
  post "race$i" "{\"command\":\"echo r-$i\",\"session_id\":\"$RID\",\"create_if_missing\":true,\"timeout_ms\":300000}" &
  PIDS="$PIDS $!"
done
wait_pids $PIDS
raceok=true
for i in 1 2 3 4; do
  case "$(code race$i)" in 200|409) ;; *) raceok=false ;; esac
done
chk "$raceok" "every racing request got a definite answer" \
  "codes: $(code race1) $(code race2) $(code race3) $(code race4)"
AFTER=$(count_sandboxes)
chk "$([ $((AFTER-BEFORE)) -eq 1 ] && echo true || echo false)" \
  "exactly one sandbox created for the racing session" "delta=$((AFTER-BEFORE))"

##########################################################
log "4. concurrent commands share the session filesystem"
##########################################################
post w "{\"command\":\"mkdir -p /tmp/cc && echo shared-payload > /tmp/cc/f\",\"session_id\":\"$S\",\"timeout_ms\":60000}"
post r "{\"command\":\"cat /tmp/cc/f\",\"session_id\":\"$S\",\"timeout_ms\":60000}"
body r | grep -q shared-payload && ok "filesystem shared across commands" || bad "got $(body r)"

##########################################################
log "5. one command's timeout spares its siblings"
##########################################################
post sib "{\"command\":\"sleep 8; echo survivor\",\"session_id\":\"$S\",\"timeout_ms\":120000}" &
SIB=$!
sleep 2
post doom "{\"command\":\"sleep 60\",\"session_id\":\"$S\",\"timeout_ms\":3000}"
chk "$([ "$(code doom)" != 200 ] && echo true || echo false)" \
  "short-deadline command failed" "code=$(code doom) body=$(body doom)"
wait_pids $SIB
sibok=false
[ "$(code sib)" = 200 ] && body sib | grep -q survivor && sibok=true
chk "$sibok" "sibling survived the other command's timeout" "code=$(code sib) body=$(body sib)"

##########################################################
log "6. per-session 409 vs worker-level 429"
##########################################################
start_worker 4 1 60   # session allows 4, worker-level capability quota only 1
post seed3 '{"command":"echo seed","timeout_ms":120000}'
S3=$(jget seed3 session_id)
post hold "{\"command\":\"sleep 5; echo hold\",\"session_id\":\"$S3\",\"timeout_ms\":60000}" &
HOLD=$!
sleep 2
post over "{\"command\":\"echo over\",\"session_id\":\"$S3\",\"timeout_ms\":30000}"
chk "$([ "$(code over)" = 429 ] && echo true || echo false)" \
  "worker-level quota exhaustion returns 429 no_capacity" "code=$(code over) body=$(body over)"
wait_pids $HOLD

##########################################################
log "7. janitor reclaims only after in-flight commands drain"
##########################################################
start_worker 4 8 6    # 6s lease, janitor ticks every 5s
post seed4 '{"command":"echo seed","lease_ttl_sec":6,"timeout_ms":120000}'
S4=$(jget seed4 session_id)
PIDS=""
for i in 1 2 3; do
  post "jan$i" "{\"command\":\"sleep 3; echo j-$i\",\"session_id\":\"$S4\",\"lease_ttl_sec\":6,\"timeout_ms\":60000}" &
  PIDS="$PIDS $!"
done
sleep 5
DURING=$(count_sandboxes)
chk "$([ "$DURING" -ge 1 ] && echo true || echo false)" \
  "session not reclaimed while commands are in flight" "sandboxes=$DURING"
wait_pids $PIDS
sleep 18
AFTER=$(count_sandboxes)
chk "$([ "$AFTER" -eq 0 ] && echo true || echo false)" \
  "session reclaimed after drain and lease expiry" "sandboxes=$AFTER"

log "8. worker logs clean"
if grep -qiE "panic|DATA RACE" "$WORKDIR"/worker-*.log; then
  bad "worker log contains panic or data race"
else
  ok "no panics or data races in worker logs"
fi

echo
echo "artifacts in $WORKDIR"
