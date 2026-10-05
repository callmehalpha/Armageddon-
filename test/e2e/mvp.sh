#!/usr/bin/env bash
# MVP acceptance test (implementation plan §0), with the privilege split of
# M3.1: `armageddon helper` runs as root, `armageddon server run` runs as the
# unprivileged `armageddon` user (created if missing), and a simulated laptop
# runs on the same machine.
#
#   sudo test/e2e/mvp.sh [path/to/armageddon]
#
# Steps:
#   1. server init/run; first admin via the one-time setup URL
#   2. create a workspace from a Git URL and an empty one
#   3. edit + commit on the server seat (as the workspace user)
#   4. pair a "laptop", clone the workspace, follow it
#   5. destroy the laptop replica, clone again: identical tree, index, history
#   6. a local edit on the replica is quarantined, never silently lost
#   7. uncommitted work survives a server restart
#   8. git access control
#   9. only the helper runs as root
set -Eeuo pipefail
# A failing command outside `check` aborts the run: say which one.
trap 'printf "\033[31mFAIL\033[0m command failed (line %s): %s\n" "$LINENO" "$BASH_COMMAND"' ERR

BIN=${1:-$(command -v armageddon)}
BIN=$(readlink -f "$BIN")
PORT=${PORT:-8091}
B=http://127.0.0.1:$PORT
ROOT=${E2E_ROOT:-/srv/armageddon-e2e-$$}
DATA=$ROOT/server
LAPTOP=$ROOT/laptop
RUN=$ROOT/run
SOCK=$RUN/helper.sock
SERVER_USER=${SERVER_USER:-armageddon}
export ARMAGEDDON_CONFIG_DIR=$LAPTOP/config ARMAGEDDON_DATA_DIR=$LAPTOP/data
SOURCE_URL=${SOURCE_URL:-https://github.com/octocat/Hello-World.git}
# Throwaway credentials for the throwaway server this test creates.
ADMIN_PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')

pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
# check DESCRIPTION CMD...: a failed check always aborts (plain `cmd && pass`
# would silently skip under set -e).
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
SERVER_PID= HELPER_PID= FOLLOW_PID=
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$HELPER_PID" ] && kill "$HELPER_PID" 2>/dev/null || true
  [ -n "$FOLLOW_PID" ] && kill "$FOLLOW_PID" 2>/dev/null || true
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root (the helper needs it; the server drops to $SERVER_USER)"
id "$SERVER_USER" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SERVER_USER" 2>/dev/null || id "$SERVER_USER" >/dev/null
rm -rf "$ROOT"; mkdir -p "$ROOT" "$LAPTOP" "$DATA"; chmod 755 "$ROOT"
chown "$SERVER_USER:" "$DATA"

# as_server CMD...: run as the server user in the foreground.
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
api() { # api METHOD PATH [JSON]
  curl -sf -b "$ROOT/jar" -c "$ROOT/jar" -X "$1" "$B$2" -H "X-CSRF-Token: $(cat "$ROOT/csrf" 2>/dev/null)" \
    -H 'Content-Type: application/json' ${3:+-d "$3"}
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
wait_ready() { # wait_ready ID
  for _ in $(seq 120); do
    st=$(api GET "/api/workspaces/$1" | json 'd["state"]')
    [ "$st" = ready ] && return; [ "$st" = failed ] && fail "workspace $1 failed: $(api GET /api/workspaces/$1 | json 'd["state_reason"]')"
    sleep 0.5
  done; fail "workspace $1 not ready"
}
as_ws() { # run a command in the server worktree as the workspace user
  local id=$1; shift
  local tree=$DATA/workspaces/$id/tree user; user=$(stat -c %U "$tree")
  runuser -u "$user" -- env HOME="$DATA/workspaces/$id/home" bash -c "cd '$tree' && $*"
}
seq_of() { api GET "/api/workspaces/$1" | json 'd["checkpoint_seq"]'; }
wait_seq_gt() { # wait_seq_gt ID N
  for _ in $(seq 60); do [ "$(seq_of "$1")" -gt "$2" ] && return; sleep 0.5; done
  fail "no new checkpoint after #$2"
}
# tree_sum DIR: one line per synced path (tracked + untracked, not ignored):
# type, exec bit, content hash or link target. Missing tracked files show as
# "missing". Run as the directory's owner so git accepts the repository.
tree_sum() {
  local owner; owner=$(stat -c %U "$1")
  runuser -u "$owner" -- env HOME=/tmp git -C "$1" ls-files -co --exclude-standard -z | sort -zu | python3 -c '
import hashlib, os, sys
root = sys.argv[1]
for p in sys.stdin.buffer.read().split(b"\0"):
    if not p: continue
    f = os.path.join(root.encode(), p)
    if os.path.islink(f): print("link", os.readlink(f).decode(), p.decode())
    elif os.path.isfile(f): print("file", oct(os.stat(f).st_mode & 0o111), hashlib.sha256(open(f, "rb").read()).hexdigest(), p.decode())
    else: print("missing", p.decode())
' "$1"
}
# settle ID LOG: capture the server seat's final state, then wait until the
# follower writing LOG has applied that checkpoint. The capture loop can commit
# a checkpoint of a half-finished edit script; waiting only for "a newer seq"
# races with it.
settle() {
  api POST "/api/workspaces/$1/sync" '' >/dev/null
  local s; s=$(seq_of "$1")
  for _ in $(seq 60); do grep -qE "applied checkpoint #$s\$" "$2" && return; sleep 0.5; done
  fail "follower did not apply checkpoint #$s"
}
same_tree() { # same_tree A B
  tree_sum "$1" >"$ROOT/a.sum" && tree_sum "$2" >"$ROOT/b.sum" && [ -s "$ROOT/a.sum" ] && diff -u "$ROOT/a.sum" "$ROOT/b.sum"
}
same_index() { [ "$(git -C "$LAPTOP/replica" ls-files -s)" = "$(as_ws "$WS" 'git ls-files -s')" ]; }
same_head() { [ "$(git -C "$LAPTOP/replica" rev-parse HEAD)" = "$(as_ws "$WS" 'git rev-parse HEAD')" ]; }

step "1. server and first admin"
as_server "$BIN" server init --data "$DATA" --listen "127.0.0.1:$PORT" --public-url "$B" >/dev/null
start_helper
start_server
check "helper runs as root" test "$(ps -o uid= -p "$HELPER_PID" | tr -d ' ')" = 0
check "server runs as $SERVER_USER" test "$(ps -o uid= -p "$SERVER_PID" | tr -d ' ')" = "$(id -u "$SERVER_USER")"
check "helper socket is 0600 and owned by $SERVER_USER" test "$(stat -c '%a %U' "$SOCK")" = "600 $SERVER_USER"
TOKEN=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1 | cut -d= -f2)
[ -n "$TOKEN" ] || fail "no setup token printed"
curl -sf -c "$ROOT/jar" -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"abdul\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/csrf"
pass "admin created from the setup URL"
reuse() { curl -s -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"intruder\",\"password\":\"$ADMIN_PW\"}" | grep -q "already used"; }
check "setup URL is single-use" reuse

step "2. workspaces"
WS=$(api POST /api/workspaces "{\"name\":\"hello\",\"source_url\":\"$SOURCE_URL\"}" | json 'd["id"]')
EMPTY=$(api POST /api/workspaces '{"name":"scratch"}' | json 'd["id"]')
wait_ready "$WS"; wait_ready "$EMPTY"
pass "cloned workspace $WS and empty workspace $EMPTY are ready"
OWNER=$(stat -c %U "$DATA/workspaces/$WS/tree")
check "worktree owned by isolated user $OWNER" test "${OWNER#ws-}" != "$OWNER"
check "workspace user cannot read the database" bash -c "! runuser -u $OWNER -- cat '$DATA/armageddon.db' >/dev/null 2>&1"
check "workspace user cannot read the checkpoints repository" bash -c "! runuser -u $OWNER -- ls '$DATA/workspaces/$WS/checkpoints.git' >/dev/null 2>&1"
check "workspace user cannot connect to the helper socket" bash -c "! runuser -u $OWNER -- python3 -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])' '$SOCK' 2>/dev/null"
check "checkpoints.git is owned by the server user" test "$(stat -c %U "$DATA/workspaces/$WS/checkpoints.git")" = "$SERVER_USER"
OTHER=$(stat -c %U "$DATA/workspaces/$EMPTY/tree")
check "workspace users are isolated from each other ($OWNER vs $OTHER)" bash -c "! runuser -u $OWNER -- ls '$DATA/workspaces/$EMPTY/tree' >/dev/null 2>&1"

step "3. work on the server seat"
S0=$(seq_of "$WS")
as_ws "$WS" "printf 'hello from the server\r\n' > notes.txt && mkdir -p src && echo 'package main' > src/main.go && git add src && git commit -qm 'add main' && echo 'staged' > staged.txt && git add staged.txt && echo 'unstaged edit' >> staged.txt && echo 'API_KEY=dev' > .env"
wait_seq_gt "$WS" "$S0"
S1=$(seq_of "$WS")
pass "edits captured as checkpoint #$S1 (commit, staged, unstaged, untracked, .env)"
api POST "/api/workspaces/$WS/sync" '' >/dev/null
check "no-op sync creates no checkpoint" test "$(seq_of "$WS")" = "$S1"

step "4. laptop: pair, clone, follow"
"$BIN" login "$B" --name test-laptop >"$ROOT/login.out" 2>&1 &
LOGIN_PID=$!
CODE=
for _ in $(seq 50); do CODE=$(grep -o 'code=[A-Z-]*' "$ROOT/login.out" 2>/dev/null | head -1 | cut -d= -f2 || true); [ -n "$CODE" ] && break; sleep 0.2; done
[ -n "$CODE" ] || fail "login printed no code"
check "approval page shows the device" bash -c "curl -sf -b '$ROOT/jar' '$B/api/pair/lookup?code=$CODE' | grep -q test-laptop"
api POST /api/pair/approve "{\"code\":\"$CODE\"}" >/dev/null
wait $LOGIN_PID && pass "device paired by approving code $CODE in the browser session"
cd "$LAPTOP"
"$BIN" clone hello replica >/dev/null
check "replica working tree is byte-identical (incl. uncommitted work and .env)" same_tree "$DATA/workspaces/$WS/tree" "$LAPTOP/replica"
check "replica has the .env file" test -f "$LAPTOP/replica/.env"
check "replica index (staged state) is identical" same_index
check "replica HEAD and history are identical" same_head
(cd "$LAPTOP/replica" && exec "$BIN" follow >"$ROOT/follow.log" 2>&1) &
FOLLOW_PID=$!
sleep 1
as_ws "$WS" "echo 'live edit' >> notes.txt && git rm -q --cached .env >/dev/null 2>&1 || true; rm -rf src && git checkout -q -b feature && echo f > feature.txt && git add feature.txt && git commit -qm feature"
settle "$WS" "$ROOT/follow.log"
check "replica follows live edits, deletes and a branch switch + commit" same_tree "$DATA/workspaces/$WS/tree" "$LAPTOP/replica"
check "replica index follows" same_index
check "replica HEAD follows the branch switch" test "$(git -C "$LAPTOP/replica" symbolic-ref HEAD)" = refs/heads/feature

step "5. lose the laptop replica, recover it"
check "a second follower on the same replica is refused" bash -c "cd '$LAPTOP/replica' && timeout 5 '$BIN' follow 2>&1 | grep -q 'in use'"
kill $FOLLOW_PID 2>/dev/null; wait $FOLLOW_PID 2>/dev/null || true
rm -rf "$LAPTOP/replica"
"$BIN" clone hello replica >/dev/null
check "fresh clone after losing the replica is identical" same_tree "$DATA/workspaces/$WS/tree" "$LAPTOP/replica"
check "recovered index identical" same_index
check "recovered HEAD identical" same_head

step "6. local edits on a read-only replica are quarantined"
echo "edited on the laptop" > "$LAPTOP/replica/laptop-only.txt"
check "status reports the replica as dirty" bash -c "cd '$LAPTOP/replica' && '$BIN' status | grep -q DIRTY"
(cd "$LAPTOP/replica" && exec "$BIN" follow >"$ROOT/follow2.log" 2>&1) &
FOLLOW_PID=$!
sleep 1
as_ws "$WS" "echo 'server moves on' >> notes.txt"
for _ in $(seq 40); do grep -q "quarantine" "$ROOT/follow2.log" && break; sleep 0.5; done
check "local edit uploaded as a quarantine before being overwritten" grep -q "saved to the server as quarantine" "$ROOT/follow2.log"
settle "$WS" "$ROOT/follow2.log"
check "server lists the quarantine" bash -c "curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS/quarantines' | grep -q 'follower dirty'"
QCP=$(api GET "/api/workspaces/$WS/quarantines" | json 'd[0]["CheckpointID"]')
check "quarantined content is intact on the server" bash -c "git --git-dir='$DATA/workspaces/$WS/checkpoints.git' cat-file -p '$QCP:worktree/laptop-only.txt' | grep -q 'edited on the laptop'"
check "replica converged to the server after quarantining" same_tree "$DATA/workspaces/$WS/tree" "$LAPTOP/replica"
kill $FOLLOW_PID 2>/dev/null; wait $FOLLOW_PID 2>/dev/null || true

step "7. uncommitted work survives a server restart"
as_ws "$WS" "echo 'not committed, not pushed' > wip.txt"
S3=$(seq_of "$WS"); wait_seq_gt "$WS" "$S3"
kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true
start_server
check "work-in-progress still on the server seat after restart" grep -q "not committed" "$DATA/workspaces/$WS/tree/wip.txt"
check "checkpoints still served after restart" test "$(seq_of "$WS")" -gt "$S3"
rm -rf "$LAPTOP/replica" && cd "$LAPTOP" && "$BIN" clone hello replica >/dev/null
check "and reaches a newly cloned replica" grep -q "not committed" "$LAPTOP/replica/wip.txt"

step "8. git access control"
check "push from a read-only replica is refused with lease_lost" bash -c "! git -C '$LAPTOP/replica' push -q armageddon HEAD:refs/heads/from-laptop 2>'$ROOT/push.err' && grep -q lease_lost '$ROOT/push.err'"
check "anonymous git access refused" test "$(curl -s -o /dev/null -w '%{http_code}' "$B/git/$WS.git/info/refs?service=git-upload-pack")" = 401
DELETED=$(as_ws "$WS" "git rev-parse master 2>/dev/null || git rev-parse HEAD~1")
as_ws "$WS" "git branch -q doomed $DELETED && git branch -q -D doomed"
check "deleting a branch on the server seat leaves a recoverable trash ref" bash -c "runuser -u $OWNER -- git --git-dir='$DATA/workspaces/$WS/repo.git' for-each-ref --format='%(objectname) %(refname)' refs/armageddon/trash/ | grep -q '$DELETED .*/delete/heads/doomed'"
check "trash ref hidden from clients" bash -c "! git -C '$LAPTOP/replica' ls-remote armageddon | grep -q refs/armageddon"
check "hooks reported the deletion to the authority socket" bash -c "curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS/events' | grep -q refs.updated"

step "9. privilege split"
# Every process of this binary that runs as root must be the helper itself.
ROOT_PIDS=$(ps -eo pid=,uid=,args= | awk -v bin="$BIN" '$2 == 0 && $3 == bin {print $1}' | sort -u)
check "only the helper runs as root (root pids: $(echo $ROOT_PIDS))" test "$(echo $ROOT_PIDS)" = "$HELPER_PID"

printf '\n\033[32mALL MVP ACCEPTANCE CHECKS PASSED\033[0m\n'
