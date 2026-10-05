#!/usr/bin/env bash
# Installer test without touching the machine: builds a release with
# deploy/make-release.sh and runs deploy/install.sh --prefix against a
# temporary root. Covers: refusal of unsigned and tampered releases, a
# signed install, the versioned layout, the unit choice (single vs helper)
# and idempotent re-runs. No users, packages or systemd are touched.
#
#   test/install/prefix.sh
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
trap 'echo "FAILED at line $LINENO: $BASH_COMMAND" >&2; [ -f "$W/log" ] && grep -v "^  " "$W/log" | tail -8 >&2' ERR
pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
ARCH=$(uname -m); case $ARCH in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac

# A throwaway signing key, generated for this run only.
go build -o "$W/tool" "$ROOT/cmd/armageddon"
MINISIGN_PASSWORD=$(openssl rand -hex 24)
export MINISIGN_PASSWORD
"$W/tool" release keygen --out "$W/key" >/dev/null
PUB=$(tail -1 "$W/key.pub")

TARGETS="linux/$ARCH" "$ROOT/deploy/make-release.sh" 0.2.0 test "$W/unsigned" >>"$W/log" 2>&1
MINISIGN_SECRET_KEY=$(cat "$W/key.key") TARGETS="linux/$ARCH" "$ROOT/deploy/make-release.sh" 0.2.0 test "$W/signed" >>"$W/log" 2>&1
UB=$W/unsigned/armageddon-0.2.0-linux-$ARCH.tar
SB=$W/signed/armageddon-0.2.0-linux-$ARCH.tar
check "signed release has a signature in its bundle" bash -c "tar -tf '$SB' | grep -q manifest.json.minisig"
check "the published installer carries the public key" grep -q "$PUB" "$W/signed/install.sh"

inst() { # inst PREFIX ARGS...
  local p=$1; shift; mkdir -p "$p"
  bash "$W/signed/install.sh" --prefix "$p" "$@" >"$W/out" 2>&1
}
refused() { # refused MESSAGE PREFIX ARGS...: the install fails, saying MESSAGE
  local msg=$1; shift
  if inst "$@"; then return 1; fi
  grep -q "$msg" "$W/out"
}
check "unsigned release refused by default" refused 'not signed' "$W/p0" --bundle "$UB"

# Tampered binary inside a correctly signed bundle.
mkdir -p "$W/t" && tar -xf "$SB" -C "$W/t" && echo backdoor >>"$W/t/armageddon-linux-$ARCH"
(cd "$W/t" && tar -cf "$W/tampered.tar" ./*)
check "tampered binary refused (sha256 mismatch)" refused 'sha256 mismatch' "$W/p1" --bundle "$W/tampered.tar"
# Tampered manifest (checksum edited to match the tampered binary): signature fails.
sum=$(sha256sum "$W/t/armageddon-linux-$ARCH" | cut -d' ' -f1)
size=$(stat -c %s "$W/t/armageddon-linux-$ARCH")
python3 - "$W/t/manifest.json" "armageddon-linux-$ARCH" "$sum" "$size" <<'EOF'
import json, sys
p, name, s, size = sys.argv[1:]
m = json.load(open(p))
for f in m["files"]:
    if f["name"] == name: f["sha256"], f["size"] = s, int(size)
open(p, "w").write(json.dumps(m, indent=2) + "\n")
EOF
(cd "$W/t" && tar -cf "$W/tampered2.tar" ./*)
check "tampered manifest refused (signature)" refused 'signature is INVALID' "$W/p2" --bundle "$W/tampered2.tar"

P=$W/root
inst "$P" --bundle "$SB" || { cat "$W/out"; fail "signed install"; }
pass "signed release installed under --prefix"
check "versioned layout: versions/0.2.0/armageddon" test -x "$P/opt/armageddon/versions/0.2.0/armageddon"
check "current → versions/0.2.0" test "$(readlink "$P/opt/armageddon/current")" = versions/0.2.0
check "/usr/local/bin/armageddon → /opt/armageddon/current/armageddon" test "$(readlink "$P/usr/local/bin/armageddon")" = /opt/armageddon/current/armageddon
check "/etc/armageddon/server.yaml points at the config" test "$(readlink "$P/etc/armageddon/server.yaml")" = /var/lib/armageddon/server.json
check "local-only config written" grep -q '127.0.0.1:8080' "$P/var/lib/armageddon/server.json"
if "$P/opt/armageddon/current/armageddon" helper --help >/dev/null 2>&1; then
  check "helper layout: armageddon-helper.service installed" test -f "$P/etc/systemd/system/armageddon-helper.service"
  check "server unit runs as armageddon" grep -q '^User=armageddon' "$P/etc/systemd/system/armageddon.service"
else
  check "no helper: single root unit installed as armageddon.service" grep -q 'single process' "$P/etc/systemd/system/armageddon.service"
  check "no helper unit installed" test ! -e "$P/etc/systemd/system/armageddon-helper.service"
  check "the notice was printed" grep -q "NOTICE: this release has no 'armageddon helper'" "$W/out"
fi
before=$(find "$P" -printf '%p %s %m\n' | sort | sha256sum)
inst "$P" --bundle "$SB" || { cat "$W/out"; fail "re-run"; }
after=$(find "$P" -printf '%p %s %m\n' | sort | sha256sum)
check "re-running the installer is idempotent" test "$before" = "$after"
check "re-run kept the configuration" grep -q 'configuration exists' "$W/out"
printf '\n\033[32mALL INSTALLER (PREFIX) CHECKS PASSED\033[0m\n'
