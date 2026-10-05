#!/usr/bin/env bash
# Runs the browser test (ui.mjs) against the split deployment: the helper
# as root, the server as the unprivileged armageddon user.
#
#   sudo test/e2e/ui.sh [path/to/armageddon]      (needs node + playwright)
set -euo pipefail
BIN=$(readlink -f "${1:-$(command -v armageddon)}")
HERE=$(cd "$(dirname "$0")" && pwd)
PORT=${PORT:-8092}
ROOT=${E2E_ROOT:-/srv/armageddon-ui-$$}
SU=${SERVER_USER:-armageddon}
DATA=$ROOT/server SOCK=$ROOT/run/helper.sock
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
id "$SU" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SU" 2>/dev/null || id "$SU" >/dev/null
rm -rf "$ROOT"; mkdir -p "$DATA"; chmod 755 "$ROOT"; chown "$SU:" "$DATA"
as_server() { setpriv --reuid="$SU" --regid="$SU" --clear-groups -- env HOME="$DATA" "$@"; }
as_server "$BIN" server init --data "$DATA" --listen "127.0.0.1:$PORT" --public-url "http://127.0.0.1:$PORT" >/dev/null
"$BIN" helper --data "$DATA" --socket "$SOCK" --server-user "$SU" >"$ROOT/helper.log" 2>&1 &
H=$!
S=
trap 'kill $S $H 2>/dev/null || true' EXIT
for _ in $(seq 50); do [ -S "$SOCK" ] && break; sleep 0.1; done
setpriv --reuid="$SU" --regid="$SU" --clear-groups -- env HOME="$DATA" \
  "$BIN" server run --data "$DATA" --helper-socket "$SOCK" >"$ROOT/server.log" 2>&1 &
S=$!
for _ in $(seq 50); do curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null && break; sleep 0.2; done
URL=$(grep -o 'http://[^ ]*setup?token=[a-z0-9]*' "$ROOT/server.log" | head -1)
node "$HERE/ui.mjs" "$URL" "${SHOTS:-$ROOT/shots}"
