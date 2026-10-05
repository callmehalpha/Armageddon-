#!/usr/bin/env bash
# Local write mode acceptance test (plan M7, contract §3.2–§4.5, §10).
# A real server as root (per-workspace users) and a simulated laptop whose
# connection goes through a TCP proxy, so the test can take it offline.
#
#   sudo test/e2e/write.sh [path/to/armageddon]
#
#   1. north star: the laptop takes the workspace (`work local`), writes
#      files, commits and branches; the server worktree follows; dev
#      processes on the server stopped; `work remote` hands it back
#   2. F11: a Git operation in progress refuses a graceful handoff
#   3. F4: edits made offline on a follower fast-forward on `work local`
#      when the server has not moved
#   4. F18: edits on a follower while the server seat writes are quarantined
#   5. F3: offline with the lease, checkpoints queue and commit in order
#   6. F3: the lease is taken while the laptop is offline: its queue is
#      quarantined on reconnect
#   7. F2: the laptop dies holding the lease; STALE; forced takeover with
#      password re-auth; the returning laptop's tail is quarantined
#   8. quarantine CLI: list, diff, export, apply (3-way), drop
#   9. revoking the holder is an immediate takeover
set -euo pipefail

BIN=${1:-$(command -v armageddon)}
BIN=$(readlink -f "$BIN")
HERE=$(cd "$(dirname "$0")" && pwd)
PORT=${PORT:-8093}
PROXY_PORT=${PROXY_PORT:-$((PORT + 1))}
B=http://127.0.0.1:$PORT
P=http://127.0.0.1:$PROXY_PORT
ROOT=${E2E_ROOT:-/srv/armageddon-write-$$}
DATA=$ROOT/server
LAPTOP=$ROOT/laptop
R=$LAPTOP/replica
export ARMAGEDDON_CONFIG_DIR=$LAPTOP/config ARMAGEDDON_DATA_DIR=$LAPTOP/data
export GIT_AUTHOR_NAME=laptop GIT_AUTHOR_EMAIL=laptop@example.com GIT_COMMITTER_NAME=laptop GIT_COMMITTER_EMAIL=laptop@example.com
ADMIN_PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')

pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
fail() {
  printf '\033[31mFAIL\033[0m %s\n' "$*"
  echo "--- agent log (tail)"; tail -40 "$ROOT/agent.log" 2>/dev/null || true
  echo "--- server log (tail)"; tail -20 "$ROOT/server.log" 2>/dev/null || true
  exit 1
}
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
SERVER_PID= PROXY_PID= AGENT_PID= SLEEP_PID=
cleanup() {
  for p in $AGENT_PID $PROXY_PID $SERVER_PID $SLEEP_PID; do kill -9 "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root (workspace isolation needs it)"
rm -rf "$ROOT"; mkdir -p "$ROOT" "$LAPTOP"; chmod 755 "$ROOT"

start_server() {
  "$BIN" server run --data "$DATA" >>"$ROOT/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 50); do curl -sf "$B/healthz" >/dev/null && return; sleep 0.2; done
  fail "server did not start"
}
stop_server() { kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=; }
proxy_up() { python3 "$HERE/proxy.py" "$PROXY_PORT" "$PORT" & PROXY_PID=$!; sleep 0.3; }
proxy_down() { kill -9 "$PROXY_PID" 2>/dev/null || true; wait "$PROXY_PID" 2>/dev/null || true; PROXY_PID=; }
agent_up() { "$BIN" agent run >>"$ROOT/agent.log" 2>&1 & AGENT_PID=$!; sleep 1; }
agent_kill() { kill -9 "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; AGENT_PID=; }
api() { # api METHOD PATH [JSON]
  curl -sf -b "$ROOT/jar" -c "$ROOT/jar" -X "$1" "$B$2" -H "X-CSRF-Token: $(cat "$ROOT/csrf" 2>/dev/null)" \
    -H 'Content-Type: application/json' ${3:+-d "$3"}
}
api_code() { # api_code METHOD PATH JSON → HTTP status
  curl -s -o "$ROOT/last.json" -w '%{http_code}' -b "$ROOT/jar" -X "$1" "$B$2" -H "X-CSRF-Token: $(cat "$ROOT/csrf")" \
    -H 'Content-Type: application/json' -d "$3"
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
ws_json() { api GET "/api/workspaces/$WS" | json "$1"; }
holder() { ws_json 'd["lease"]["holder"]'; }
epoch() { ws_json 'd["lease"]["epoch"]'; }
lstate() { ws_json 'd["lease"]["state"]'; }
seq_of() { ws_json 'd["checkpoint_seq"]'; }
as_ws() { # run a command in the server worktree as the workspace user
  runuser -u "$OWNER" -- env HOME="$DATA/workspaces/$WS/home" bash -c "cd '$TREE' && $*"
}
# eventually DESC CMD...: retry for up to 30 s.
eventually() {
  local d=$1; shift
  for _ in $(seq 60); do if "$@" >/dev/null 2>&1; then pass "$d"; return; fi; sleep 0.5; done
  fail "$d"
}
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
same_tree() { tree_sum "$TREE" >"$ROOT/a.sum" && tree_sum "$R" >"$ROOT/b.sum" && [ -s "$ROOT/a.sum" ] && diff -q "$ROOT/a.sum" "$ROOT/b.sum" >/dev/null; }
# settle: capture the server seat now and wait until the laptop applied it.
settle() {
  api POST "/api/workspaces/$WS/sync" '' >/dev/null
  local s; s=$(seq_of)
  for _ in $(seq 60); do grep -qE "(applied|committed) checkpoint #$s\$" "$ROOT/agent.log" && return; sleep 0.5; done
  fail "laptop did not apply checkpoint #$s"
}
quarantine_count() { api GET "/api/workspaces/$WS/quarantines" | json 'len(d)'; }
# quarantined_has TEXT: some quarantine's tree has a file containing TEXT.
quarantined_has() {
  local cps=$DATA/workspaces/$WS/checkpoints.git q
  for q in $(api GET "/api/workspaces/$WS/quarantines" | json '" ".join(x["CheckpointID"] for x in d)'); do
    git --git-dir="$cps" grep -q -F "$1" "$q:worktree" -- 2>/dev/null && return 0
  done
  return 1
}
in_cp_tree() { # in_cp_tree FILE: FILE is in the current checkpoint's tree
  git --git-dir="$DATA/workspaces/$WS/checkpoints.git" cat-file -e "$(ws_json 'd["current_checkpoint"]'):worktree/$1"
}
laptop() { (cd "$R" && "$BIN" "$@"); }

step "0. server, admin, workspace, laptop"
"$BIN" server init --data "$DATA" --listen "127.0.0.1:$PORT" --public-url "$P" >/dev/null
python3 - "$DATA/server.json" <<'EOF'
import json, sys
p = sys.argv[1]; c = json.load(open(p))
c["lease_stale_ms"] = 6000      # T_stale shortened for the test (default 2 min)
c["handoff_timeout_ms"] = 10000 # T_handoff (default 30 s)
c["capture_interval_ms"] = 1000
json.dump(c, open(p, "w"))
EOF
start_server
proxy_up
TOKEN=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1 | cut -d= -f2)
curl -sf -c "$ROOT/jar" -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"abdul\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/csrf"
WS=$(api POST /api/workspaces '{"name":"writer"}' | json 'd["id"]')
for _ in $(seq 60); do [ "$(ws_json 'd["state"]')" = ready ] && break; sleep 0.5; done
TREE=$DATA/workspaces/$WS/tree
OWNER=$(stat -c %U "$TREE")
as_ws "mkdir -p src && echo 'package main' > src/main.go && echo 'v1' > notes.txt && git add -A && git commit -qm init"
"$BIN" login "$P" --name test-laptop >"$ROOT/login.out" 2>&1 &
LOGIN_PID=$!
CODE=
for _ in $(seq 50); do CODE=$(grep -o 'code=[A-Z-]*' "$ROOT/login.out" 2>/dev/null | head -1 | cut -d= -f2 || true); [ -n "$CODE" ] && break; sleep 0.2; done
api POST /api/pair/approve "{\"code\":\"$CODE\"}" >/dev/null
wait $LOGIN_PID
DEV=$(api GET /api/devices | json 'd[0]["id"]')
api POST "/api/workspaces/$WS/sync" '' >/dev/null
(cd "$LAPTOP" && "$BIN" clone writer replica >/dev/null)
check "work local refuses while no agent runs for the replica" bash -c "cd '$R' && ! '$BIN' work local >'$ROOT/noagent.out' 2>&1 && grep -q 'no agent is running' '$ROOT/noagent.out'"
agent_up
check "replica cloned and identical" same_tree
check "lease starts with the server seat at epoch 1" test "$(holder) $(epoch)" = "server 1"

step "1. north star: take the workspace local, write, hand it back"
runuser -u "$OWNER" -- sleep 1000 &
SLEEP_PID=$!
sleep 0.3
laptop work local >"$ROOT/work-local.out" 2>&1 || { cat "$ROOT/work-local.out"; fail "work local"; }
check "work local grants the lease to the laptop (epoch 2)" test "$(holder) $(epoch)" = "device:$DEV 2"
check "the agent reports WRITING" grep -q WRITING "$ROOT/work-local.out"
eventually "server dev processes stopped on handoff (Q1)" bash -c "! kill -0 $SLEEP_PID"
SLEEP_PID=
check "server-seat terminal refused while the laptop writes" bash -c "test \"\$(curl -s -o /dev/null -w '%{http_code}' -b '$ROOT/jar' '$B/api/workspaces/$WS/terminal')\" = 409"
S0=$(seq_of)
echo "written on the laptop" > "$R/laptop.txt"
echo "v2 from laptop" > "$R/notes.txt"
eventually "laptop edits committed as a checkpoint by the device" bash -c "[ \$(curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS' | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"checkpoint_seq\"])') -gt $S0 ]"
eventually "server worktree follows the laptop (apply as the workspace user)" grep -q "written on the laptop" "$TREE/laptop.txt"
check "server worktree file owned by the workspace user" test "$(stat -c %U "$TREE/laptop.txt")" = "$OWNER"
git -C "$R" add laptop.txt notes.txt && git -C "$R" commit -qm "from the laptop" && git -C "$R" checkout -qb feature
echo "on feature" > "$R/feature.txt"
eventually "laptop branch pushed to repo.git under the lease" bash -c "runuser -u $OWNER -- git --git-dir='$DATA/workspaces/$WS/repo.git' rev-parse -q --verify refs/heads/feature"
eventually "server worktree HEAD follows the laptop's branch switch" bash -c "[ \"\$(runuser -u $OWNER -- env HOME=/tmp git -C '$TREE' symbolic-ref HEAD)\" = refs/heads/feature ] && grep -q 'on feature' '$TREE/feature.txt'"
check "laptop commit is in repo.git" bash -c "runuser -u $OWNER -- git --git-dir='$DATA/workspaces/$WS/repo.git' log --format=%s main | grep -q 'from the laptop'"
eventually "server and laptop trees identical" same_tree
git -C "$R" checkout -q main && git -C "$R" branch -q -D feature
eventually "branch deleted on the laptop lands as a trash ref on the server (M7.3)" bash -c "runuser -u $OWNER -- git --git-dir='$DATA/workspaces/$WS/repo.git' for-each-ref refs/armageddon/trash/ | grep -q delete/heads/feature"
echo "last laptop edit" > "$R/last.txt"
laptop work remote --restart >"$ROOT/work-remote.out" 2>&1 || { cat "$ROOT/work-remote.out"; fail "work remote"; }
check "work remote: the server seat holds the lease again (epoch 3)" test "$(holder) $(epoch)" = "server 3"
check "the laptop's last edit was flushed before the release" grep -q "last laptop edit" "$TREE/last.txt"
eventually "laptop agent released and follows" grep -q "released:" "$ROOT/agent.log"
as_ws "echo 'server again' > server2.txt"
settle
check "server edits reach the laptop after the handoff back" grep -q "server again" "$R/server2.txt"
check "--restart was recorded for the runtime layer" bash -c "curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS/events' | grep -q runtime.restart_requested"

step "2. F11: a Git operation in progress refuses a graceful handoff"
laptop work local >/dev/null 2>&1 || fail "work local"
git -C "$R" checkout -qb side && echo side > "$R/conflict.txt" && git -C "$R" add conflict.txt && git -C "$R" commit -qm side
git -C "$R" checkout -q main && echo main > "$R/conflict.txt" && git -C "$R" add conflict.txt && git -C "$R" commit -qm main
git -C "$R" merge -q side >/dev/null 2>&1 || true
check "a merge is in progress on the laptop" test -f "$R/.git/MERGE_HEAD"
check "work remote is refused with an explanation" bash -c "! (cd '$R' && '$BIN' work remote) >'$ROOT/refused.out' 2>&1 && grep -q 'merge is in progress' '$ROOT/refused.out'"
check "the laptop still holds the lease" test "$(holder)" = "device:$DEV"
git -C "$R" merge --abort
git -C "$R" branch -q -D side
laptop work remote >/dev/null 2>&1 || fail "work remote after the merge was aborted"
check "after aborting, the handoff succeeds" test "$(holder)" = server

step "3. F4: offline follower edits fast-forward on work local"
settle
agent_kill
echo "edited while the agent was down" > "$R/offline-follower.txt"
agent_up
eventually "the agent reports the replica DIRTY" bash -c "cd '$R' && '$BIN' status | grep -q DIRTY"
Q0=$(quarantine_count)
laptop work local >"$ROOT/ff.out" 2>&1 || { cat "$ROOT/ff.out"; fail "work local"; }
eventually "the edit becomes the laptop's first checkpoint (fast-forward)" grep -q "edited while the agent was down" "$TREE/offline-follower.txt"
check "nothing was quarantined" test "$(quarantine_count)" = "$Q0"
laptop work remote >/dev/null 2>&1 || fail "work remote"

step "4. F18: follower edits while the server seat writes are quarantined"
settle
echo "laptop edit without the lease" > "$R/f18.txt"
eventually "DIRTY reported" bash -c "cd '$R' && '$BIN' status | grep -q DIRTY"
as_ws "echo 'server moves on' >> notes.txt"
eventually "the laptop's edit is quarantined, not lost" quarantined_has "laptop edit without the lease"
settle
check "the laptop converged to the server" same_tree

step "5. F3: offline with the lease, checkpoints queue and commit in order"
laptop work local >/dev/null 2>&1 || fail "work local"
proxy_down
for i in 1 2 3; do echo "offline edit $i" > "$R/queued-$i.txt"; sleep 3; done
eventually "the laptop knows it is offline and keeps queueing" bash -c "cd '$R' && '$BIN' status 2>/dev/null | grep -q OFFLINE"
check "nothing reached the server while offline" test ! -e "$TREE/queued-1.txt"
proxy_up
eventually "on reconnect the queue is committed" grep -q "offline edit 3" "$TREE/queued-3.txt"
check "all queued edits arrived" bash -c "grep -q 'offline edit 1' '$TREE/queued-1.txt' && grep -q 'offline edit 2' '$TREE/queued-2.txt'"
check "queued checkpoints were committed in order (one per capture)" bash -c "curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS/checkpoints' | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(1 for c in d if c[\"author\"]==\"device\"))' | awk '{exit !(\$1>=3)}'"

step "6. F3: the lease is taken while the laptop is offline"
proxy_down
echo "written offline, after the takeover" > "$R/lost-tail.txt"
sleep 3
check "forced takeover without the password is refused" test "$(api_code POST "/api/workspaces/$WS/lease/force" '{"to":"server"}')" = 401
check "forced takeover with password re-auth" test "$(api_code POST "/api/workspaces/$WS/lease/force" "{\"to\":\"server\",\"password\":\"$ADMIN_PW\"}")" = 200
E=$(epoch)
check "the server seat holds a new epoch" test "$(holder)" = server
check "lease.forced audit event recorded" bash -c "curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS/events' | grep -q lease.forced"
as_ws "echo 'server after takeover' > after-takeover.txt"
proxy_up
eventually "the returning laptop's queue is quarantined (F3)" quarantined_has "written offline, after the takeover"
eventually "the agent reports the lost lease" grep -q "LEASE LOST" "$ROOT/agent.log"
settle
check "and the laptop follows the server again" grep -q "server after takeover" "$R/after-takeover.txt"
check "the takeover never put the lost tail in current" bash -c "! test -e '$TREE/lost-tail.txt'"

step "7. F2: the laptop dies holding the lease"
laptop work local >/dev/null 2>&1 || fail "work local"
echo "acknowledged before the crash" > "$R/acked.txt"
eventually "edit acknowledged" grep -q "acknowledged before the crash" "$TREE/acked.txt"
agent_kill
echo "never sent: the laptop died" > "$R/tail.txt"
eventually "the server shows the dead holder as STALE" bash -c "[ \"\$(curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS' | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"lease\"][\"state\"])')\" = stale ]"
check "a graceful work remote is refused as holder_unresponsive" bash -c "! (cd '$R' && '$BIN' work remote) >'$ROOT/stale.out' 2>&1 && grep -q 'not responding' '$ROOT/stale.out'"
check "forced takeover from the browser" test "$(api_code POST "/api/workspaces/$WS/lease/force" "{\"to\":\"server\",\"password\":\"$ADMIN_PW\"}")" = 200
check "the new holder continues from the last acknowledged checkpoint" grep -q "acknowledged before the crash" "$TREE/acked.txt"
as_ws "echo 'continued on the server' > continued.txt"
agent_up
eventually "the laptop's unsent tail reaches quarantine when it returns" quarantined_has "never sent: the laptop died"
settle
check "the returned laptop follows the new holder" grep -q "continued on the server" "$R/continued.txt"

step "8. quarantine CLI"
laptop quarantine list >"$ROOT/qlist.out"
check "list shows the quarantines with reasons" grep -q lease_lost "$ROOT/qlist.out"
QID=$(api GET "/api/workspaces/$WS/quarantines" | python3 -c '
import json, subprocess, sys
for q in json.load(sys.stdin):
    if subprocess.run(["git", "--git-dir=" + sys.argv[1], "cat-file", "-e", q["CheckpointID"] + ":worktree/tail.txt"]).returncode == 0:
        print(q["ID"]); break' "$DATA/workspaces/$WS/checkpoints.git")
[ -n "$QID" ] || fail "no quarantine holds tail.txt"
laptop quarantine diff "$QID" >"$ROOT/qdiff.out"
check "diff shows the quarantined change" grep -q "never sent: the laptop died" "$ROOT/qdiff.out"
laptop quarantine export "$QID" "$ROOT/exported" >/dev/null
check "export writes the quarantined tree" grep -q "never sent" "$ROOT/exported/tail.txt"
check "apply needs the lease" bash -c "! (cd '$R' && '$BIN' quarantine apply $QID) >/dev/null 2>&1"
laptop work local >/dev/null 2>&1 || fail "work local"
echo "changed here too" > "$R/acked.txt"
laptop quarantine apply "$QID" >"$ROOT/qapply.out" || { cat "$ROOT/qapply.out"; fail "apply"; }
check "apply restores the quarantined file" grep -q "never sent: the laptop died" "$R/tail.txt"
eventually "the applied result is checkpointed by the writer" grep -q "never sent: the laptop died" "$TREE/tail.txt"
N=$(quarantine_count)
laptop quarantine drop "$QID" >/dev/null
check "drop clears exactly that quarantine (nothing else evicted)" test "$(quarantine_count)" = $((N - 1))

step "9. revoking the holder is an immediate takeover"
api DELETE "/api/devices/$DEV" >/dev/null
check "the server seat holds the lease after revocation" test "$(holder)" = server

printf '\n\033[32mALL LOCAL WRITE MODE CHECKS PASSED\033[0m\n'
