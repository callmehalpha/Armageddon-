#!/usr/bin/env bash
# Remote IDE acceptance test (Phase 5: plan M4.3-M4.5, M2.8), with the
# privilege split: `armageddon helper` runs as root, `armageddon server run`
# as the unprivileged `armageddon` user (created if missing). A fake
# code-server (or the real one with CODE_SERVER=/path), the SSH endpoint and
# the OpenSSH client.
#
#   sudo test/e2e/ide.sh path/to/armageddon path/to/fake-code-server
#
# Build the fake with: go build -o fake-code-server ./test/e2e/fakecodeserver
#
# Steps:
#   1. helper (root) and server (armageddon) with the SSH endpoint; admin;
#      a workspace
#   2. IDE proxy: refused without a session, for a non-member, cross-origin;
#      served to the member, as the workspace user, through the helper
#   3. SSH: ssh-config, exec, PTY, sftp, -L forward, non-loopback refused
#   4. Git credentials: git push over HTTP from an SSH session with a
#      stored PAT; the PAT is never in the workspace
#   5. escapes from a second workspace: its user cannot use workspace A's
#      credential or agent socket, cannot reach A's code-server socket, and
#      a socket planted at A's code-server path is refused by the server
set -Eeuo pipefail
trap 'printf "\033[31mFAIL\033[0m command failed (line %s): %s\n" "$LINENO" "$BASH_COMMAND"' ERR

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
RUN=$ROOT/run
SOCK=$RUN/helper.sock
SERVER_USER=${SERVER_USER:-armageddon}
export ARMAGEDDON_CONFIG_DIR=$LAPTOP/config ARMAGEDDON_DATA_DIR=$LAPTOP/data
ADMIN_PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
PAT=e2e_pat_$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')

pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
fail() {
  printf '\033[31mFAIL\033[0m %s\n' "$*"
  echo "--- helper log (tail)"; tail -20 "$ROOT/helper.log" 2>/dev/null || true
  echo "--- server log (tail)"; tail -30 "$ROOT/server.log" 2>/dev/null || true
  exit 1
}
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
PIDS=()
SERVER_PID='' HELPER_PID=''
cleanup() {
  for p in "${PIDS[@]}" $SERVER_PID $HELPER_PID; do kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root (the helper needs it; the server drops to $SERVER_USER)"
for t in ssh sftp ssh-add; do command -v "$t" >/dev/null || fail "needs the OpenSSH client (ssh, sftp, ssh-add)"; done
id "$SERVER_USER" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SERVER_USER" 2>/dev/null || id "$SERVER_USER" >/dev/null
rm -rf "$ROOT"; mkdir -p "$ROOT" "$LAPTOP" "$DATA" "$ROOT/bin"; chmod 755 "$ROOT" "$ROOT/bin"
chown "$SERVER_USER:" "$DATA"
# Everything run by a non-root user comes from the test root, not the
# checkout (which other users may not be able to traverse). The helper only
# runs a code-server that no workspace (or anyone but root and the server
# user) can replace, so it must live in a root-owned directory.
if [ -z "${CODE_SERVER:-}" ]; then install -m 0755 "$CS" "$ROOT/bin/fake-code-server"; CS=$ROOT/bin/fake-code-server; fi
install -m 0644 "$HERE/gitremote.py" "$ROOT/bin/gitremote.py"

as_server() { setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$DATA" "$@"; }
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
code() { # code JAR METHOD PATH [curl args...]: HTTP status only
  local jar=$1 m=$2 p=$3; shift 3
  curl -s -o /dev/null -w '%{http_code}' -b "$ROOT/$jar.jar" -X "$m" "$B$p" "$@"
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
wait_ready() { # wait_ready ID
  for _ in $(seq 120); do
    st=$(api admin GET "/api/workspaces/$1" | json 'd["state"]')
    [ "$st" = ready ] && return; [ "$st" = failed ] && fail "workspace $1 failed"
    sleep 0.5
  done; fail "workspace $1 not ready"
}
as_user() { # as_user USER CMD...: run as a workspace user
  local u=$1; shift
  runuser -u "$u" -- env HOME=/tmp "$@"
}

step "1. helper, server, admin, workspace"
as_server "$BIN" server init --data "$DATA" --listen "127.0.0.1:$PORT" --public-url "$B" \
  --ssh-listen "127.0.0.1:$SSH_PORT" --code-server "$CS" >/dev/null
start_helper
start_server
check "helper runs as root" test "$(ps -o uid= -p "$HELPER_PID" | tr -d ' ')" = 0
check "server runs as $SERVER_USER" test "$(ps -o uid= -p "$SERVER_PID" | tr -d ' ')" = "$(id -u "$SERVER_USER")"
TOKEN=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1 | cut -d= -f2)
curl -sf -c "$ROOT/admin.jar" -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"abdul\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/admin.csrf"
check "ssh endpoint listening on $SSH_PORT" grep -q "ssh endpoint listening on 127.0.0.1:$SSH_PORT" "$ROOT/server.log"
WS=$(api admin POST /api/workspaces '{"name":"ide"}' | json 'd["id"]')
wait_ready "$WS"
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
RUNDIR=$DATA/workspaces/$WS/run
check "run/ is $OWNER:$SERVER_USER, mode 2750" test "$(stat -c '%a %U %G' "$RUNDIR")" = "2750 $OWNER $SERVER_USER"
check "the code-server socket is 660 with the server's group" test "$(stat -c '%a %G' "$RUNDIR/ide.sock")" = "660 $SERVER_USER"
check "cross-origin WebSocket refused" test "$(code admin GET "$IDE/ws" "${WSH[@]}" -H 'Origin: https://evil.example')" = 403
check "POST without Origin refused" test "$(code admin POST "$IDE/echo" -d x)" = 403
if [ -z "${CODE_SERVER:-}" ]; then
  SESS=$(awk '$6 == "arm_session" {print $7}' "$ROOT/admin.jar")
  ECHO=$(curl -sf "$B$IDE/echo" -H "Cookie: arm_session=$SESS; other=1")
  check "Armageddon session cookie stripped before code-server" bash -c "! grep -q '$SESS' <<<'$ECHO' && grep -q other=1 <<<'$ECHO'"
  check "member WebSocket upgrades (101)" test "$(curl -s -o /dev/null -w '%{http_code}' -m 3 -b "$ROOT/admin.jar" "$B$IDE/ws" "${WSH[@]}" -H "Origin: $B" || true)" = 101
  ENVJSON=$(curl -sf -b "$ROOT/admin.jar" "$B$IDE/env")
  check "code-server's uid is the workspace user's" test "$(json 'd["uid"]' <<<"$ENVJSON")" = "$(id -u "$OWNER")"
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
# shellcheck disable=SC2016 # expanded by the remote shell
PTYOWNER=$("${SSH[@]}" -tt armageddon-ide 'stat -c %U "$(tty)"' 2>/dev/null | tr -d '\r')
check "the SSH session's tty belongs to $OWNER" test "$PTYOWNER" = "$OWNER"
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
python3 "$ROOT/bin/gitremote.py" "$PAT" "$ROOT/remotes" "$GIT_PORT" &
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

step "5. escapes from a second workspace"
WSB=$(api admin POST /api/workspaces '{"name":"other"}' | json 'd["id"]')
wait_ready "$WSB"
OWNER_B=$(stat -c %U "$DATA/workspaces/$WSB/tree")
pass "second workspace $WSB (user $OWNER_B)"
# connect.py SOCKET [REQUEST]: connect, send REQUEST, print the reply; exit
# 3 if the connection was refused by the file system.
cat >"$ROOT/bin/connect.py" <<'PY'
import socket, sys
s = socket.socket(socket.AF_UNIX)
s.settimeout(5)
try:
    s.connect(sys.argv[1])
except OSError as e:
    print("connect:", e.strerror)
    sys.exit(3)
if len(sys.argv) > 2:
    s.sendall(sys.argv[2].encode() + b"\n")
out = b""
try:
    while True:
        d = s.recv(65536)
        if not d:
            break
        out += d
except OSError:
    pass
sys.stdout.write(out.decode(errors="replace"))
PY
chmod 0644 "$ROOT/bin/connect.py"

# A's credential sockets exist while one of A's sessions is open.
api admin POST /api/credentials '{"host":"github.com","kind":"ssh-key"}' >/dev/null
# shellcheck disable=SC2016 # expanded by the remote shell
"${SSH[@]}" armageddon-ide 'echo "$ARMAGEDDON_CREDENTIAL_SOCKET $SSH_AUTH_SOCK"; exec sleep 120' >"$ROOT/session.env" &
SESSION_PID=$!
PIDS+=("$SESSION_PID")
for _ in $(seq 50); do [ -s "$ROOT/session.env" ] && break; sleep 0.2; done
read -r CRED_SOCK AGENT_SOCK <"$ROOT/session.env"
{ [ -S "$CRED_SOCK" ] && [ -S "$AGENT_SOCK" ]; } || fail "session sockets missing: $(cat "$ROOT/session.env")"
check "credential sockets are outside the workspace" bash -c "[[ '$CRED_SOCK' != $DATA/workspaces/* ]]"
check "their directory is $SERVER_USER's, mode 711" test "$(stat -c '%a %U' "$(dirname "$CRED_SOCK")")" = "711 $SERVER_USER"
REQ="{\"protocol\":\"http\",\"host\":\"127.0.0.1:$GIT_PORT\"}"
GOT_A=$(as_user "$OWNER" python3 "$ROOT/bin/connect.py" "$CRED_SOCK" "$REQ" || true)
check "workspace A's user gets the PAT from its credential socket (control)" bash -c "grep -q '$PAT' <<<'$GOT_A'"
GOT_B=$(as_user "$OWNER_B" python3 "$ROOT/bin/connect.py" "$CRED_SOCK" "$REQ" || true)
check "workspace B's user gets nothing from A's credential socket" bash -c "! grep -q '$PAT' <<<'$GOT_B' && ! grep -q password <<<'$GOT_B'"
check "the refusal is logged with B's uid" grep -q "refused a connection from uid $(id -u "$OWNER_B")" "$ROOT/server.log"
check "A's user lists the stored key through its agent socket (control)" bash -c "runuser -u '$OWNER' -- env SSH_AUTH_SOCK='$AGENT_SOCK' ssh-add -l | grep -q armageddon-abdul@github.com"
check "B's user cannot use A's agent socket" bash -c "! runuser -u '$OWNER_B' -- env SSH_AUTH_SOCK='$AGENT_SOCK' ssh-add -l >/dev/null 2>&1"
kill "$SESSION_PID" 2>/dev/null || true
wait "$SESSION_PID" 2>/dev/null || true

check "B's user cannot reach A's code-server socket" bash -c "as() { runuser -u '$OWNER_B' -- \"\$@\"; }; ! as python3 '$ROOT/bin/connect.py' '$RUNDIR/ide.sock' 'GET / HTTP/1.0' >/dev/null"
check "B's user cannot create a socket in A's run/" bash -c "! runuser -u '$OWNER_B' -- python3 -c 'import socket,sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' '$RUNDIR/planted.sock' 2>/dev/null"

# A listener of B's placed at A's code-server path (as if B could write
# there): the server must refuse it on SO_PEERCRED and send it nothing.
cat >"$ROOT/bin/listen.py" <<'PY'
import os, socket, sys
path, log = sys.argv[1], sys.argv[2]
s = socket.socket(socket.AF_UNIX)
s.bind(path)
os.chmod(path, 0o666)
s.listen(8)
with open(log, "ab", 0) as out:
    while True:
        c, _ = s.accept()
        c.settimeout(3)
        out.write(b"CONN\n")
        try:
            while True:
                d = c.recv(65536)
                if not d:
                    break
                out.write(d)
        except OSError:
            pass
        c.close()
PY
chmod 0644 "$ROOT/bin/listen.py"
plant() { # plant USER: USER listens on a socket that replaces A's ide.sock
  local u=$1 dir=$ROOT/plant-$1
  mkdir -p "$dir"; chown "$u:" "$dir"; chmod 700 "$dir"; rm -f "$dir/ide.sock" "$dir/got"
  runuser -u "$u" -- python3 "$ROOT/bin/listen.py" "$dir/ide.sock" "$dir/got" &
  PIDS+=($!)
  for _ in $(seq 50); do [ -S "$dir/ide.sock" ] && break; sleep 0.1; done
  mv -f "$dir/ide.sock" "$RUNDIR/ide.sock"
}
probe_ide() { curl -s -o /dev/null -w '%{http_code}' -m 10 -b "$ROOT/admin.jar" "$B$IDE/ws" "${WSH[@]}" -H "Origin: $B" -H "Cookie: probe=secret-cookie" || true; }
plant "$OWNER_B"
check "a socket of B's at A's code-server path is refused (502)" test "$(probe_ide)" = 502
check "the server refused it on the peer uid" grep -q "code-server socket is served by pid [0-9]* uid $(id -u "$OWNER_B")" "$ROOT/server.log"
check "B's listener received no request bytes" bash -c "[ \"\$(grep -v '^CONN\$' '$ROOT/plant-$OWNER_B/got' 2>/dev/null)\" = '' ]"
plant "$OWNER"
check "a socket of another process of A's is refused too (502)" test "$(probe_ide)" = 502
check "A's impostor received no request bytes" bash -c "[ \"\$(grep -v '^CONN\$' '$ROOT/plant-$OWNER/got' 2>/dev/null)\" = '' ]"

printf '\n\033[32mALL IDE CHECKS PASSED\033[0m\n'
