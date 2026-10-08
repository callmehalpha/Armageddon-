#!/usr/bin/env bash
# Runtimes and Docker acceptance test (plan M8), with the privilege split:
# `armageddon helper` runs as root, `armageddon server run` as the
# unprivileged `armageddon` user (created if missing).
#
#   sudo test/e2e/runtime.sh path/to/armageddon
#
# Needs network access (nodejs.org, the npm registry, Docker Hub). PHP and
# Docker parts are skipped when php or a running Docker daemon is missing.
#
# Steps:
#   1. helper (root) and server (armageddon); admin; a second user; a
#      workspace with a sample Next.js app
#   2. Node (M8.2): plan, install of the pinned Node release and the
#      dependencies as the workspace user, `next dev` started through the
#      API, healthy, running as the workspace user, node on PATH in a login
#      shell
#   3. ports (M8.6): port 3000 listed; reachable through the authenticated
#      proxy only (401 anonymous, 404 non-member)
#   4. PHP (M8.3): a plain PHP workspace served by `php -S`; a sample
#      Laravel app installed (composer, .env, key, SQLite) and served by
#      `php artisan serve`
#   5. Compose (M8.4): privileged, host-mount and Docker-socket files
#      refused with a clear error; Postgres and Redis up, bound to
#      127.0.0.1; the workspace user has no Docker access; down
set -Eeuo pipefail
trap 'printf "\033[31mFAIL\033[0m command failed (line %s): %s\n" "$LINENO" "$BASH_COMMAND"' ERR

BIN=$(readlink -f "${1:-$(command -v armageddon)}")
PORT=${PORT:-8107}
B=http://127.0.0.1:$PORT
ROOT=${E2E_ROOT:-/srv/armageddon-runtime-$$}
DATA=$ROOT/server
RUN=$ROOT/run
SOCK=$RUN/helper.sock
SERVER_USER=${SERVER_USER:-armageddon}
NODE_VERSION=${NODE_VERSION:-22.12.0}
ADMIN_PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')

pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
skip() { printf '\033[33mSKIP\033[0m %s\n' "$*"; }
fail() {
  printf '\033[31mFAIL\033[0m %s\n' "$*"
  echo "--- helper log (tail)"; tail -20 "$ROOT/helper.log" 2>/dev/null || true
  echo "--- server log (tail)"; tail -30 "$ROOT/server.log" 2>/dev/null || true
  exit 1
}
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
SERVER_PID='' HELPER_PID=''
cleanup() {
  if [ -n "${WS:-}" ] && [ -n "$SERVER_PID" ]; then api admin POST "/api/workspaces/$WS/compose/down" >/dev/null 2>&1 || true; fi
  for p in $SERVER_PID $HELPER_PID; do kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root (the helper needs it; the server drops to $SERVER_USER)"
id "$SERVER_USER" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SERVER_USER" 2>/dev/null || id "$SERVER_USER" >/dev/null
rm -rf "$ROOT"; mkdir -p "$ROOT" "$DATA"; chmod 755 "$ROOT"
chown "$SERVER_USER:" "$DATA"

start_helper() {
  "$BIN" helper --data "$DATA" --socket "$SOCK" --server-user "$SERVER_USER" >>"$ROOT/helper.log" 2>&1 &
  HELPER_PID=$!
  for _ in $(seq 50); do [ -S "$SOCK" ] && return; sleep 0.1; done
  fail "helper did not start"
}
start_server() {
  setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$DATA" \
    "$BIN" server run --data "$DATA" --helper-socket "$SOCK" >>"$ROOT/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 50); do curl -sf "$B/healthz" >/dev/null && return; sleep 0.2; done
  fail "server did not start"
}
api() { # api JAR METHOD PATH [JSON]
  curl -sf -b "$ROOT/$1.jar" -c "$ROOT/$1.jar" -X "$2" "$B$3" -H "X-CSRF-Token: $(cat "$ROOT/$1.csrf" 2>/dev/null)" \
    -H 'Content-Type: application/json' ${4:+-d "$4"}
}
call() { # call JAR METHOD PATH JSON: body and status (last line), whatever the status
  curl -s -w '\n%{http_code}' -b "$ROOT/$1.jar" -X "$2" "$B$3" -H "X-CSRF-Token: $(cat "$ROOT/$1.csrf" 2>/dev/null)" \
    -H 'Content-Type: application/json' -d "$4"
}
code() { # code JAR METHOD PATH: HTTP status only
  curl -s -o /dev/null -w '%{http_code}' -b "$ROOT/$1.jar" -X "$2" "$B$3"
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
wait_ready() { # wait_ready ID
  for _ in $(seq 120); do
    st=$(api admin GET "/api/workspaces/$1" | json 'd["state"]')
    [ "$st" = ready ] && return; [ "$st" = failed ] && fail "workspace $1 failed"
    sleep 0.5
  done; fail "workspace $1 not ready"
}
put() { # put WS_TREE OWNER RELPATH < content: write a file as the workspace user
  runuser -u "$2" -- mkdir -p "$(dirname "$1/$3")"
  runuser -u "$2" -- tee "$1/$3" >/dev/null
}
# proc WS NAME FIELD: a field of a managed process
proc() {
  api admin GET "/api/workspaces/$1/runtime" | json "next((str(p.get('$3')) for p in d['processes'] if p['name']=='$2'), '')"
}
wait_proc() { # wait_proc WS NAME FIELD VALUE SECONDS
  for _ in $(seq "$5"); do
    [ "$(proc "$1" "$2" "$3")" = "$4" ] && return 0
    [ "$(proc "$1" "$2" state)" = exited ] && [ "$4" != exited ] && break
    sleep 1
  done
  echo "--- $2 output"; api admin GET "/api/workspaces/$1/runtime/logs?name=$2" | json 'd["data"]' | tail -40
  fail "$2: $3 never became $4"
}

step "1. helper, server, admin, workspace"
as_server() { setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$DATA" "$@"; }
as_server "$BIN" server init --data "$DATA" --listen "127.0.0.1:$PORT" --public-url "$B" >/dev/null
start_helper
start_server
TOKEN=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1 | cut -d= -f2)
curl -sf -c "$ROOT/admin.jar" -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"abdul\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/admin.csrf"
INVITE=$(api admin POST /api/invites | json 'd["url"]' | sed 's/.*token=//')
curl -sf -c "$ROOT/jane.jar" -X POST "$B/api/invites/accept" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$INVITE\",\"username\":\"jane\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/jane.csrf"
WS=$(api admin POST /api/workspaces '{"name":"next-app"}' | json 'd["id"]')
wait_ready "$WS"
TREE=$DATA/workspaces/$WS/tree
WHOME=$DATA/workspaces/$WS/home
OWNER=$(stat -c %U "$TREE")
pass "workspace $WS ready (user $OWNER)"
put "$TREE" "$OWNER" package.json <<'EOF'
{"name": "sample-next", "private": true, "scripts": {"dev": "next dev"},
 "dependencies": {"next": "15.1.6", "react": "19.0.0", "react-dom": "19.0.0"}}
EOF
echo "$NODE_VERSION" | put "$TREE" "$OWNER" .nvmrc
put "$TREE" "$OWNER" app/layout.js <<'EOF'
export default function RootLayout({ children }) { return <html><body>{children}</body></html>; }
EOF
put "$TREE" "$OWNER" app/page.js <<'EOF'
export default function Page() { return <h1>Hello from the Armageddon seat</h1>; }
EOF

step "2. Node: plan, install, start (M8.2)"
PLAN=$(api admin GET "/api/workspaces/$WS/runtime")
check "detected Next.js with npm" test "$(echo "$PLAN" | json 'd["plan"]["provider"]+"/"+d["plan"]["framework"]+"/"+d["plan"]["package_manager"]')" = node/next/npm
check "Node $NODE_VERSION from .nvmrc, to install" test "$(echo "$PLAN" | json 'd["plan"]["toolchain"]["version"]+"/"+d["plan"]["toolchain"]["source"]')" = "$NODE_VERSION/install"
api admin POST "/api/workspaces/$WS/runtime/install" >/dev/null
wait_proc "$WS" install state exited 600
check "install exited 0" test "$(proc "$WS" install exit_code)" = 0
api admin GET "/api/workspaces/$WS/runtime/logs?name=install" | json 'd["data"]' >"$ROOT/install.log"
check "install log shows the checked download" grep -q "Installed Node.js $NODE_VERSION" "$ROOT/install.log"
check "Node installed in the workspace, owned by $OWNER" test "$(stat -c %U "$WHOME/.armageddon/runtimes/node/$NODE_VERSION/bin/node")" = "$OWNER"
check "node_modules owned by $OWNER" test "$(stat -c %U "$TREE/node_modules")" = "$OWNER"
LOGIN_NODE=$(runuser -u "$OWNER" -- env -i HOME="$WHOME" PATH=/usr/bin:/bin bash -lc 'node --version')
check "a login shell (the terminal) finds node v$NODE_VERSION" test "$LOGIN_NODE" = "v$NODE_VERSION"
api admin POST "/api/workspaces/$WS/runtime/start" '{}' >/dev/null
wait_proc "$WS" dev health healthy 180
pass "next dev answers on port 3000"
PID=$(ss -ltnpH 'sport = :3000' | grep -o 'pid=[0-9]*' | head -1 | cut -d= -f2)
check "the dev server runs as $OWNER" test "$(ps -o user= -p "$PID" | tr -d ' ')" = "$OWNER"
STATUS=$(call admin POST "/api/workspaces/$WS/runtime/start" '{}' | tail -1)
check "a second start is refused (409)" test "$STATUS" = 409

step "3. ports and the authenticated proxy (M8.6)"
check "port 3000 listed" test "$(api admin GET "/api/workspaces/$WS/ports" | json '",".join(str(p["port"])+":"+p["source"] for p in d)' | tr ',' '\n' | grep -c '^3000:process$')" = 1
P=/api/workspaces/$WS/ports/3000/
BODY=$(curl -sf -b "$ROOT/admin.jar" "$B$P")
check "the member gets the app through the proxy" grep -q "Hello from the Armageddon seat" <<<"$BODY"
check "anonymous callers are refused (401)" test "$(code nobody GET "$P")" = 401
check "non-members are refused (404)" test "$(code jane GET "$P")" = 404
check "the server's own port is not proxied" test "$(code admin GET "/api/workspaces/$WS/ports/$PORT/")" = 404
api admin POST "/api/workspaces/$WS/runtime/stop" '{"name":"dev"}' >/dev/null
check "stopped by the user" test "$(proc "$WS" dev stopped_by)" = user

step "4. PHP (M8.3)"
if command -v php >/dev/null && [ -x /usr/bin/php ]; then
  WP=$(api admin POST /api/workspaces '{"name":"php-app"}' | json 'd["id"]')
  wait_ready "$WP"
  PTREE=$DATA/workspaces/$WP/tree
  POWNER=$(stat -c %U "$PTREE")
  put "$PTREE" "$POWNER" public/index.php <<<'<?php echo "php ", PHP_VERSION;'
  check "detected PHP" test "$(api admin GET "/api/workspaces/$WP/runtime" | json 'd["plan"]["provider"]')" = php
  api admin POST "/api/workspaces/$WP/runtime/start" '{}' >/dev/null
  wait_proc "$WP" dev health healthy 30
  check "php -S serves through the proxy" grep -q "^php " <<<"$(curl -sf -b "$ROOT/admin.jar" "$B/api/workspaces/$WP/ports/8000/")"
  api admin POST "/api/workspaces/$WP/runtime/stop" '{}' >/dev/null
  if command -v composer >/dev/null; then
    # A sample Laravel app from a fresh skeleton, as a clone would bring
    # it: no vendor/, no .env, no database. Unpinned: Composer picks the
    # newest skeleton the runner's PHP supports, and refuses framework
    # releases with security advisories, which old majors accumulate.
    WL=$(api admin POST /api/workspaces '{"name":"laravel-app"}' | json 'd["id"]')
    wait_ready "$WL"
    LTREE=$DATA/workspaces/$WL/tree LHOME=$DATA/workspaces/$WL/home
    LOWNER=$(stat -c %U "$LTREE")
    runuser -u "$LOWNER" -- env -C "$LHOME" HOME="$LHOME" composer create-project --no-install --no-scripts --no-interaction -q \
      laravel/laravel "$LHOME/skeleton" || fail "composer create-project"
    runuser -u "$LOWNER" -- cp -a "$LHOME/skeleton/." "$LTREE/"
    check "detected Laravel" test "$(api admin GET "/api/workspaces/$WL/runtime" | json 'd["plan"]["provider"]+"/"+d["plan"]["framework"]')" = php/laravel
    api admin POST "/api/workspaces/$WL/runtime/install" >/dev/null
    wait_proc "$WL" install state exited 900
    if [ "$(proc "$WL" install exit_code)" != 0 ]; then
      echo "--- install output"; api admin GET "/api/workspaces/$WL/runtime/logs?name=install" | json 'd["data"]' | tail -60
    fi
    check "composer install, .env, key and SQLite migration (exit 0)" test "$(proc "$WL" install exit_code)" = 0
    check "APP_KEY set" grep -q '^APP_KEY=base64:' "$LTREE/.env"
    api admin POST "/api/workspaces/$WL/runtime/start" '{}' >/dev/null
    wait_proc "$WL" dev health healthy 60
    check "php artisan serve answers through the proxy" grep -q "Laravel" <<<"$(curl -sf -b "$ROOT/admin.jar" "$B/api/workspaces/$WL/ports/8000/")"
    api admin POST "/api/workspaces/$WL/runtime/stop" '{}' >/dev/null
  else
    skip "Laravel: composer not installed"
  fi
else
  skip "PHP: php not installed in /usr/bin"
fi

step "5. Compose (M8.4)"
if ! docker info >/dev/null 2>&1; then
  skip "Compose: no running Docker daemon"
else
  put "$TREE" "$OWNER" bad-privileged.yaml <<'EOF'
services:
  x: {image: alpine, privileged: true}
EOF
  put "$TREE" "$OWNER" bad-hostmount.yaml <<'EOF'
services:
  x: {image: alpine, volumes: ["/:/host"]}
EOF
  put "$TREE" "$OWNER" bad-socket.yaml <<'EOF'
services:
  x: {image: alpine, volumes: ["/var/run/docker.sock:/var/run/docker.sock"]}
EOF
  for f in privileged:privileged hostmount:"bind mounts" socket:"Docker socket"; do
    file=bad-${f%%:*}.yaml want=${f#*:}
    OUT=$(call admin POST "/api/workspaces/$WS/compose/up" "{\"file\":\"$file\"}")
    check "$file refused (400) naming the problem" test "$(tail -1 <<<"$OUT")" = 400
    grep -q "$want" <<<"$OUT" || fail "$file: error does not mention '$want': $OUT"
  done
  check "nothing was started by the refused files" test "$(docker ps -q --filter "label=com.docker.compose.project=arm-${WS,,}" | wc -l)" = 0
  put "$TREE" "$OWNER" compose.yaml <<'EOF'
services:
  db:
    image: postgres:16-alpine
    environment: {POSTGRES_PASSWORD: dev}
    ports: ["5432:5432"]
    volumes: [pgdata:/var/lib/postgresql/data]
  cache:
    image: redis:7-alpine
    ports: ["0.0.0.0:6379:6379"]
volumes:
  pgdata:
EOF
  UP=$(api admin POST "/api/workspaces/$WS/compose/up" '{}')
  check "compose up: db and cache running" test "$(echo "$UP" | json '",".join(s["service"]+"="+s["state"] for s in d["services"])')" = "cache=running,db=running"
  check "the 0.0.0.0 port was rebound to 127.0.0.1 (with a note)" grep -q "127.0.0.1" <<<"$(echo "$UP" | json 'd.get("warnings")')"
  check "published ports bind 127.0.0.1 only" test "$(docker port "arm-${WS,,}-db-1" 5432/tcp)" = "127.0.0.1:5432"
  check "Postgres answers" bash -c "for i in \$(seq 30); do docker exec arm-${WS,,}-db-1 pg_isready -q && exit 0; sleep 1; done; exit 1"
  check "Redis answers" test "$(docker exec "arm-${WS,,}-cache-1" redis-cli ping)" = PONG
  check "port 5432 listed as a Compose port" test "$(api admin GET "/api/workspaces/$WS/ports" | json '[p["source"]+" "+p.get("service","") for p in d if p["port"]==5432][0]')" = "compose db"
  check "the workspace user cannot use Docker" bash -c "! runuser -u '$OWNER' -- docker ps >/dev/null 2>&1"
  api admin POST "/api/workspaces/$WS/compose/down" >/dev/null
  check "compose down" test "$(api admin GET "/api/workspaces/$WS/compose" | json 'len(d["services"])')" = 0
  check "volumes are kept" docker volume inspect "arm-${WS,,}_pgdata" >/dev/null
  docker volume rm "arm-${WS,,}_pgdata" >/dev/null
fi

printf '\n\033[32mALL RUNTIME CHECKS PASSED\033[0m\n'
