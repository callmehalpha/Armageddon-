#!/usr/bin/env bash
# Operations acceptance test (plan M5.3, M5.5, M5.6). Runs a real server as
# root over IP-only TLS, then:
#   1. server init --ip-only prints a fingerprint; login pins it (TOFU),
#      a wrong --fingerprint is refused, git clones through the pin
#   2. doctor passes on a live server
#   3. backup → fresh data directory → restore: identical refs, identical
#      current checkpoint, identical worktree; READY with the server holding
#      the lease at a new epoch; no new checkpoint after restart
#   4. doctor flags a broken checkpoint ref and --repair fixes it
#   5. a replaced server certificate is refused by the paired device
#
#   sudo PORT=8104 test/e2e/ops.sh [path/to/armageddon]
set -euo pipefail

BIN=${1:-$(command -v armageddon)}
BIN=$(readlink -f "$BIN")
PORT=${PORT:-8104}
B=https://127.0.0.1:$PORT
ROOT=${E2E_ROOT:-/srv/armageddon-ops-$$}
DATA=$ROOT/server
DATA2=$ROOT/restored
LAPTOP=$ROOT/laptop
SERVER_USER=${SERVER_USER:-armageddon}
export ARMAGEDDON_CONFIG_DIR=$LAPTOP/config ARMAGEDDON_DATA_DIR=$LAPTOP/data
ADMIN_PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
export ARMAGEDDON_BACKUP_PASSPHRASE
ARMAGEDDON_BACKUP_PASSPHRASE=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')

pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; [ -f "$ROOT/server.log" ] && tail -20 "$ROOT/server.log"; exit 1; }
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
SERVER_PID='' HELPER_PIDS=''
cleanup() {
  if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; fi
  for p in $HELPER_PIDS; do kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root (the helper needs it; the server drops to $SERVER_USER)"
id "$SERVER_USER" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SERVER_USER" 2>/dev/null || id "$SERVER_USER" >/dev/null
rm -rf "$ROOT"; mkdir -p "$ROOT" "$LAPTOP" "$DATA" "$DATA2" "$ROOT/backups"; chmod 755 "$ROOT"
chown "$SERVER_USER:" "$DATA" "$DATA2" "$ROOT/backups"

# Production layout (M3.1): one helper (root) per data directory, and every
# server command (init, run, backup, restore, doctor) as $SERVER_USER.
sock_of() { echo "$ROOT/run-$(basename "$1")/helper.sock"; }
as_server() { setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$ROOT" ARMAGEDDON_BACKUP_PASSPHRASE="$ARMAGEDDON_BACKUP_PASSPHRASE" "$@"; }
start_helper() { # start_helper DATA_DIR
  local sock; sock=$(sock_of "$1")
  "$BIN" helper --data "$1" --socket "$sock" --server-user "$SERVER_USER" >>"$ROOT/helper.log" 2>&1 &
  HELPER_PIDS="$HELPER_PIDS $!"
  for _ in $(seq 50); do [ -S "$sock" ] && return; sleep 0.1; done
  fail "helper did not start"
}
start_server() { # start_server DATA_DIR
  setpriv --reuid="$SERVER_USER" --regid="$SERVER_USER" --clear-groups -- env HOME="$ROOT" \
    "$BIN" server run --data "$1" --helper-socket "$(sock_of "$1")" >>"$ROOT/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 50); do curl -sf --cacert "$1/keys/tls-selfsigned.crt" "$B/healthz" >/dev/null && return; sleep 0.2; done
  fail "server did not start"
}
stop_server() { kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=; }
CA=$DATA/keys/tls-selfsigned.crt
api() { # api METHOD PATH [JSON]
  curl -sf --cacert "$CA" -b "$ROOT/jar" -c "$ROOT/jar" -X "$1" "$B$2" -H "X-CSRF-Token: $(cat "$ROOT/csrf" 2>/dev/null)" \
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
cur_of() { api GET "/api/workspaces/$1/current" | json 'd["id"]'; }
as_ws() { # as_ws DATA ID CMD
  local tree=$1/workspaces/$2/tree user; user=$(stat -c %U "$tree")
  runuser -u "$user" -- env HOME="$1/workspaces/$2/home" bash -c "cd '$tree' && $3"
}
repo_refs() { # repo_refs DATA ID
  local d=$1/workspaces/$2; runuser -u "$(stat -c %U "$d/repo.git")" -- env HOME=/tmp \
    git --git-dir="$d/repo.git" for-each-ref --format='%(objectname) %(refname)'
}
cp_refs() { git --git-dir="$1/workspaces/$2/checkpoints.git" for-each-ref --format='%(objectname) %(refname)' refs/checkpoints/; }
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
db() { python3 -c "import sqlite3,sys; c=sqlite3.connect('file:'+sys.argv[1]+'?mode=ro', uri=True); print('\n'.join('|'.join(map(str,r)) for r in c.execute(sys.argv[2])))" "$1/armageddon.db" "$2"; }

step "1. IP-only TLS, first admin, trust on first use"
as_server "$BIN" server init --data "$DATA" --ip-only --ip 127.0.0.1 --listen "127.0.0.1:$PORT" >"$ROOT/init.out"
FP=$(grep -oE '([0-9A-F]{2}:){31}[0-9A-F]{2}' "$ROOT/init.out" | head -1)
check "server init --ip-only printed the certificate fingerprint" test -n "$FP"
check "fingerprint matches the certificate (openssl)" bash -c "openssl x509 -in '$CA' -noout -fingerprint -sha256 | grep -q '$FP'"
check "server fingerprint prints the same" test "$("$BIN" server fingerprint --data "$DATA")" = "$FP"
start_helper "$DATA"
start_server "$DATA"
check "plain-HTTP request to the TLS port is refused" bash -c "! curl -sf http://127.0.0.1:$PORT/healthz >/dev/null 2>&1"
TOKEN=$(grep -o 'setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1 | cut -d= -f2)
curl -sf --cacert "$CA" -c "$ROOT/jar" -X POST "$B/api/setup" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"username\":\"abdul\",\"password\":\"$ADMIN_PW\"}" | json 'd["csrf"]' >"$ROOT/csrf"
pass "admin created over TLS"
if [ "${FP:0:2}" = 00 ]; then WRONG="11${FP:2}"; else WRONG="00${FP:2}"; fi
check "login with a wrong --fingerprint is refused" bash -c "! '$BIN' login '$B' --name x --fingerprint '$WRONG' >'$ROOT/wrong.out' 2>&1 && grep -q 'certificate has changed' '$ROOT/wrong.out'"
"$BIN" login "$B" --name ops-laptop --fingerprint "$FP" >"$ROOT/login.out" 2>&1 &
LOGIN_PID=$!
CODE=
for _ in $(seq 50); do CODE=$(grep -o 'code=[A-Z-]*' "$ROOT/login.out" 2>/dev/null | head -1 | cut -d= -f2 || true); [ -n "$CODE" ] && break; sleep 0.2; done
[ -n "$CODE" ] || { cat "$ROOT/login.out"; fail "login printed no code"; }
check "login showed the fingerprint for comparison" grep -q "$FP" "$ROOT/login.out"
api POST /api/pair/approve "{\"code\":\"$CODE\"}" >/dev/null
wait $LOGIN_PID && pass "device paired over the pinned certificate"

step "2. workspace with history, uncommitted work, a deleted branch"
WS=$(api POST /api/workspaces '{"name":"ops"}' | json 'd["id"]')
wait_ready "$WS"
as_ws "$DATA" "$WS" "echo 'package main' > main.go && git add main.go && git commit -qm main && git checkout -q -b feature && echo f > f.txt && git add f.txt && git commit -qm feature && git branch -q doomed && git branch -q -D doomed && echo staged > s.txt && git add s.txt && echo more >> s.txt && echo 'wip' > wip.txt && echo 'K=v' > .env"
api POST "/api/workspaces/$WS/sync" '' >/dev/null
SEQ=$(seq_of "$WS"); CUR=$(cur_of "$WS")
check "checkpoints recorded (#$SEQ)" test "$SEQ" -ge 2
(cd "$LAPTOP" && "$BIN" clone ops replica >/dev/null 2>"$ROOT/clone.err") || { cat "$ROOT/clone.err"; fail "clone"; }
check "git clone through the pinned self-signed certificate works" test -f "$LAPTOP/replica/wip.txt"
as_server "$BIN" doctor --data "$DATA" --helper-socket "$(sock_of "$DATA")" >"$ROOT/doctor1.out" 2>&1 || { cat "$ROOT/doctor1.out"; fail "doctor on a healthy live server"; }
pass "doctor passes on the live server"

step "3. backup → fresh data directory → restore"
as_server "$BIN" server backup --data "$DATA" --helper-socket "$(sock_of "$DATA")" --to "$ROOT/backups" >"$ROOT/backup.out" 2>&1 || { cat "$ROOT/backup.out"; fail backup; }
BK=$(sed -n 's/^Backup written to //p' "$ROOT/backup.out")
check "backup written with encrypted keys" test -f "$BK/keys.tar.enc"
WANT_REPO=$(repo_refs "$DATA" "$WS"); WANT_CP=$(cp_refs "$DATA" "$WS"); tree_sum "$DATA/workspaces/$WS/tree" >"$ROOT/want.sum"
check "a deleted branch's trash ref is in the backed-up refs" bash -c "grep -q 'refs/armageddon/trash/.*doomed' <<<'$WANT_REPO'"
stop_server
start_helper "$DATA2"
as_server "$BIN" server restore "$BK" --data "$DATA2" --helper-socket "$(sock_of "$DATA2")" >"$ROOT/restore.out" 2>&1 || { cat "$ROOT/restore.out"; fail restore; }
pass "restore finished: $(grep -c 'restored at checkpoint' "$ROOT/restore.out") workspace(s)"
check "restored repo.git refs are identical (incl. trash refs)" test "$(repo_refs "$DATA2" "$WS")" = "$WANT_REPO"
check "restored checkpoint refs are identical" test "$(cp_refs "$DATA2" "$WS")" = "$WANT_CP"
check "lease held by the server at a new epoch" test "$(db "$DATA2" "SELECT holder_kind, epoch FROM leases WHERE workspace_id='$WS'")" = "server|2"
CA=$DATA2/keys/tls-selfsigned.crt
check "keys restored: same certificate fingerprint" test "$("$BIN" server fingerprint --data "$DATA2")" = "$FP"
start_server "$DATA2"
check "workspace is READY after restore" test "$(api GET "/api/workspaces/$WS" | json 'd["state"]')" = ready
check "current checkpoint identical (#$SEQ $CUR)" test "$(seq_of "$WS") $(cur_of "$WS")" = "$SEQ $CUR"
tree_sum "$DATA2/workspaces/$WS/tree" >"$ROOT/got.sum"
check "restored worktree is byte-identical (uncommitted, staged, .env)" diff -u "$ROOT/want.sum" "$ROOT/got.sum"
check "restored index (staged state) identical" test "$(as_ws "$DATA2" "$WS" 'git show :s.txt')" = staged
sleep 3
check "no new checkpoint after restart (the seat matches current exactly)" test "$(seq_of "$WS")" = "$SEQ"
check "the paired device still works against the restored server" bash -c "'$BIN' workspaces | grep -q ops"

step "4. doctor on the restored server; broken ref flagged and repaired"
as_server "$BIN" doctor --data "$DATA2" --helper-socket "$(sock_of "$DATA2")" >"$ROOT/doctor2.out" 2>&1 || { cat "$ROOT/doctor2.out"; fail "doctor after restore"; }
pass "doctor passes after restore"
git --git-dir="$DATA2/workspaces/$WS/checkpoints.git" update-ref refs/checkpoints/current "$(git --git-dir="$DATA2/workspaces/$WS/checkpoints.git" rev-parse refs/checkpoints/1)"
check "doctor flags refs/checkpoints/current ≠ database" bash -c "! setpriv --reuid=$SERVER_USER --regid=$SERVER_USER --clear-groups -- '$BIN' doctor --data '$DATA2' --helper-socket '$(sock_of "$DATA2")' >'$ROOT/doctor3.out' 2>&1 && grep -q 'doctor --repair' '$ROOT/doctor3.out'"
as_server "$BIN" doctor --data "$DATA2" --helper-socket "$(sock_of "$DATA2")" --repair >/dev/null 2>&1 || true
check "doctor --repair rewrote the ref from the database" test "$(git --git-dir="$DATA2/workspaces/$WS/checkpoints.git" rev-parse refs/checkpoints/current)" = "$CUR"

step "5. a replaced certificate is refused"
stop_server
as_server "$BIN" server init --data "$DATA2" --ip-only --ip 127.0.0.1 --listen "127.0.0.1:$PORT" --new-cert >/dev/null
start_server "$DATA2"
check "the device refuses the changed certificate with a clear error" bash -c "! '$BIN' workspaces >'$ROOT/changed.out' 2>&1 && grep -q 'certificate has changed' '$ROOT/changed.out'"

printf '\n\033[32mALL OPS CHECKS PASSED\033[0m\n'
