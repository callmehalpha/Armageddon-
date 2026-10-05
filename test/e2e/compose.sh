#!/usr/bin/env bash
# Docker Compose deployment test (plan M5.7):
#   1. `docker compose up` (deploy/compose.yaml) becomes healthy
#   2. first admin and a workspace through the published port
#   3. the per-workspace user exists inside the container; the data
#      directory is the volume; no Docker socket is visible
#   4. data survives recreating the container
#   5. test/e2e/mvp.sh passes inside the container image
#
#   test/e2e/compose.sh                          # builds and starts deploy/compose.yaml
#   CONTAINER=name URL=http://127.0.0.1:8114 test/e2e/compose.sh   # an already running container (no step 4)
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
URL=${URL:-http://127.0.0.1:8080}
COMPOSE=(docker compose -f "$ROOT/deploy/compose.yaml")
W=$(mktemp -d)
pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; docker logs "$CTR" 2>&1 | tail -20; exit 1; }
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

if [ -z "${CONTAINER:-}" ]; then
  "${COMPOSE[@]}" up -d --build
  trap '"${COMPOSE[@]}" down -v >/dev/null 2>&1 || true; rm -rf "$W"' EXIT
  CTR=$("${COMPOSE[@]}" ps -q armageddon)
else
  CTR=$CONTAINER
  trap 'rm -rf "$W"' EXIT
fi
dx() { docker exec -i "$CTR" "$@"; }

wait_healthy() {
  for _ in $(seq 60); do dx armageddon server health --data /var/lib/armageddon --timeout 2s >/dev/null 2>&1 && return; sleep 1; done
  fail "container not healthy"
}
wait_healthy
check "server answers on the published port" curl -sf "$URL/healthz"
TOKEN=$(docker logs "$CTR" 2>&1 | grep -o 'setup?token=[a-z0-9]*' | tail -1 | cut -d= -f2)
check "setup URL printed in the container log" test -n "$TOKEN"
PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
curl -sf -c "$W/jar" -X POST "$URL/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"admin\",\"password\":\"$PW\"}" | json 'd["csrf"]' >"$W/csrf"
api() { curl -sf -b "$W/jar" -X "$1" "$URL$2" -H "X-CSRF-Token: $(cat "$W/csrf")" -H 'Content-Type: application/json' ${3:+-d "$3"}; }
WS=$(api POST /api/workspaces '{"name":"compose"}' | json 'd["id"]')
for _ in $(seq 60); do [ "$(api GET "/api/workspaces/$WS" | json 'd["state"]')" = ready ] && break; sleep 1; done
check "workspace ready" test "$(api GET "/api/workspaces/$WS" | json 'd["state"]')" = ready
OWNER=$(dx stat -c %U "/var/lib/armageddon/workspaces/$WS/tree")
check "per-workspace user $OWNER exists in the container" dx getent passwd "$OWNER"
check "workspace user cannot read the database" bash -c "! docker exec '$CTR' runuser -u '$OWNER' -- cat /var/lib/armageddon/armageddon.db >/dev/null 2>&1"
check "no Docker socket in the container" bash -c "! docker exec '$CTR' test -e /var/run/docker.sock"
check "doctor runs in the container" bash -c "docker exec '$CTR' armageddon doctor >'$W/doctor' 2>&1 || grep -qv FAIL '$W/doctor'"

if [ -z "${CONTAINER:-}" ]; then
  SEQ=$(api GET "/api/workspaces/$WS" | json 'd["checkpoint_seq"]')
  "${COMPOSE[@]}" up -d --force-recreate
  CTR=$("${COMPOSE[@]}" ps -q armageddon)
  wait_healthy
  check "data survives recreating the container" test "$(api GET "/api/workspaces/$WS" | json 'd["checkpoint_seq"]')" -ge "$SEQ"
  # Workspace users live in the container's /etc/passwd and are recreated on
  # start (sysuser.Ensure); file ownership on the volume must still match.
  OWNER2=$(dx stat -c %U "/var/lib/armageddon/workspaces/$WS/tree")
  check "workspace files still owned by the workspace user after recreation ($OWNER2)" test "$OWNER2" = "$OWNER"
fi

# mvp.sh inside the image: same binary, same git, users created in the container.
docker cp "$ROOT/test/e2e/mvp.sh" "$CTR:/tmp/mvp.sh"
if ! dx sh -c 'command -v python3' >/dev/null; then
  dx sh -c 'apt-get update -qq && apt-get install -y -qq python3 >/dev/null' || fail "cannot install python3 for mvp.sh"
fi
dx sh -c 'mkdir -p /srv && chmod 755 /srv'
if dx env PORT="${MVP_PORT:-8099}" bash /tmp/mvp.sh /usr/local/bin/armageddon >"$W/mvp.log" 2>&1; then
  pass "mvp.sh passes inside the container"
else
  tail -30 "$W/mvp.log"; fail "mvp.sh inside the container"
fi
printf '\n\033[32mALL COMPOSE CHECKS PASSED\033[0m\n'
