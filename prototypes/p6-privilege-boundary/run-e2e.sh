#!/usr/bin/env bash
# P6 functional end-to-end run (throwaway). Root helper + unprivileged server
# half; checks that PTY, git-service and capture work through the helper and
# run as the workspace user, then times helper spawns against direct setuid.
#
#   sudo prototypes/p6-privilege-boundary/run-e2e.sh [iterations]
set -euo pipefail
N=${1:-100}
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=/var/lib/p6-e2e-$$
SOCKDIR=/run/p6-$$
BIN=/usr/local/bin/p6-proto-$$
SRV=p6srv$$
WS=p6ws$(printf '%05d' $(( $$ % 100000 )))e2e
HELPER_PID=

pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; exit 1; }
cleanup() {
  [ -n "$HELPER_PID" ] && kill "$HELPER_PID" 2>/dev/null || true
  userdel "ws-$WS" 2>/dev/null || true
  userdel "$SRV" 2>/dev/null || true
  rm -rf "$ROOT" "$SOCKDIR" "$BIN"
}
trap cleanup EXIT
[ "$(id -u)" = 0 ] || fail "run as root"

(cd "$HERE/.." && go build -o "$BIN" ./p6-privilege-boundary)
chmod 0755 "$BIN"
useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SRV"
SRV_UID=$(id -u "$SRV")
mkdir -p "$ROOT/workspaces" "$ROOT/srv" "$SOCKDIR"
chmod 755 "$ROOT" "$ROOT/workspaces" "$SOCKDIR"
chown "$SRV:" "$ROOT/srv" "$SOCKDIR"; chmod 700 "$ROOT/srv"

"$BIN" helper -socket "$SOCKDIR/helper.sock" -server-uid "$SRV_UID" -base "$ROOT/workspaces" 2>"$ROOT/helper.log" &
HELPER_PID=$!
for _ in $(seq 50); do [ -S "$SOCKDIR/helper.sock" ] && break; sleep 0.1; done
[ -S "$SOCKDIR/helper.sock" ] || fail "helper did not start"
[ "$(stat -c '%a %U' "$SOCKDIR/helper.sock")" = "600 $SRV" ] && pass "helper socket is mode 0600, owned by the server user" || fail "socket mode/owner: $(stat -c '%a %U' "$SOCKDIR/helper.sock")"

echo "== server half as $SRV (uid $SRV_UID)"
runuser -u "$SRV" -- "$BIN" demo -socket "$SOCKDIR/helper.sock" -base "$ROOT/workspaces" -srv "$ROOT/srv" -ws "$WS" -n "$N"
echo "== baseline: direct setuid spawn from a root process"
"$BIN" bench-direct -base "$ROOT/workspaces" -srv "$ROOT/srv" -ws "$WS" -n "$N"
echo "== helper log"; cat "$ROOT/helper.log"
