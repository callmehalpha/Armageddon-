#!/bin/sh
# Builds a release into OUT (used by .github/workflows/release.yml and the
# CI install job):
#
#   deploy/make-release.sh VERSION COMMIT OUT
#
#   armageddon-{linux,darwin}-{amd64,arm64}   binaries, version and commit stamped
#   install.sh, *.service                    installer and systemd units
#   manifest.json [+ manifest.json.minisig]  signed when MINISIGN_SECRET_KEY is set
#   armageddon-VERSION-linux-ARCH.tar         offline bundles for install.sh --bundle
#
# Environment:
#   MINISIGN_SECRET_KEY, MINISIGN_PASSWORD  signing key (a GitHub secret); unset = unsigned
#   CODE_SERVER_JSON                        pinned code-server entry (see deploy/code-server.json)
#   TARGETS                                 default "linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"
set -eu
[ $# -eq 3 ] || { echo "usage: $0 VERSION COMMIT OUT" >&2; exit 2; }
VERSION=${1#v} COMMIT=$2 OUT=$3
TARGETS=${TARGETS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
PKG=github.com/callmehalpha/Armageddon-

# A tool binary for this machine, to derive the public key, write the
# manifest and sign it.
TOOL=$OUT/.tool-armageddon
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$TOOL" ./cmd/armageddon)

PUBKEY=
if [ -n "${MINISIGN_SECRET_KEY:-}" ]; then
  PUBKEY=$("$TOOL" release pubkey)
  echo "signing with public key $PUBKEY"
else
  echo "MINISIGN_SECRET_KEY is not set: the release will be UNSIGNED"
fi

for t in $TARGETS; do
  os=${t%/*} arch=${t#*/}
  echo "building $os/$arch"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X $PKG/internal/release.PublicKey=$PUBKEY" \
    -o "$OUT/armageddon-$os-$arch" ./cmd/armageddon)
done

cp "$ROOT"/deploy/systemd/*.service "$OUT/"
# The published installer carries the public key (decision D1).
sed "s|^ARMAGEDDON_PUBKEY=\${ARMAGEDDON_PUBKEY:-\"\"}|ARMAGEDDON_PUBKEY=\${ARMAGEDDON_PUBKEY:-\"$PUBKEY\"}|" \
  "$ROOT/deploy/install.sh" >"$OUT/install.sh"
chmod 0755 "$OUT/install.sh"

set -- --dir "$OUT" --version "$VERSION" --commit "$COMMIT"
[ -z "${CODE_SERVER_JSON:-}" ] || set -- "$@" --code-server "$CODE_SERVER_JSON"
"$TOOL" release manifest "$@"
if [ -n "$PUBKEY" ]; then
  "$TOOL" release sign "$OUT/manifest.json"
  "$TOOL" release verify "$OUT/manifest.json" "$OUT/manifest.json.minisig" --pubkey "$PUBKEY"
fi

for t in $TARGETS; do
  os=${t%/*} arch=${t#*/}
  [ "$os" = linux ] || continue
  b=armageddon-$VERSION-linux-$arch.tar
  set -- manifest.json "armageddon-linux-$arch" install.sh armageddon.service armageddon-helper.service armageddon-single.service
  [ ! -f "$OUT/manifest.json.minisig" ] || set -- "$@" manifest.json.minisig
  (cd "$OUT" && tar -cf "$b" "$@")
  echo "bundle $b"
done
rm -f "$TOOL"
(cd "$OUT" && sha256sum -- * >SHA256SUMS)
