#!/usr/bin/env bash
# Privilege-split escape matrix (contract §2.5, plan M3.1/M3.2; the P6
# escape tests promoted to integration tests). Runs as root: starts
# `armageddon helper` as root and the server as the unprivileged
# `armageddon` user, then attacks the boundary from workspace users.
#
#   sudo test/integration/escape.sh [path/to/armageddon]
#
#   E1  a workspace user cannot read the DB, config, keys or checkpoints.git
#   E2  workspaces are isolated from each other
#   E3  a workspace user cannot signal the server or the helper
#   E4  a workspace user cannot use the helper socket (mode and SO_PEERCRED),
#       nor another workspace's authority socket
#   E5  poisoned repo.git / seat shadow config (core.hooksPath,
#       core.fsmonitor, filter driver) only ever runs as ws-<id>, with
#       no_new_privs, during push, fetch and capture
#   E6  symlink swaps and out-of-workspace paths are refused
#   H   the helper API refuses everything outside the allowlist
#   U   an MVP (root-owned) data directory is upgraded at server start
set -euo pipefail

BIN=$(readlink -f "${1:-$(command -v armageddon)}")
HERE=$(cd "$(dirname "$0")" && pwd)
PORT=${PORT:-8093}
B=http://127.0.0.1:$PORT
ROOT=${IT_ROOT:-/srv/armageddon-it-$$}
DATA=$ROOT/server
RUN=$ROOT/run
SOCK=$RUN/helper.sock
CTL="python3 $ROOT/helperctl.py" # copied below: the checkout may be unreadable to other users
LAPTOP=$ROOT/laptop
POISON=$ROOT/poison
SU=${SERVER_USER:-armageddon}
export ARMAGEDDON_CONFIG_DIR=$LAPTOP/config ARMAGEDDON_DATA_DIR=$LAPTOP/data
ADMIN_PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')

pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
fail() {
  printf '\033[31mFAIL\033[0m %s\n' "$*"
  for f in helper.log server.log hold.out; do
    [ -f "$ROOT/$f" ] && { echo "--- $f (tail)"; tail -20 "$ROOT/$f"; }
  done
  exit 1
}
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
refused() { local d=$1; shift; if "$@" >/dev/null 2>&1; then fail "$d"; else pass "$d"; fi; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

SERVER_PID= HELPER_PID= HOLD_PID=
cleanup() {
  for p in $HOLD_PID $SERVER_PID $HELPER_PID; do kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root"
id "$SU" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SU" 2>/dev/null || id "$SU" >/dev/null
rm -rf "$ROOT"; mkdir -p "$DATA" "$LAPTOP" "$POISON"; chmod 755 "$ROOT"; chown "$SU:" "$DATA"
install -m 0644 "$HERE/helperctl.py" "$ROOT/helperctl.py"
chmod 1777 "$POISON"

as_server() { setpriv --reuid="$SU" --regid="$SU" --clear-groups -- env HOME="$DATA" "$@"; }
start_helper() {
  "$BIN" helper --data "$DATA" --socket "$SOCK" --server-user "$SU" >>"$ROOT/helper.log" 2>&1 &
  HELPER_PID=$!
  for _ in $(seq 50); do [ -S "$SOCK" ] && return; sleep 0.1; done
  fail "helper did not start"
}
start_server() {
  setpriv --reuid="$SU" --regid="$SU" --clear-groups -- env HOME="$DATA" \
    "$BIN" server run --data "$DATA" --helper-socket "$SOCK" >>"$ROOT/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 50); do curl -sf "$B/healthz" >/dev/null && return; sleep 0.2; done
  cat "$ROOT/server.log"; fail "server did not start"
}
stop_server() { kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=; }
api() {
  curl -sf -b "$ROOT/jar" -c "$ROOT/jar" -X "$1" "$B$2" -H "X-CSRF-Token: $(cat "$ROOT/csrf" 2>/dev/null)" \
    -H 'Content-Type: application/json' ${3:+-d "$3"}
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
wait_ready() {
  for _ in $(seq 120); do
    st=$(api GET "/api/workspaces/$1" | json 'd["state"]')
    [ "$st" = ready ] && return; [ "$st" = failed ] && fail "workspace $1 failed"
    sleep 0.5
  done; fail "workspace $1 not ready"
}
seq_of() { api GET "/api/workspaces/$1" | json 'd["checkpoint_seq"]'; }
# ctl JSON [--stdio]: talk to the helper as the server user.
ctl() { as_server $CTL "$SOCK" "$@"; }
spawn_req() { # spawn_req WS KIND CWD ARGV-JSON
  printf '{"op":"SpawnInWorkspace","workspace":"%s","spawn":{"kind":"%s","argv":%s,"cwd":"%s"}}' "$1" "$2" "$4" "$3"
}

step "setup: helper as root, server as $SU, two workspaces, one paired device"
as_server "$BIN" server init --data "$DATA" --listen "127.0.0.1:$PORT" --public-url "$B" >/dev/null
start_helper
start_server
TOKEN=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1 | cut -d= -f2)
curl -sf -c "$ROOT/jar" -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"it\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/csrf"
A=$(api POST /api/workspaces '{"name":"alpha"}' | json 'd["id"]')
Bw=$(api POST /api/workspaces '{"name":"beta"}' | json 'd["id"]')
wait_ready "$A"; wait_ready "$Bw"
WA=$DATA/workspaces/$A WB=$DATA/workspaces/$Bw
UA=$(stat -c %U "$WA/tree") UB=$(stat -c %U "$WB/tree")
as_a() { (cd / && runuser -u "$UA" -- env HOME="$WA/home" bash -c "cd '$WA/tree' && $*"); }
pass "workspaces $A ($UA) and $Bw ($UB) ready"
"$BIN" login "$B" --name it-laptop >"$ROOT/login.out" 2>&1 &
LOGIN_PID=$!
CODE=
for _ in $(seq 50); do CODE=$(grep -o 'code=[A-Z-]*' "$ROOT/login.out" 2>/dev/null | head -1 | cut -d= -f2 || true); [ -n "$CODE" ] && break; sleep 0.2; done
api POST /api/pair/approve "{\"code\":\"$CODE\"}" >/dev/null
wait $LOGIN_PID || fail "pairing failed"
pass "device paired"

step "E1: workspace user vs server data"
for f in "$DATA/armageddon.db" "$DATA/server.json" "$WA/checkpoints.git/config" "$WA/checkpoints.git/HEAD"; do
  refused "E1 $UA cannot read $(basename "$(dirname "$f")")/$(basename "$f")" runuser -u "$UA" -- cat "$f"
done
refused "E1 $UA cannot list checkpoints.git" runuser -u "$UA" -- ls "$WA/checkpoints.git"
refused "E1 $UA cannot write the hooks directory" runuser -u "$UA" -- touch "$WA/hooks/evil"
refused "E1 $UA cannot write the workspace directory" runuser -u "$UA" -- mkdir "$WA/evil"
if [ -d "$DATA/keys" ]; then refused "E1 $UA cannot list keys/" runuser -u "$UA" -- ls "$DATA/keys"; fi
check "E1 database is 0600 $SU" test "$(stat -c '%a %U' "$DATA/armageddon.db")" = "600 $SU"

step "E2: workspaces are isolated from each other"
for d in tree repo.git home seat; do
  refused "E2 $UA cannot list $UB's $d" runuser -u "$UA" -- ls "$WB/$d"
done
refused "E2 $UA cannot write into $UB's tree" runuser -u "$UA" -- touch "$WB/tree/x"
refused "E2 $UA cannot read $UB's authority socket messages (connect refused by SO_PEERCRED)" \
  runuser -u "$UA" -- python3 -c '
import socket,sys
s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); s.sendall(b"{\"hook\":\"post-receive\",\"updates\":[[\"a\",\"b\",\"refs/heads/x\"]]}\n")
sys.exit(0 if s.recv(16).startswith(b"ok") else 1)' "$RUN/$Bw.sock"
check "E2 $UA can talk to its own authority socket" runuser -u "$UA" -- python3 -c '
import socket,sys
s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); s.sendall(b"{\"hook\":\"post-receive\",\"updates\":[[\"a\",\"b\",\"refs/heads/x\"]]}\n")
sys.exit(0 if s.recv(16).startswith(b"ok") else 1)' "$RUN/$A.sock"

step "E3: signals"
refused "E3 $UA cannot signal the server" runuser -u "$UA" -- kill -0 "$SERVER_PID"
refused "E3 $UA cannot signal the helper" runuser -u "$UA" -- kill -0 "$HELPER_PID"
refused "E3 $UA cannot read the server's environment" runuser -u "$UA" -- cat "/proc/$SERVER_PID/environ"
# A long-running process of workspace B, started through the helper.
ctl "$(spawn_req "$Bw" runtime-command "$WB/tree" '["sleep","300"]')" --stdio </dev/null >/dev/null 2>"$ROOT/hold.out" &
HOLD_PID=$!
for _ in $(seq 50); do HOLD=$(pgrep -u "$UB" -x sleep || true); [ -n "$HOLD" ] && break; sleep 0.1; done
[ -n "$HOLD" ] || fail "could not start a process in $Bw"
refused "E3 $UA cannot signal $UB's processes" runuser -u "$UA" -- kill -0 "$HOLD"
HANDLE=$(head -1 "$ROOT/hold.out" | json 'd["handle"]')
refused "E3 SignalWorkspace refuses $Bw's handle under workspace $A" ctl "{\"op\":\"SignalWorkspace\",\"workspace\":\"$A\",\"signal\":{\"handle\":\"$HANDLE\",\"signal\":9}}"
ctl "{\"op\":\"SignalWorkspace\",\"workspace\":\"$A\",\"signal\":{\"handle\":\"all\",\"signal\":9}}" 2>/dev/null
check "E3 SignalWorkspace(all) on $A leaves $Bw's process alive" kill -0 "$HOLD"
check "E3 SignalWorkspace(all) on $A leaves the server alive" kill -0 "$SERVER_PID"
check "E3 SignalWorkspace(handle) ends $Bw's process" ctl "{\"op\":\"SignalWorkspace\",\"workspace\":\"$Bw\",\"signal\":{\"handle\":\"$HANDLE\",\"signal\":15}}"
wait "$HOLD_PID" 2>/dev/null || true; HOLD_PID=
check "E3 helper streamed the exit status (signal 15)" bash -c "tail -1 '$ROOT/hold.out' | grep -q '\"signal\": 15'"

step "E4: helper socket"
check "E4 helper socket is 0600 $SU" test "$(stat -c '%a %U' "$SOCK")" = "600 $SU"
refused "E4 $UA cannot connect to the helper socket" runuser -u "$UA" -- $CTL "$SOCK" '{"op":"RepairDataOwnership"}'
chmod 666 "$SOCK"
set +e; runuser -u "$UA" -- $CTL "$SOCK" "{\"op\":\"CreateWorkspaceUser\",\"workspace\":\"$A\"}" 2>/dev/null; rc=$?; set -e
chmod 600 "$SOCK"
check "E4 even with the socket mode opened, SO_PEERCRED refuses $UA (rc=$rc)" test "$rc" = 2
check "E4 the helper logged the refused uid" grep -q "refused connection from uid $(id -u "$UA")" "$ROOT/helper.log"

step "H: helper API allowlist"
refused "H unknown operation" ctl '{"op":"RunAsRoot","workspace":"'"$A"'"}'
refused "H unknown field" ctl '{"op":"CreateWorkspaceUser","workspace":"'"$A"'","uid":0}'
refused "H workspace ID with a path" ctl '{"op":"PrepareWorkspaceDirs","workspace":"../../etc"}'
refused "H git-service runs only git" ctl "$(spawn_req "$A" git-service "$WA/tree" '["sh","-c","id"]')" --stdio
refused "H pty-shell runs only a shell" ctl "$(spawn_req "$A" pty-shell "$WA/tree" '["/usr/bin/python3"]')"
refused "H reserved kind code-server" ctl "$(spawn_req "$A" code-server "$WA/tree" '["true"]')" --stdio
refused "H cwd outside the workspace" ctl "$(spawn_req "$A" runtime-command / '["true"]')" --stdio
refused "H stdio-less spawn" ctl "$(spawn_req "$A" runtime-command "$WA/tree" '["true"]')"
refused "H limits without cgroup v2 or bad values" ctl '{"op":"SetWorkspaceLimits","workspace":"'"$A"'","limits":{"pids":-1}}'
OUT=$(ctl "$(spawn_req "$A" runtime-command "$WA/tree" '["sh","-c","id -u; id -G; grep NoNewPrivs /proc/self/status; echo HOME=$HOME; env | grep -c LD_PRELOAD || true"]')" --stdio 2>/dev/null </dev/null)
check "H spawn runs as $UA, never root" test "$(echo "$OUT" | sed -n 1p)" = "$(id -u "$UA")"
check "H spawn has no supplementary groups" test "$(echo "$OUT" | sed -n 2p)" = "$(id -g "$UA")"
check "H spawn has no_new_privs" bash -c "echo '$OUT' | grep -q 'NoNewPrivs:[[:space:]]*1'"
check "H spawn HOME is the workspace home" bash -c "echo '$OUT' | grep -qx 'HOME=$WA/home'"
OUT=$(ctl "$(spawn_req "$A" runtime-command "$WA/tree" '["env"]' | sed 's/"cwd"/"env":["LD_PRELOAD=\/tmp\/x.so","GODEBUG=x","GIT_OK=1"],"cwd"/')" --stdio 2>/dev/null </dev/null)
check "H env allowlist drops LD_PRELOAD/GODEBUG, keeps GIT_*" bash -c "echo '$OUT' | grep -q '^GIT_OK=1' && ! echo '$OUT' | grep -q 'LD_PRELOAD\|GODEBUG'"

step "E6: symlinks"
as_a "ln -s / '$WA/tree/escape'"
refused "E6 cwd through a symlink is refused" ctl "$(spawn_req "$A" runtime-command "$WA/tree/escape/etc" '["true"]')" --stdio
mv "$WA/home" "$WA/home.real"; ln -s /etc "$WA/home"
refused "E6 PrepareWorkspaceDirs refuses a symlinked home" ctl "{\"op\":\"PrepareWorkspaceDirs\",\"workspace\":\"$A\"}"
check "E6 /etc ownership untouched" test "$(stat -c %u /etc)" = 0
rm "$WA/home"; mv "$WA/home.real" "$WA/home"
mv "$WA" "$WA.real"; ln -s "$WB" "$WA"
refused "E6 PrepareWorkspaceDirs refuses a symlinked workspace directory" ctl "{\"op\":\"PrepareWorkspaceDirs\",\"workspace\":\"$A\"}"
check "E6 $UB's tree still owned by $UB" test "$(stat -c %U "$WB/tree")" = "$UB"
rm "$WA"; mv "$WA.real" "$WA"
check "E6 PrepareWorkspaceDirs succeeds on the restored layout" ctl "{\"op\":\"PrepareWorkspaceDirs\",\"workspace\":\"$A\"}"

step "E5: poisoned repository config only runs as the workspace user"
cat >"$POISON/p.sh" <<'EOF'
#!/bin/sh
# Records who ran it; behaves as a no-op hook / filter / fsmonitor.
echo "$(id -u) $(grep NoNewPrivs /proc/self/status | tr -d '[:space:]') $0 $*" >>"$(dirname "$(readlink -f "$0")")/log"
case "$1" in clean|smudge) cat ;; esac
exit 0
EOF
chmod 755 "$POISON/p.sh"
mkdir -p "$POISON/hooks"
for h in pre-receive update post-receive post-update reference-transaction pre-commit post-checkout; do ln -sf ../p.sh "$POISON/hooks/$h"; done
for cfg in "$WA/repo.git/config" "$WA/seat/shadow.git/config"; do
  as_a "git config -f '$cfg' core.hooksPath '$POISON/hooks' && git config -f '$cfg' core.fsmonitor '$POISON/p.sh' && git config -f '$cfg' filter.evil.clean '$POISON/p.sh clean' && git config -f '$cfg' filter.evil.smudge '$POISON/p.sh smudge' && git config -f '$cfg' filter.evil.required false && git config -f '$cfg' diff.external '$POISON/p.sh' && git config -f '$cfg' core.sshCommand '$POISON/p.sh'"
done
as_a "cd '$WA/tree' && echo '* filter=evil' > .gitattributes && echo poisoned > p.txt"
S0=$(seq_of "$A")
api POST "/api/workspaces/$A/sync" '' >/dev/null
check "E5 capture still produced a checkpoint" test "$(seq_of "$A")" -gt "$S0"
cd "$LAPTOP"
"$BIN" clone alpha replica >/dev/null 2>&1 || fail "clone (fetch) failed"
git -C replica fetch -q armageddon || fail "fetch failed"
git -C replica push -q armageddon HEAD:refs/heads/it 2>/dev/null || true # refused: lease (receive-pack advertisement still runs)
as_a "cd '$WA/tree' && git add p.txt && git commit -qm poisoned"  # local commit: the hooks run as $UA
sleep 1
[ -s "$POISON/log" ] || fail "E5 the poisoned config never ran (test is not exercising anything)"
echo "    poisoned commands ran $(wc -l <"$POISON/log") times:"; sed 's/^/      /' "$POISON/log" | sort | uniq -c | sort -rn | head -8
check "E5 every poisoned command ran as $UA ($(id -u "$UA"))" bash -c "! awk '{print \$1}' '$POISON/log' | grep -vqx '$(id -u "$UA")'"
check "E5 server-triggered ones ran with no_new_privs" bash -c "grep -q 'NoNewPrivs:1' '$POISON/log'"
check "E5 nothing ran as root or $SU" bash -c "! awk '{print \$1}' '$POISON/log' | grep -qx -e 0 -e '$(id -u "$SU")'"

step "U: upgrade an MVP (root-owned) data directory"
stop_server
# What an MVP server running as root left behind.
for p in "$DATA" "$DATA/armageddon.db" "$DATA/server.json" "$DATA/bin" "$WA" "$WA/checkpoints.git" "$WA/hooks" "$WB" "$WB/checkpoints.git"; do chown root:root "$p"; done
chown -R root:root "$WA/checkpoints.git" "$DATA/bin"
chmod 600 "$DATA/server.json"
ln -s /etc/shadow "$DATA/evil-link"
touch "$WA/home/root-file"; chown root:root "$WA/home/root-file"
start_server
check "U server started on the MVP layout" curl -sf "$B/healthz"
check "U database handed to $SU" test "$(stat -c %U "$DATA/armageddon.db")" = "$SU"
check "U checkpoints.git handed to $SU (recursively)" bash -c "test -z \"\$(find '$WA/checkpoints.git' ! -user '$SU' | head -1)\""
check "U symlink target not followed (/etc/shadow still root)" test "$(stat -c %u /etc/shadow)" = 0
check "U workspace-owned trees are not entered" test "$(stat -c %U "$WA/home/root-file")" = root
check "U helper logged the upgrade" grep -q "data directory upgrade" "$ROOT/helper.log"
as_a "echo after-upgrade > '$WA/tree/u.txt'"
S1=$(seq_of "$A"); api POST "/api/workspaces/$A/sync" '' >/dev/null
check "U capture works after the upgrade" test "$(seq_of "$A")" -gt "$S1"

printf '\n\033[32mALL ESCAPE-MATRIX CHECKS PASSED\033[0m\n'
