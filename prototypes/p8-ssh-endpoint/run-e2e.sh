#!/usr/bin/env bash
# P8 end-to-end with the stock OpenSSH client (throwaway).
#
#   sudo prototypes/p8-ssh-endpoint/run-e2e.sh
#
# Checks: Ed25519 device-key auth, exec, PTY, SFTP, `ssh -L` and `ssh -D` to
# loopback, refusal of non-loopback forwards, and an emulation of the VS Code
# Remote-SSH bootstrap (bash script over exec downloads + starts a server,
# client then reaches it through a forward).
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
T=/var/lib/p8-e2e-$$
BIN=/usr/local/bin/p8-proto-$$
WSU=ws-p8e2e$(printf '%05d' $(( $$ % 100000 )))
PORT=${P8_PORT:-22801}; APP=$((PORT+1)); UPD=$((PORT+2)); FWD=$((PORT+3)); SOCKS=$((PORT+4)); FWD2=$((PORT+5))
PIDS=()

pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; exit 1; }
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  pkill -u "$WSU" 2>/dev/null || true
  userdel "$WSU" 2>/dev/null || true
  rm -rf "$T" "$BIN"
}
trap cleanup EXIT
[ "$(id -u)" = 0 ] || fail "run as root"

(cd "$HERE/.." && go build -o "$BIN" ./p8-ssh-endpoint); chmod 0755 "$BIN"
mkdir -p "$T/client" "$T/home" "$T/upd"; chmod 755 "$T"
useradd --system --user-group --home-dir "$T/home" --shell /bin/bash "$WSU"
chown "$WSU:" "$T/home"; chmod 700 "$T/home"
WSUID=$(id -u "$WSU")

ssh-keygen -q -t ed25519 -N '' -f "$T/client/dev" -C device
ssh-keygen -q -t ed25519 -N '' -f "$T/client/other" -C unpaired
ssh-keygen -q -t rsa -b 2048 -N '' -f "$T/client/rsa" -C rsa
{ echo "$WSU $(cat "$T/client/dev.pub")"; echo "$WSU $(cat "$T/client/rsa.pub")"; } > "$T/authorized"

"$BIN" serve -listen 127.0.0.1:$PORT -keys "$T/authorized" -hostkey "$T/hostkey" 2>"$T/server.log" &
PIDS+=($!)
for _ in $(seq 50); do (exec 3<>/dev/tcp/127.0.0.1/$PORT) 2>/dev/null && break; sleep 0.1; done

SSHO=(-p $PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o IdentitiesOnly=yes)
S() { ssh "${SSHO[@]}" -i "$T/client/dev" "$WSU@127.0.0.1" "$@"; }

echo "== auth"
check "paired Ed25519 device key logs in" test "$(S true && echo ok)" = ok
check "unpaired Ed25519 key is refused" bash -c "! ssh ${SSHO[*]} -i '$T/client/other' $WSU@127.0.0.1 true 2>/dev/null"
check "RSA key is refused even when listed (Ed25519 only)" bash -c "! ssh ${SSHO[*]} -i '$T/client/rsa' $WSU@127.0.0.1 true 2>/dev/null"
check "unknown user is refused" bash -c "! ssh ${SSHO[*]} -i '$T/client/dev' ws-nosuchuser0@127.0.0.1 true 2>/dev/null"

echo "== exec / pty"
check "exec runs as the workspace user (uid $WSUID)" test "$(S id -u)" = "$WSUID"
check "exec exit status propagates" bash -c "S() { ssh ${SSHO[*]} -i '$T/client/dev' $WSU@127.0.0.1 \"\$@\"; }; S 'exit 7'; [ \$? = 7 ]"
OUT=$(S -tt 'tty; echo cols=$(tput cols 2>/dev/null || stty size)' </dev/null | tr -d '\r')
check "PTY session has a tty" bash -c "grep -q '^/dev/pts/' <<<'$OUT'"

echo "== sftp"
echo "hello sftp" > "$T/client/up.txt"
sftp -b - "${SSHO[@]/-p/-P}" -i "$T/client/dev" "$WSU@127.0.0.1" >/dev/null <<EOF
put $T/client/up.txt up.txt
get up.txt $T/client/down.txt
EOF
check "sftp put+get round-trips" cmp -s "$T/client/up.txt" "$T/client/down.txt"
check "sftp-written file is owned by the workspace user" test "$(stat -c %U "$T/home/up.txt")" = "$WSU"

echo "== port forwarding"
runuser -u "$WSU" -- bash -c "cd '$T/home' && echo app-ok > index.html && exec python3 -m http.server --bind 127.0.0.1 $APP" >/dev/null 2>&1 &
PIDS+=($!); sleep 0.7
ssh "${SSHO[@]}" -i "$T/client/dev" -N -L $FWD:127.0.0.1:$APP "$WSU@127.0.0.1" & PIDS+=($!); sleep 0.7
check "ssh -L to a loopback port works" test "$(curl -s http://127.0.0.1:$FWD/index.html)" = app-ok
ssh "${SSHO[@]}" -i "$T/client/dev" -N -D $SOCKS "$WSU@127.0.0.1" & PIDS+=($!); sleep 0.7
check "ssh -D (SOCKS, used by VS Code local-server mode) to loopback works" test "$(curl -s --socks5-hostname 127.0.0.1:$SOCKS http://127.0.0.1:$APP/index.html)" = app-ok
check "forward to a non-loopback destination is refused" bash -c "! ssh ${SSHO[*]} -i '$T/client/dev' -W 192.0.2.1:80 $WSU@127.0.0.1 </dev/null 2>/dev/null"

echo "== VS Code Remote-SSH emulation"
# A stand-in for the VS Code update server: serves a tarball holding a tiny
# "server" that listens on an ephemeral loopback port and records it.
mkdir -p "$T/pkg/bin"
cat > "$T/pkg/bin/code-server-stub" <<'PY'
#!/usr/bin/env python3
import http.server, os, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"vscode-server-ok uid=%d" % os.getuid())
    def log_message(self, *a): pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(s.server_port))
s.serve_forever()
PY
chmod 755 "$T/pkg/bin/code-server-stub"
tar -C "$T/pkg" -czf "$T/upd/vscode-server.tar.gz" .
(cd "$T/upd" && exec python3 -m http.server --bind 127.0.0.1 $UPD) >/dev/null 2>&1 & PIDS+=($!); sleep 0.7
# Like Remote-SSH: one exec channel running bash with the script on stdin.
BOOT=$(ssh "${SSHO[@]}" -i "$T/client/dev" -T "$WSU@127.0.0.1" bash <<EOF
set -e
DIR=\$HOME/.vscode-server/bin/0123abcd
mkdir -p "\$DIR"
curl -sf http://127.0.0.1:$UPD/vscode-server.tar.gz | tar -C "\$DIR" -xzf -
rm -f "\$DIR/port"
nohup "\$DIR/bin/code-server-stub" "\$DIR/port" >/dev/null 2>&1 &
for i in \$(seq 50); do [ -s "\$DIR/port" ] && break; sleep 0.1; done
echo "listeningOn==\$(cat "\$DIR/port")=="
EOF
)
RPORT=$(sed -n 's/.*listeningOn==\([0-9]*\)==.*/\1/p' <<<"$BOOT")
check "bootstrap script over exec downloaded and started the server (port ${RPORT:-none})" test -n "$RPORT"
ssh "${SSHO[@]}" -i "$T/client/dev" -N -L $FWD2:127.0.0.1:$RPORT "$WSU@127.0.0.1" & PIDS+=($!); sleep 0.7
check "client reaches the remote server through the forward, running as $WSU" test "$(curl -s http://127.0.0.1:$FWD2/)" = "vscode-server-ok uid=$WSUID"
check "server survives the bootstrap session (nohup, as Remote-SSH expects)" test "$(curl -s --socks5-hostname 127.0.0.1:$SOCKS http://127.0.0.1:$RPORT/)" = "vscode-server-ok uid=$WSUID"
echo "== all P8 checks passed"
