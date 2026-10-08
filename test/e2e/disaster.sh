#!/usr/bin/env bash
# Disaster suite (plan M9.1, contract §10): the failure scenarios F1–F18
# against a real server as root (per-workspace users), a simulated laptop
# whose connection goes through a TCP proxy, fault injection in the server
# (ARMAGEDDON_FAULTS) and a data directory on a small tmpfs that the test
# fills up.
#
#   sudo test/e2e/disaster.sh [path/to/armageddon]
#
# Covered here: F1, F5, F6, F7, F8, F9 (b), F10, F12, F13, F14, F15, F17.
# Covered elsewhere (the nightly job runs all of them; see
# docs/disaster-suite.md): F2, F3, F4, F11, F18 in write.sh; F9 (a) in
# ops.sh; F16 in internal/lifecycle's rollback tests. In every scenario the
# invariant of §12 item 7 is checked: no acknowledged work is lost, and
# unacknowledged work is either delivered or kept in a quarantine.
set -euo pipefail

BIN=${1:-$(command -v armageddon)}
BIN=$(readlink -f "$BIN")
HERE=$(cd "$(dirname "$0")" && pwd)
PORT=${PORT:-8113}
PROXY_PORT=${PROXY_PORT:-$((PORT + 1))}
PORT2=${PORT2:-$((PORT + 2))}
B=http://127.0.0.1:$PORT
P=http://127.0.0.1:$PROXY_PORT
B2=http://127.0.0.1:$PORT2
ROOT=${E2E_ROOT:-/srv/armageddon-disaster-$$}
DATA=$ROOT/server
DATA2=$ROOT/server2
LAPTOP=$ROOT/laptop
R=$LAPTOP/replica
RUN=$ROOT/run
SOCK=$RUN/helper.sock
SOCK2=$RUN/helper2.sock
SERVER_USER=${SERVER_USER:-armageddon}
TMPFS_SIZE=${TMPFS_SIZE:-400m}
export ARMAGEDDON_CONFIG_DIR=$LAPTOP/config ARMAGEDDON_DATA_DIR=$LAPTOP/data
export GIT_AUTHOR_NAME=laptop GIT_AUTHOR_EMAIL=laptop@example.com GIT_COMMITTER_NAME=laptop GIT_COMMITTER_EMAIL=laptop@example.com
ADMIN_PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')

pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
skip() { printf '\033[33mSKIP\033[0m %s\n' "$*"; }
not() { ! "$@"; }
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
fail() {
  printf '\033[31mFAIL\033[0m %s\n' "$*"
  echo "--- agent log (tail)"; tail -40 "$ROOT/agent.log" 2>/dev/null || true
  echo "--- server log (tail)"; tail -30 "$ROOT/server.log" 2>/dev/null || true
  # The data directory is a tmpfs that cleanup unmounts: KEEP=1 copies it.
  [ -z "${KEEP:-}" ] || cp -a "$DATA" "$ROOT/server-kept" 2>/dev/null || true
  exit 1
}
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
SERVER_PID='' HELPER_PID='' PROXY_PID='' AGENT_PID='' SERVER2_PID='' HELPER2_PID=''
cleanup() {
  for p in $AGENT_PID $PROXY_PID $SERVER_PID $SERVER2_PID $HELPER_PID $HELPER2_PID; do kill -9 "$p" 2>/dev/null || true; done
  sleep 0.5
  umount "$DATA" 2>/dev/null || true
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root (the helper needs it; the server drops to $SERVER_USER)"
id "$SERVER_USER" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SERVER_USER" 2>/dev/null || id "$SERVER_USER" >/dev/null
rm -rf "$ROOT"; mkdir -p "$ROOT" "$LAPTOP" "$DATA" "$DATA2"; chmod 755 "$ROOT"
# The data directory on its own small filesystem, so F8 can fill it.
mount -t tmpfs -o "size=$TMPFS_SIZE,mode=0755" tmpfs "$DATA"
chown "$SERVER_USER:" "$DATA" "$DATA2"

as_server() { setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$DATA" "$@"; }
start_helper() { # start_helper DATA SOCK → HELPER pid in $!
  "$BIN" helper --data "$1" --socket "$2" --server-user "$SERVER_USER" >>"$ROOT/helper.log" 2>&1 &
}
wait_sock() { for _ in $(seq 50); do [ -S "$1" ] && return; sleep 0.1; done; fail "helper did not start"; }
# start_server: FAULTS=<points> start_server arms fault injection (F6, F7).
start_server() {
  setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$DATA" ARMAGEDDON_FAULTS="${FAULTS:-}" \
    "$BIN" server run --data "$DATA" --helper-socket "$SOCK" >>"$ROOT/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 50); do curl -sf "$B/healthz" >/dev/null && return; sleep 0.2; done
  fail "server did not start"
}
stop_server() { kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=; }
# server_crashed: the fault point killed the server (SIGKILL).
server_crashed() { ! kill -0 "$SERVER_PID" 2>/dev/null; }
reap_server() { wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=; }
proxy_up() { python3 "$HERE/proxy.py" "$PROXY_PORT" "$PORT" ${1:+"$1"} >"$ROOT/proxy.log" 2>&1 & PROXY_PID=$!; sleep 0.3; }
proxy_down() { kill -9 "$PROXY_PID" 2>/dev/null || true; wait "$PROXY_PID" 2>/dev/null || true; PROXY_PID=; }
agent_up() { ${AGENT_ENV:-} "$BIN" agent run >>"$ROOT/agent.log" 2>&1 & AGENT_PID=$!; sleep 1; }
agent_kill() { kill -9 "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; AGENT_PID=; }
api() { # api METHOD PATH [JSON]
  curl -sf -b "$ROOT/jar" -c "$ROOT/jar" -X "$1" "$B$2" -H "X-CSRF-Token: $(cat "$ROOT/csrf" 2>/dev/null)" \
    -H 'Content-Type: application/json' ${3:+-d "$3"}
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
ws_json() { api GET "/api/workspaces/$WS" | json "$1"; }
holder() { ws_json 'd["lease"]["holder"]'; }
wstate() { ws_json 'd["state"]'; }
seq_of() { ws_json 'd["checkpoint_seq"]'; }
cur_of() { ws_json 'd["current_checkpoint"]'; }
as_ws() { # run a command in the server worktree as the workspace user
  runuser -u "$OWNER" -- env HOME="$DATA/workspaces/$WS/home" bash -c "cd '$TREE' && $*"
}
repo_git() { runuser -u "$OWNER" -- env HOME=/tmp git --git-dir="$DATA/workspaces/$WS/repo.git" "$@"; }
cps_git() { git --git-dir="$DATA/workspaces/$WS/checkpoints.git" "$@"; }
eventually() { # eventually DESC CMD...: retry for up to 30 s
  local d=$1; shift
  for _ in $(seq 60); do if "$@" >/dev/null 2>&1; then pass "$d"; return; fi; sleep 0.5; done
  fail "$d"
}
settle() { # capture the server seat now and wait until the laptop applied it
  api POST "/api/workspaces/$WS/sync" '' >/dev/null
  local s; s=$(seq_of)
  for _ in $(seq 60); do grep -qE "(applied|committed) checkpoint #$s\$" "$ROOT/agent.log" && return; sleep 0.5; done
  fail "laptop did not apply checkpoint #$s"
}
quarantines() { api GET "/api/workspaces/$WS/quarantines"; }
quarantine_count() { quarantines | json 'len(d)'; }
quarantined_has() { # quarantined_has SOURCE TEXT: a quarantine from SOURCE has a file containing TEXT
  local q
  for q in $(quarantines | json '" ".join(x["CheckpointID"] for x in d if x["SourceKind"] == "'"$1"'")'); do
    cps_git grep -q -F "$2" "$q:worktree" -- 2>/dev/null && return 0
  done
  return 1
}
in_cp() { cps_git cat-file -e "$(cur_of):worktree/$1" 2>/dev/null; }
cp_has() { cps_git grep -q -F "$2" "$(cur_of):worktree" -- "$1" 2>/dev/null; }
laptop() { (cd "$R" && "$BIN" "$@"); }
# acked_invariant: every checkpoint the database lists exists in
# checkpoints.git, and refs/checkpoints/current is the database's current.
acked_invariant() {
  local id
  for id in $(api GET "/api/workspaces/$WS/checkpoints" | json '" ".join(c["id"] for c in d)'); do
    cps_git cat-file -e "$id^{commit}" || return 1
  done
  [ "$(cps_git rev-parse refs/checkpoints/current)" = "$(cur_of)" ]
}
set_cfg() { # set_cfg KEY JSON-VALUE (server stopped)
  python3 - "$DATA/server.json" "$1" "$2" <<'EOF'
import json, sys
p, k, v = sys.argv[1], sys.argv[2], json.loads(sys.argv[3])
c = json.load(open(p)); c[k] = v; json.dump(c, open(p, "w"))
EOF
}
pair() { # pair URL JAR CSRF → device paired with the laptop's config
  "$BIN" login "$1" --name test-laptop >"$ROOT/login.out" 2>&1 &
  local pid=$! code=
  for _ in $(seq 50); do code=$(grep -o 'code=[A-Z-]*' "$ROOT/login.out" 2>/dev/null | head -1 | cut -d= -f2 || true); [ -n "$code" ] && break; sleep 0.2; done
  curl -sf -b "$2" -X POST "${4:-$1}/api/pair/approve" -H "X-CSRF-Token: $(cat "$3")" -H 'Content-Type: application/json' -d "{\"code\":\"$code\"}" >/dev/null
  wait $pid
}

step "0. server (data directory on a $TMPFS_SIZE tmpfs), admin, workspace, laptop"
as_server "$BIN" server init --data "$DATA" --listen "127.0.0.1:$PORT" --public-url "$P" >/dev/null
set_cfg lease_stale_ms 6000
set_cfg handoff_timeout_ms 10000
set_cfg capture_interval_ms 1000
set_cfg fsck_interval_s 3
start_helper "$DATA" "$SOCK"; HELPER_PID=$!; wait_sock "$SOCK"
start_server
proxy_up
TOKEN=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1 | cut -d= -f2)
curl -sf -c "$ROOT/jar" -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"abdul\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/csrf"
WS=$(api POST /api/workspaces '{"name":"disaster"}' | json 'd["id"]')
for _ in $(seq 60); do [ "$(wstate)" = ready ] && break; sleep 0.5; done
TREE=$DATA/workspaces/$WS/tree
OWNER=$(stat -c %U "$TREE")
as_ws "mkdir -p src .armageddon && echo 'package main' > src/main.go && echo v1 > notes.txt && echo 'SECRET=1' > .env && echo '.env' > .gitignore \
  && printf 'sync:\n  max_file_size: 1MB\n' > .armageddon/sync.yaml && git add -A && git commit -qm init"
pair "$P" "$ROOT/jar" "$ROOT/csrf" "$B"
DEV=$(api GET /api/devices | json 'd[0]["id"]')
api POST "/api/workspaces/$WS/sync" '' >/dev/null
(cd "$LAPTOP" && "$BIN" clone disaster replica >/dev/null)
agent_up
check "replica cloned, .env included" grep -q SECRET=1 "$R/.env"

step "F1. the laptop dies in the default server-first workflow"
S0=$(seq_of)
agent_kill
as_ws "echo 'server keeps working' > f1.txt"
api POST "/api/workspaces/$WS/sync" '' >/dev/null
check "the server seat holds the workspace; it never moved to the laptop" test "$(holder)" = server
check "work continues on the server and is checkpointed" bash -c "[ $(seq_of) -gt $S0 ] && grep -q 'server keeps working' '$TREE/f1.txt'"
agent_up
settle
check "the replaced laptop catches up" grep -q "server keeps working" "$R/f1.txt"

step "F17. a file over max_file_size is not synced, and is reported"
as_ws "head -c 2097152 /dev/urandom > ~/big.tmp && mv ~/big.tmp big.bin && echo small > small.txt" # whole, not caught half-written
settle
check "the small file is in the checkpoint" in_cp small.txt
check "the 2 MB file (limit 1MB from .armageddon/sync.yaml) is not" not in_cp big.bin
check "checkpoint meta lists it as excluded.oversize" bash -c "git --git-dir='$DATA/workspaces/$WS/checkpoints.git' cat-file blob '$(cur_of):meta.json' | grep -q '\"oversize\":\\[\"big.bin\"\\]'"
check "the laptop never received it" test ! -e "$R/big.bin"
laptop status >"$ROOT/status.out" 2>&1 || true
check "armageddon status lists it as not synced" grep -q "not synced: .*big.bin" "$ROOT/status.out"
as_ws "rm big.bin"

step "F13. git gc --prune=now on the server seat; the trash keeps deleted branches"
as_ws "git checkout -qb doomed && echo 'only on doomed' > doomed.txt && git add doomed.txt && git commit -qm 'doomed work' && git checkout -q main && git branch -q -D doomed"
DOOMED=$(repo_git for-each-ref --format='%(objectname)' 'refs/armageddon/trash/' | tail -1)
check "the deleted branch went to the trash" test -n "$DOOMED"
as_ws "git reflog expire --expire=now --all && git gc -q --prune=now"
check "the trashed commit survives gc --prune=now" repo_git cat-file -e "$DOOMED^{commit}"
as_ws "'$BIN' git trash list" >"$ROOT/trash.out"
check "armageddon git trash lists it" grep -q "refs/heads/doomed" "$ROOT/trash.out"
as_ws "'$BIN' git trash restore 1" >"$ROOT/restore.out"
check "restore brings the branch back at the same commit" test "$(repo_git rev-parse refs/heads/doomed)" = "$DOOMED"
check "restore never overwrites an existing branch" bash -c "! runuser -u '$OWNER' -- env HOME='$DATA/workspaces/$WS/home' bash -c \"cd '$TREE' && '$BIN' git trash restore 1\" >/dev/null 2>&1"
settle

step "F14. a server-side process edits tracked files while the laptop holds the lease"
laptop work local >/dev/null 2>&1 || fail "work local"
as_ws "echo 'edited by a server process' > notes.txt"
echo "laptop version" > "$R/notes.txt"
eventually "the laptop's edit wins on the server" grep -q "laptop version" "$TREE/notes.txt"
eventually "the server's edit is kept as a server_drift quarantine" quarantined_has server "edited by a server process"
check "acknowledged checkpoints intact" acked_invariant

step "F5. the network drops in the middle of a checkpoint upload"
proxy_down
proxy_up 1000000 # 1 MB/s uploads
mkdir -p "$R/upload" # 5 × 700 KB (under the 1 MB limit): about 3 s on the wire
for i in 1 2 3 4 5; do head -c 520000 /dev/urandom | base64 > "$R/upload/part$i.txt"; done
eventually "the upload has started (throttled to 1 MB/s)" grep -q "upload in progress" "$ROOT/proxy.log"
proxy_down # cut mid-upload
sleep 2
proxy_up
eventually "the upload is retried and every file arrives intact" bash -c "for i in 1 2 3 4 5; do cmp -s '$R/upload/part'\$i.txt '$TREE/upload/part'\$i.txt || exit 1; done"
check "acknowledged checkpoints intact, refs agree with the database" acked_invariant

step "F6 (a). the server crashes after staging a checkpoint, before acknowledging it"
stop_server
FAULTS=commit-before-db start_server
Q0=$(quarantine_count)
echo "f6a" > "$R/f6a.txt"
eventually "the fault point crashed the server mid-commit" server_crashed
reap_server
check "nothing was acknowledged for it" bash -c "! grep -q f6a '$TREE/f6a.txt' 2>/dev/null"
start_server
eventually "after restart the laptop retries and the checkpoint commits" grep -q f6a "$TREE/f6a.txt"
check "no quarantine was needed" test "$(quarantine_count)" = "$Q0"
check "acknowledged checkpoints intact" acked_invariant

step "F6 (b). the server crashes after committing, before updating refs and replying"
stop_server
FAULTS=commit-after-db start_server
echo "f6b" > "$R/f6b.txt"
eventually "the fault point crashed the server after the database commit" server_crashed
reap_server
start_server
check "startup reconcile rebuilt refs/checkpoints/current from the database" acked_invariant
eventually "the laptop's retry is idempotent: the same checkpoint, acknowledged" grep -q f6b "$TREE/f6b.txt"
check "still no quarantine (the retry matched, F5/F6)" test "$(quarantine_count)" = "$Q0"
eventually "the laptop's queue drained" bash -c "cd '$R' && '$BIN' status | grep -q ' 0 checkpoint(s) queued'"

step "F7. the server crashes in the middle of applying a checkpoint to its worktree"
stop_server
FAULTS=seat-apply start_server
echo "f7 from the laptop" > "$R/f7.txt"
eventually "the fault point crashed the server before its worktree followed" server_crashed
reap_server
check "the checkpoint itself was committed (server down: read checkpoints.git)" bash -c "git --git-dir='$DATA/workspaces/$WS/checkpoints.git' grep -q -F 'f7 from the laptop' refs/checkpoints/current:worktree -- f7.txt"
# What a crash in the middle of the apply leaves: some files written, others not.
as_ws "echo 'half-writ' > f7.txt"
start_server
eventually "after restart the worktree reaches current" grep -q "f7 from the laptop" "$TREE/f7.txt"
check "the half-applied file was kept as a quarantine, not overwritten silently" quarantined_has server "half-writ"
check "acknowledged checkpoints intact" acked_invariant

step "F8. the server's disk fills up"
FREE=$(df --output=avail -B1 "$DATA" | tail -1)
SIZE=$(df --output=size -B1 "$DATA" | tail -1)
fallocate -l $((FREE - SIZE * 3 / 100)) "$DATA/ballast"
check "the disk is below the 5% floor" bash -c "[ \$(df --output=avail -B1 '$DATA' | tail -1) -lt \$(( $SIZE * 5 / 100 )) ]"
echo "written while the disk was full" > "$R/f8.txt"
eventually "the workspace goes DEGRADED (disk_full)" bash -c "[ \"\$(curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS' | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d[\"state\"], d[\"state_reason\"])')\" = 'degraded disk_full' ]"
sleep 3
check "the commit was refused, not stored" bash -c "! grep -q 'written while the disk was full' '$TREE/f8.txt' 2>/dev/null"
check "the agent was told disk_full and keeps the checkpoint queued" bash -c "grep -q 'almost full' '$ROOT/agent.log' && cd '$R' && '$BIN' status | grep -qE ' [1-9][0-9]* checkpoint\(s\) queued'"
check "reads still work: the laptop can fetch" bash -c "cd '$R' && git fetch -q armageddon"
as_server "$BIN" doctor --data "$DATA" --helper-socket "$SOCK" >"$ROOT/doctor-full.out" 2>&1 || true
check "doctor reports the disk and the degraded workspace" bash -c "grep -q 'disk_full' '$ROOT/doctor-full.out' && grep -q '\\[FAIL\\].*disk' '$ROOT/doctor-full.out'"
rm -f "$DATA/ballast"
as_server "$BIN" doctor --data "$DATA" --helper-socket "$SOCK" --repair >"$ROOT/doctor-repair.out" 2>&1 || true
check "doctor --repair returns it to READY once space is free" bash -c "grep -q 'returned to READY' '$ROOT/doctor-repair.out' && [ \"\$(curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS' | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"state\"])')\" = ready ]"
eventually "the queued checkpoint is committed: nothing was lost" grep -q "written while the disk was full" "$TREE/f8.txt"
check "acknowledged checkpoints intact" acked_invariant
laptop work remote >/dev/null 2>&1 || fail "work remote"
settle

step "F10. objects lost from repo.git; repaired from the laptop's replica"
as_ws "echo f10 > f10.txt && git add f10.txt && git commit -qm 'f10 commit'"
settle
HEADC=$(repo_git rev-parse HEAD)
OBJ=$DATA/workspaces/$WS/repo.git/objects/${HEADC:0:2}/${HEADC:2}
[ -f "$OBJ" ] || fail "expected $HEADC to be a loose object"
eventually "the laptop has the commit" bash -c "git -C '$R' cat-file -e $HEADC"
rm -f "$OBJ"
eventually "scheduled fsck marks the workspace DEGRADED (repo_corrupt)" bash -c "curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS' | grep -q '\"state_reason\":\"repo_corrupt'"
check "writes are refused while DEGRADED (a push from the laptop)" bash -c "cd '$R' && ! git push -q armageddon HEAD:refs/heads/f10-try 2>/dev/null"
laptop workspace repair --from-device >"$ROOT/repair.out" 2>&1 || { cat "$ROOT/repair.out"; fail "workspace repair --from-device"; }
check "repair reports a clean fsck" grep -q "fsck is clean" "$ROOT/repair.out"
check "the lost commit is back" repo_git cat-file -e "$HEADC^{commit}"
check "the workspace is READY again" test "$(wstate)" = ready
as_ws "echo after-repair > f10b.txt"
settle
check "and writes again" grep -q after-repair "$R/f10b.txt"

step "F15. clock skew between machines"
if command -v faketime >/dev/null 2>&1; then
  agent_kill
  AGENT_ENV="faketime -f +3h" agent_up
  laptop work local >/dev/null 2>&1 || fail "work local with a skewed clock"
  echo "written 3 hours in the future" > "$R/f15.txt"
  eventually "a laptop 3 hours ahead still writes (ordering never uses clocks)" grep -q "3 hours in the future" "$TREE/f15.txt"
  (cd "$R" && faketime -f +3h "$BIN" status) >"$ROOT/skew.out" 2>&1 || true
  check "status warns about the skew" grep -q "clock:.*WARNING.*ahead of" "$ROOT/skew.out"
  laptop work remote >/dev/null 2>&1 || fail "work remote"
  agent_kill
  AGENT_ENV='' agent_up
  check "acknowledged checkpoints intact" acked_invariant
else
  skip "faketime is not installed; the skewed-laptop check runs in CI (apt-get install faketime)"
fi
as_server "$BIN" doctor --data "$DATA" --helper-socket "$SOCK" >"$ROOT/doctor-clock.out" 2>&1 || true
check "doctor checks the server's clock" grep -q "clock" "$ROOT/doctor-clock.out"

step "F12. the laptop is stolen: revoke it"
settle
laptop work local >/dev/null 2>&1 || fail "work local"
api DELETE "/api/devices/$DEV" >/dev/null
check "revocation force-releases the lease to the server seat" test "$(holder)" = server
check "the device's tokens are dead" bash -c "! (cd '$R' && '$BIN' workspaces) >/dev/null 2>&1"
check "Git over HTTP refuses it" bash -c "cd '$R' && ! git fetch -q armageddon 2>/dev/null"
check "lease.forced audit trail recorded" bash -c "curl -sf -b '$ROOT/jar' '$B/api/workspaces/$WS/events' | grep -q 'device.revoked\|lease.forced\|lease.revoked'"
agent_kill

step "F9 (b). the server is lost; a new server is rebuilt from the replica"
echo "uncommitted on the laptop" > "$R/uncommitted.txt"
git -C "$R" checkout -qb laptop-branch && git -C "$R" checkout -q main
stop_server
proxy_down
mkdir -p "$DATA2"; chown "$SERVER_USER:" "$DATA2"
setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$DATA2" \
  "$BIN" server init --data "$DATA2" --listen "127.0.0.1:$PORT2" --public-url "$B2" >/dev/null
start_helper "$DATA2" "$SOCK2"; HELPER2_PID=$!; wait_sock "$SOCK2"
setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$DATA2" \
  "$BIN" server run --data "$DATA2" --helper-socket "$SOCK2" >>"$ROOT/server2.log" 2>&1 &
SERVER2_PID=$!
for _ in $(seq 50); do curl -sf "$B2/healthz" >/dev/null && break; sleep 0.2; done
TOKEN2=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server2.log" | head -1 | cut -d= -f2)
curl -sf -c "$ROOT/jar2" -X POST "$B2/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN2\",\"username\":\"abdul\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/csrf2"
pair "$B2" "$ROOT/jar2" "$ROOT/csrf2"
laptop workspace seed --from-replica --name rebuilt >"$ROOT/seed.out" 2>&1 || { cat "$ROOT/seed.out"; tail -20 "$ROOT/server2.log"; fail "workspace seed --from-replica"; }
check "seed reports history and working state" grep -q "history and working state" "$ROOT/seed.out"
WS2=$(curl -sf -b "$ROOT/jar2" "$B2/api/workspaces" | json '[w["id"] for w in d if w["name"] == "rebuilt"][0]')
TREE2=$DATA2/workspaces/$WS2/tree
check "the new workspace has the laptop's uncommitted file" grep -q "uncommitted on the laptop" "$TREE2/uncommitted.txt"
check "and its .env" grep -q SECRET=1 "$TREE2/.env"
check "and every committed file" bash -c "grep -q f10 '$TREE2/f10.txt' && grep -q 'f7 from the laptop' '$TREE2/f7.txt'"
OWNER2=$(stat -c %U "$TREE2")
check "and every branch" runuser -u "$OWNER2" -- git --git-dir="$DATA2/workspaces/$WS2/repo.git" rev-parse -q --verify refs/heads/laptop-branch
check "with the same history" test "$(runuser -u "$OWNER2" -- git --git-dir="$DATA2/workspaces/$WS2/repo.git" rev-parse main)" = "$(git -C "$R" rev-parse main)"
check "the rebuilt workspace's repositories pass fsck" runuser -u "$OWNER2" -- git --git-dir="$DATA2/workspaces/$WS2/repo.git" fsck --no-progress --connectivity-only

printf '\n\033[32mALL DISASTER SUITE CHECKS PASSED\033[0m\n'
