#!/usr/bin/env bash
# Remote IDE acceptance test (Phase 5: plan M4.3-M4.5, M2.8). Runs a real
# server as root with workspace isolation, a fake code-server (or the real
# one with CODE_SERVER=/path), the SSH endpoint and the OpenSSH client.
#
#   sudo test/e2e/ide.sh path/to/armageddon path/to/fake-code-server
#
# Build the fake with: go build -o fake-code-server ./test/e2e/fakecodeserver
#
# Steps:
#   1. server with the SSH endpoint enabled; admin; a workspace
#   2. IDE proxy: refused without a session, for a non-member, cross-origin;
#      served to the member, as the workspace user
#   3. SSH: ssh-config, exec, PTY, sftp, -L forward, non-loopback refused
#   4. Git credentials: git push over HTTP(S) from an SSH session with a
#      stored PAT; the PAT is never in the workspace
set -euo pipefail

HERE=$(dirname "$(readlink -f "$0")")
BIN=$(readlink -f "${1:-$(command -v armageddon)}")
CS=${CODE_SERVER:-${2:-}}
[ -n "$CS" ] || { echo "usage: ide.sh ARMAGEDDON FAKE_CODE_SERVER (or CODE_SERVER=...)"; exit 2; }
PORT=${PORT:-8105}
SSH_PORT=${SSH_PORT:-2205}
FWD_PORT=${FWD_PORT:-18105}
GIT_PORT=${GIT_PORT:-18106}
B=http://127.0.0.1:$PORT
ROOT=${E2E_ROOT:-/srv/armageddon-ide-$$}
DATA=$ROOT/server
LAPTOP=$ROOT/laptop
export ARMAGEDDON_CONFIG_DIR=$LAPTOP/config ARMAGEDDON_DATA_DIR=$LAPTOP/data
ADMIN_PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
PAT=e2e_pat_$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')

pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
PIDS=()
cleanup() { for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root (workspace isolation needs it)"
command -v ssh >/dev/null && command -v sftp >/dev/null || fail "needs the OpenSSH client (ssh, sftp)"
rm -rf "$ROOT"; mkdir -p "$ROOT" "$LAPTOP"; chmod 755 "$ROOT"
if [ -z "${CODE_SERVER:-}" ]; then install -m 0755 "$CS" "$ROOT/fake-code-server"; CS=$ROOT/fake-code-server; fi

api() { # api JAR METHOD PATH [JSON]
  curl -sf -b "$ROOT/$1.jar" -c "$ROOT/$1.jar" -X "$2" "$B$3" -H "X-CSRF-Token: $(cat "$ROOT/$1.csrf" 2>/dev/null)" \
    -H 'Content-Type: application/json' ${4:+-d "$4"}
}
code() { # code JAR METHOD PATH [curl args...]: HTTP status only
  local jar=$1 m=$2 p=$3; shift 3
  curl -s -o /dev/null -w '%{http_code}' -b "$ROOT/$jar.jar" -X "$m" "$B$p" "$@"
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

step "1. server, admin, workspace"
"$BIN" server init --data "$DATA" --listen "127.0.0.1:$PORT" --public-url "$B" \
  --ssh-listen "127.0.0.1:$SSH_PORT" --code-server "$CS" >/dev/null
"$BIN" server run --data "$DATA" >>"$ROOT/server.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -sf "$B/healthz" >/dev/null && break; sleep 0.2; done
TOKEN=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1 | cut -d= -f2)
curl -sf -c "$ROOT/admin.jar" -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"abdul\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/admin.csrf"
check "ssh endpoint listening on $SSH_PORT" grep -q "ssh endpoint listening on 127.0.0.1:$SSH_PORT" "$ROOT/server.log"
WS=$(api admin POST /api/workspaces '{"name":"ide"}' | json 'd["id"]')
for _ in $(seq 120); do [ "$(api admin GET "/api/workspaces/$WS" | json 'd["state"]')" = ready ] && break; sleep 0.5; done
TREE=$DATA/workspaces/$WS/tree
OWNER=$(stat -c %U "$TREE")
pass "workspace $WS ready (user $OWNER)"
# A second user who is not a member.
INVITE=$(api admin POST /api/invites | json 'd["url"]' | sed 's/.*token=//')
curl -sf -c "$ROOT/jane.jar" -X POST "$B/api/invites/accept" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$INVITE\",\"username\":\"jane\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/jane.csrf"

step "2. IDE proxy"
IDE=/api/workspaces/$WS/ide
WSH=(-H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==')
for p in / /echo /ws /static/app.js; do
  for m in GET POST; do
    [ "$(code nobody $m "$IDE$p" -H "Origin: $B")" = 401 ] || fail "anonymous $m $IDE$p not refused"
    [ "$(code jane $m "$IDE$p" -H "Origin: $B")" = 404 ] || fail "non-member $m $IDE$p not 404"
  done
  [ "$(code nobody GET "$IDE$p" "${WSH[@]}" -H "Origin: $B")" = 401 ] || fail "anonymous websocket $p not refused"
  [ "$(code jane GET "$IDE$p" "${WSH[@]}" -H "Origin: $B")" = 404 ] || fail "non-member websocket $p not 404"
done
pass "every probed IDE route refuses anonymous callers (401) and non-members (404)"
check "code-server was not started by refused requests" bash -c "! pgrep -u '$OWNER' -f code-server >/dev/null"
FIRST=$(code admin GET "$IDE/")
check "member opens the IDE (HTTP $FIRST)" bash -c "[ '$FIRST' = 200 ] || [ '$FIRST' = 302 ]"
check "code-server runs as the workspace user" bash -c "pgrep -u '$OWNER' -f code-server >/dev/null"
check "cross-origin WebSocket refused" test "$(code admin GET "$IDE/ws" "${WSH[@]}" -H 'Origin: https://evil.example')" = 403
check "POST without Origin refused" test "$(code admin POST "$IDE/echo" -d x)" = 403
if [ -z "${CODE_SERVER:-}" ]; then
  SESS=$(awk '$6 == "arm_session" {print $7}' "$ROOT/admin.jar")
  ECHO=$(curl -sf "$B$IDE/echo" -H "Cookie: arm_session=$SESS; other=1")
  check "Armageddon session cookie stripped before code-server" bash -c "! grep -q '$SESS' <<<'$ECHO' && grep -q other=1 <<<'$ECHO'"
  check "member WebSocket upgrades (101)" test "$(curl -s -o /dev/null -w '%{http_code}' -m 3 -b "$ROOT/admin.jar" "$B$IDE/ws" "${WSH[@]}" -H "Origin: $B" || true)" = 101
fi

step "3. SSH endpoint"
cd "$LAPTOP"
"$BIN" login "$B" --name e2e-laptop >"$ROOT/login.out" 2>&1 &
LOGIN_PID=$!
CODE=
for _ in $(seq 50); do CODE=$(grep -o 'code=[A-Z-]*' "$ROOT/login.out" 2>/dev/null | head -1 | cut -d= -f2 || true); [ -n "$CODE" ] && break; sleep 0.2; done
api admin POST /api/pair/approve "{\"code\":\"$CODE\"}" >/dev/null
wait $LOGIN_PID
"$BIN" ssh-config --file "$ROOT/ssh_config" >/dev/null
check "ssh-config wrote a Host block" grep -q "Host armageddon-ide" "$ROOT/ssh_config"
SSH=(ssh -F "$ROOT/ssh_config" -o BatchMode=yes -o ConnectTimeout=10)
OUT=$("${SSH[@]}" armageddon-ide 'id -un; pwd')
check "exec runs as $OWNER in the worktree" test "$OUT" = "$OWNER"$'\n'"$TREE"
check "PTY session gets a tty" bash -c "${SSH[*]} -tt armageddon-ide 'tty' 2>/dev/null | grep -q /dev/pts/"
echo "uploaded over sftp" >"$ROOT/up.txt"
printf 'put %s up.txt\nget up.txt %s\n' "$ROOT/up.txt" "$ROOT/down.txt" >"$ROOT/sftp.batch"
sftp -F "$ROOT/ssh_config" -o BatchMode=yes -b "$ROOT/sftp.batch" armageddon-ide >/dev/null
check "sftp put lands in the worktree" grep -q "uploaded over sftp" "$TREE/up.txt"
check "sftp files belong to the workspace user" test "$(stat -c %U "$TREE/up.txt")" = "$OWNER"
check "sftp get round-trips" cmp -s "$ROOT/up.txt" "$ROOT/down.txt"
check "sftp cannot read the server database" bash -c "! sftp -F '$ROOT/ssh_config' -o BatchMode=yes -b - armageddon-ide <<<'get $DATA/armageddon.db $ROOT/stolen.db' >/dev/null 2>&1"
"${SSH[@]}" armageddon-ide "mkdir -p www && echo served-from-workspace > www/index.html && exec python3 -m http.server $FWD_PORT --bind 127.0.0.1 --directory www" >/dev/null 2>&1 &
PIDS+=($!)
"${SSH[@]}" -N -L "127.0.0.1:$((FWD_PORT + 10)):127.0.0.1:$FWD_PORT" armageddon-ide &
PIDS+=($!)
GOT=
for _ in $(seq 50); do GOT=$(curl -sf "http://127.0.0.1:$((FWD_PORT + 10))/" || true); [ -n "$GOT" ] && break; sleep 0.2; done
check "-L forward reaches a dev server on the workspace's loopback" test "$GOT" = served-from-workspace
check "forwarding to a non-loopback address is refused" bash -c "! ${SSH[*]} -W 10.255.255.1:80 armageddon-ide </dev/null 2>/dev/null"

step "4. Git provider credentials"
mkdir -p "$ROOT/remotes" && git init -q --bare "$ROOT/remotes/r.git" && git -C "$ROOT/remotes/r.git" config http.receivepack true
python3 "$HERE/gitremote.py" "$PAT" "$ROOT/remotes" "$GIT_PORT" &
PIDS+=($!)
sleep 0.5
check "push without a stored credential is refused" bash -c "! ${SSH[*]} armageddon-ide 'GIT_TERMINAL_PROMPT=0 git push -q http://127.0.0.1:$GIT_PORT/r.git HEAD:refs/heads/main' 2>/dev/null"
RESP=$(api admin POST /api/credentials "{\"host\":\"127.0.0.1:$GIT_PORT\",\"kind\":\"https-token\",\"token\":\"$PAT\"}")
check "the API does not echo the token" bash -c "! grep -q '$PAT' <<<'$RESP'"
LIST=$(api admin GET /api/credentials)
check "the credential list does not contain the token" bash -c "grep -q '127.0.0.1:$GIT_PORT' <<<'$LIST' && ! grep -q '$PAT' <<<'$LIST'"
"${SSH[@]}" armageddon-ide "git config --global credential.helper store; GIT_TERMINAL_PROMPT=0 git push -q http://127.0.0.1:$GIT_PORT/r.git HEAD:refs/heads/main"
check "git push from the server seat authenticated with the stored PAT" bash -c "git -C '$ROOT/remotes/r.git' rev-parse --verify -q refs/heads/main >/dev/null"
check "grep -r finds no PAT in the workspace" bash -c "! grep -r -q '$PAT' '$DATA/workspaces/$WS'"
check "grep -r finds no PAT in the database files" bash -c "! grep -r -q -a '$PAT' $DATA/armageddon.db*"

printf '\n\033[32mALL IDE CHECKS PASSED\033[0m\n'
