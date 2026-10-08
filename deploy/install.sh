#!/bin/sh
# Armageddon server installer (contract §9.2, plan M5.2).
#
#   curl -fsSL <install URL> | sudo sh
#   sudo sh install.sh --bundle armageddon-<v>-linux-<arch>.tar     (offline)
#
# Options:
#   --bundle FILE      install from a release bundle instead of downloading
#   --version V        release to download (default: latest)
#   --base URL         releases URL (default: GitHub Releases, decision D2)
#   --pubkey KEY       minisign public key that must have signed the manifest
#   --allow-unsigned   accept a release without a signature (development only)
#   --data DIR         data directory (default /var/lib/armageddon)
#   --prefix DIR       install under DIR instead of / (testing: no users,
#                      no packages, no systemctl; requirements only warn)
#   --no-start         install and enable, but do not start the services
#   --skip-requirements  warn instead of failing on RAM/disk/kernel checks
#
# Re-running is safe: every step checks what is already there.
set -eu

# Stamped by the release workflow when the release is signed (decision D1).
ARMAGEDDON_PUBKEY=${ARMAGEDDON_PUBKEY:-""}
BASE=https://github.com/callmehalpha/Armageddon-/releases
BUNDLE='' VERSION='' PREFIX='' DATA=/var/lib/armageddon NO_START='' SKIP_REQ='' ALLOW_UNSIGNED=''
MIN_GIT=2.39

say() { printf '==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case $1 in
    --bundle) BUNDLE=$2; shift 2 ;;
    --version) VERSION=${2#v}; shift 2 ;;
    --base) BASE=$2; shift 2 ;;
    --pubkey) ARMAGEDDON_PUBKEY=$2; shift 2 ;;
    --allow-unsigned) ALLOW_UNSIGNED=1; shift ;;
    --data) DATA=$2; shift 2 ;;
    --prefix) PREFIX=$2; shift 2 ;;
    --no-start) NO_START=1; shift ;;
    --skip-requirements) SKIP_REQ=1; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) die "unknown option $1 (see --help)" ;;
  esac
done
if [ -n "$PREFIX" ]; then
  PREFIX=$(cd "$PREFIX" && pwd) || die "--prefix directory must exist"
  SKIP_REQ=1
fi
[ -n "$PREFIX" ] || [ "$(id -u)" = 0 ] || die "run as root (sudo sh install.sh)"
[ -z "$BUNDLE" ] || [ -f "$BUNDLE" ] || die "bundle $BUNDLE not found"

# fail_req MESSAGE: a requirement is not met.
fail_req() { if [ -n "$SKIP_REQ" ]; then warn "$1"; else die "$1 (--skip-requirements to continue anyway)"; fi; }

# ---- 1. OS and architecture -------------------------------------------------
detect_os() {
  [ -r /etc/os-release ] || die "cannot read /etc/os-release"
  # In subshells: os-release sets VERSION, which is also our --version.
  # shellcheck disable=SC1091
  OS_ID=$(. /etc/os-release && echo "${ID:-unknown}")
  # shellcheck disable=SC1091
  OS_VERSION=$(. /etc/os-release && echo "${VERSION_ID:-}")
  # shellcheck disable=SC1091
  OS_LIKE=$(. /etc/os-release && echo "${ID_LIKE:-}")
  case " $OS_ID $OS_LIKE " in
    *" debian "*|*" ubuntu "*) PKG=apt ;;
    *" fedora "*|*" rhel "*|*" centos "*) PKG=dnf ;;
    *) PKG=none ;;
  esac
  case $OS_ID in
    debian|ubuntu|fedora) ;;
    *) fail_req "unsupported distribution '$OS_ID' (supported: Debian, Ubuntu, Fedora)" ;;
  esac
  case $(uname -m) in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported architecture $(uname -m) (amd64 and arm64 only)" ;;
  esac
  [ "$(uname -s)" = Linux ] || die "the server runs on Linux only"
  say "detected $OS_ID $OS_VERSION on $ARCH"
}

# ---- 2. Requirements ---------------------------------------------------------
version_ge() { # version_ge A B: A >= B (dotted numbers)
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n -k3,3n | head -1)" = "$2" ]
}
check_requirements() {
  kernel=$(uname -r | sed 's/[^0-9.].*//')
  version_ge "$kernel" 4.18 || fail_req "kernel $kernel is too old (4.18 or newer, 5.6+ recommended)"
  version_ge "$kernel" 5.6 || warn "kernel $kernel has no openat2 (5.6+): the helper cannot rule out symlink races"
  mem_kb=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
  [ "${mem_kb:-0}" -ge 900000 ] || fail_req "at least 1 GB of RAM is required (found $((mem_kb / 1024)) MB)"
  probe=$DATA; while [ ! -d "$probe" ]; do probe=$(dirname "$probe"); done
  free_kb=$(df -Pk "$probe" | awk 'NR==2 {print $4}')
  [ "${free_kb:-0}" -ge 10000000 ] || fail_req "at least 10 GB free is required for $DATA (found $((free_kb / 1048576)) GB)"
  if command -v ss >/dev/null 2>&1; then
    for p in 80 443; do
      if ss -Hltn "sport = :$p" 2>/dev/null | grep -q .; then
        warn "port $p is already in use: ACME and HTTPS need it (or choose a custom --listen port in server init)"
      fi
    done
  fi
}

git_version() { git --version 2>/dev/null | awk '{print $3}'; }
ensure_git() {
  if command -v git >/dev/null 2>&1 && version_ge "$(git_version)" "$MIN_GIT"; then
    say "git $(git_version) is new enough"
    return
  fi
  if [ -n "$PREFIX" ]; then
    warn "git $MIN_GIT or newer is needed (found '$(git_version)'); not installing packages under --prefix"
    return
  fi
  say "installing git"
  case $PKG in
    apt)
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq
      apt-get install -y -qq git ca-certificates curl >/dev/null ;;
    dnf) dnf install -y -q git-core ca-certificates curl >/dev/null ;;
    *) die "install git $MIN_GIT or newer, then re-run" ;;
  esac
  version_ge "$(git_version)" "$MIN_GIT" || die "git $(git_version) is older than $MIN_GIT after installing; install a newer git and re-run"
  say "git $(git_version) installed"
}

# ensure_openssl installs OpenSSL to check the release signature with,
# when neither minisign nor a capable openssl is present (minimal images).
ensure_openssl() {
  [ -z "$PREFIX" ] || return 0
  say "installing openssl to verify the release signature"
  case $PKG in
    apt)
      export DEBIAN_FRONTEND=noninteractive
      apt-get install -y -qq openssl >/dev/null 2>&1 ||
        { apt-get update -qq && apt-get install -y -qq openssl >/dev/null; } || true ;;
    dnf) dnf install -y -q openssl >/dev/null || true ;;
  esac
}

# ---- 3. Release: download or bundle, then verify -----------------------------
fetch() { # fetch URL FILE
  if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then wget -q -O "$2" "$1"
  else die "curl or wget is needed to download"; fi
}
# manifest_field NAME FIELD: a field of one file entry in manifest.json.
manifest_field() {
  awk -v want="$1" -v field="$2" '
    /"name":/ { gsub(/[",]/, "", $2); cur = $2 }
    $0 ~ "\"" field "\":" && cur == want { v = $2; gsub(/[",]/, "", v); print v; exit }
  ' "$STAGE/manifest.json"
}
manifest_version() { awk -F'"' '/"version":/ {print $4; exit}' "$STAGE/manifest.json"; }
sha256() { sha256sum "$1" | awk '{print $1}'; }

get_release() {
  STAGE=$(mktemp -d)
  trap 'rm -rf "$STAGE"' EXIT
  BIN_NAME=armageddon-linux-$ARCH
  if [ -n "$BUNDLE" ]; then
    say "unpacking $BUNDLE"
    tar -xf "$BUNDLE" -C "$STAGE"
    # Bundles may hold the files at the top level or in one directory.
    for f in "$STAGE"/*/*; do [ -f "$f" ] && mv "$f" "$STAGE/"; done
  else
    if [ -n "$VERSION" ]; then dir=$BASE/download/v$VERSION; else dir=$BASE/latest/download; fi
    say "downloading the release manifest from $dir"
    fetch "$dir/manifest.json" "$STAGE/manifest.json" || die "cannot download $dir/manifest.json"
    fetch "$dir/manifest.json.minisig" "$STAGE/manifest.json.minisig" 2>/dev/null || rm -f "$STAGE/manifest.json.minisig"
    for f in "$BIN_NAME" armageddon.service armageddon-helper.service armageddon-single.service; do
      fetch "$dir/$f" "$STAGE/$f" || die "cannot download $dir/$f"
    done
  fi
  [ -f "$STAGE/manifest.json" ] || die "the release has no manifest.json"
  [ -f "$STAGE/$BIN_NAME" ] || die "the release has no $BIN_NAME"
  VERSION=$(manifest_version)
  [ -n "$VERSION" ] || die "cannot read the version from manifest.json"
  verify_release
}

# openssl_ed25519: whether openssl can verify Ed25519 signatures over BLAKE2b.
openssl_ed25519() {
  command -v openssl >/dev/null 2>&1 && command -v base64 >/dev/null 2>&1 &&
    printf x | openssl dgst -blake2b512 -binary >/dev/null 2>&1 &&
    openssl list -public-key-algorithms 2>/dev/null | grep -qi ed25519
}

# minisign_openssl PUBKEY FILE SIGFILE verifies a minisign signature with
# openssl: the key ids must match, the signature must cover FILE ("Ed", what
# the release pipeline makes) or BLAKE2b-512(FILE) ("ED", minisign -H), and
# the global signature must cover the signature and its trusted comment.
minisign_openssl() {
  t=$(mktemp -d) || return 1
  # SubjectPublicKeyInfo DER prefix for an Ed25519 key.
  printf '\060\052\060\005\006\003\053\145\160\003\041\000' >"$t/der"
  printf '%s' "$1" | base64 -d >"$t/pk" 2>/dev/null &&
    [ "$(wc -c <"$t/pk")" -eq 42 ] &&
    [ "$(head -c 2 "$t/pk")" = Ed ] &&
    sed -n 2p "$3" | base64 -d >"$t/sig" 2>/dev/null &&
    [ "$(wc -c <"$t/sig")" -eq 74 ] &&
    alg=$(head -c 2 "$t/sig") && { [ "$alg" = Ed ] || [ "$alg" = ED ]; } &&
    [ "$(tail -c +3 "$t/pk" | head -c 8 | od -An -tx1)" = "$(tail -c +3 "$t/sig" | head -c 8 | od -An -tx1)" ] &&
    tail -c 32 "$t/pk" >>"$t/der" &&
    openssl pkey -pubin -inform DER -in "$t/der" -out "$t/pub.pem" 2>/dev/null &&
    tail -c 64 "$t/sig" >"$t/s" &&
    if [ "$alg" = ED ]; then openssl dgst -blake2b512 -binary "$2" >"$t/h"; else cp "$2" "$t/h"; fi &&
    openssl pkeyutl -verify -pubin -inkey "$t/pub.pem" -rawin -in "$t/h" -sigfile "$t/s" >/dev/null 2>&1 &&
    sed -n 3p "$3" | grep -q '^trusted comment: ' &&
    sed -n 4p "$3" | base64 -d >"$t/gs" 2>/dev/null &&
    { cat "$t/s"; sed -n 3p "$3" | sed 's/^trusted comment: //' | tr -d '\n'; } >"$t/g" &&
    openssl pkeyutl -verify -pubin -inkey "$t/pub.pem" -rawin -in "$t/g" -sigfile "$t/gs" >/dev/null 2>&1
  rc=$?
  rm -rf "$t"
  return $rc
}

verify_release() {
  # Every artefact against the manifest's sha256 first.
  for f in "$BIN_NAME" armageddon.service armageddon-helper.service armageddon-single.service; do
    [ -f "$STAGE/$f" ] || continue
    want=$(manifest_field "$f" sha256)
    [ -n "$want" ] || die "$f is not listed in the manifest"
    [ "$(sha256 "$STAGE/$f")" = "$want" ] || die "$f does not match the manifest (sha256 mismatch): refusing to install"
  done
  # Then the manifest's signature.
  if [ ! -f "$STAGE/manifest.json.minisig" ]; then
    [ -n "$ALLOW_UNSIGNED" ] || die "release $VERSION is not signed (no manifest.json.minisig); --allow-unsigned installs it anyway"
    warn "release $VERSION is UNSIGNED; installing because of --allow-unsigned"
    return
  fi
  if [ -z "$ARMAGEDDON_PUBKEY" ]; then
    [ -n "$ALLOW_UNSIGNED" ] || die "no release public key is configured in this installer (--pubkey KEY); --allow-unsigned skips the check"
    warn "signature NOT checked: no public key (--allow-unsigned)"
    return
  fi
  command -v minisign >/dev/null 2>&1 || openssl_ed25519 || ensure_openssl
  if command -v minisign >/dev/null 2>&1; then
    minisign -Vq -P "$ARMAGEDDON_PUBKEY" -m "$STAGE/manifest.json" -x "$STAGE/manifest.json.minisig" ||
      die "manifest signature is INVALID: refusing to install"
  elif openssl_ed25519; then
    # Without the minisign tool, openssl checks the same signature. The
    # downloaded binary never verifies itself (security review M9.2).
    minisign_openssl "$ARMAGEDDON_PUBKEY" "$STAGE/manifest.json" "$STAGE/manifest.json.minisig" ||
      die "manifest signature is INVALID: refusing to install"
  else
    die "cannot check the release signature: install minisign, or OpenSSL 1.1.1 or newer (--allow-unsigned skips the check)"
  fi
  say "release $VERSION: signature and checksums verified"
}

# ---- 4. Layout ---------------------------------------------------------------
P() { printf '%s%s' "$PREFIX" "$1"; }
install_layout() {
  vdir=$(P /opt/armageddon/versions/"$VERSION")
  if [ -x "$vdir/armageddon" ] && [ "$(sha256 "$vdir/armageddon")" = "$(sha256 "$STAGE/$BIN_NAME")" ]; then
    say "version $VERSION already installed in $vdir"
  else
    [ ! -e "$vdir" ] || die "$vdir exists but differs from the release; remove it or use 'armageddon server update'"
    mkdir -p "$(P /opt/armageddon/versions)"
    tmp=$(P /opt/armageddon/versions/.install-"$VERSION")
    rm -rf "$tmp"; mkdir -p "$tmp"
    install -m 0755 "$STAGE/$BIN_NAME" "$tmp/armageddon"
    install -m 0644 "$STAGE/manifest.json" "$tmp/manifest.json"
    [ ! -f "$STAGE/manifest.json.minisig" ] || install -m 0644 "$STAGE/manifest.json.minisig" "$tmp/"
    mv "$tmp" "$vdir"
    say "installed $vdir"
  fi
  cur=$(P /opt/armageddon/current)
  if [ -L "$cur" ] && [ "$(readlink "$cur")" != "versions/$VERSION" ]; then
    die "Armageddon $(basename "$(readlink "$cur")") is already installed: use 'armageddon server update --to $VERSION' to change versions"
  fi
  ln -sfn "versions/$VERSION" "$cur"
  mkdir -p "$(P /usr/local/bin)"
  ln -sfn /opt/armageddon/current/armageddon "$(P /usr/local/bin/armageddon)"
  BIN=$vdir/armageddon
}

ensure_user() {
  [ -z "$PREFIX" ] || return 0
  if id armageddon >/dev/null 2>&1; then
    say "user armageddon exists"
  else
    nologin=$(command -v nologin || echo /usr/sbin/nologin)
    useradd --system --user-group --home-dir "$DATA" --no-create-home --shell "$nologin" armageddon
    say "created system user armageddon"
  fi
}

ensure_dirs() {
  data=$(P "$DATA")
  mkdir -p "$data" "$(P /etc/armageddon)"
  # Workspace users must traverse the data directory to reach their own
  # directories; everything sensitive inside it is 0600/0700.
  chmod 0755 "$data"
  if [ -z "$PREFIX" ] && [ -n "$HELPER" ]; then
    chown armageddon:armageddon "$data"
  fi
  # The configuration lives in the data directory (written and migrated by
  # the binary, contract §9.3); /etc/armageddon/server.yaml points at it.
  # JSON is valid YAML.
  ln -sfn "$DATA/server.json" "$(P /etc/armageddon/server.yaml)"
}

detect_helper() {
  HELPER=
  if "$BIN" helper --help >/dev/null 2>&1; then
    HELPER=1
    say "this release has the privileged helper: two services (armageddon-helper as root, armageddon as user armageddon)"
  else
    say "NOTICE: this release has no 'armageddon helper' yet: installing a single armageddon.service running as root."
    say "        Re-run this installer after upgrading to a release with the helper to switch to the two-service layout."
  fi
}

install_units() {
  units=$(P /etc/systemd/system)
  mkdir -p "$units"
  src=$STAGE
  [ -f "$src/armageddon.service" ] || src=$(dirname "$0")/systemd
  if [ -n "$HELPER" ]; then
    install -m 0644 "$src/armageddon-helper.service" "$units/armageddon-helper.service"
    install -m 0644 "$src/armageddon.service" "$units/armageddon.service"
  else
    rm -f "$units/armageddon-helper.service"
    install -m 0644 "$src/armageddon-single.service" "$units/armageddon.service"
  fi
  say "installed systemd units in $units"
}

init_config() {
  if [ -f "$(P "$DATA")/server.json" ]; then
    say "configuration exists ($DATA/server.json); not touching it"
    return
  fi
  # Local-only until the operator chooses a domain or IP-only mode.
  owner=
  [ -z "$HELPER" ] || [ -n "$PREFIX" ] || owner="--owner armageddon"
  # shellcheck disable=SC2086
  "$BIN" server init --data "$(P "$DATA")" --listen 127.0.0.1:8080 --public-url http://127.0.0.1:8080 $owner >/dev/null
  say "wrote a local-only configuration (127.0.0.1:8080)"
}

start_services() {
  if [ -n "$PREFIX" ]; then say "--prefix: not touching systemd"; return; fi
  command -v systemctl >/dev/null 2>&1 || { warn "systemctl not found: start 'armageddon server run --data $DATA' yourself"; return; }
  systemctl daemon-reload
  if [ -n "$HELPER" ]; then u="armageddon-helper.service armageddon.service"; else u=armageddon.service; fi
  if [ -n "$NO_START" ]; then
    # shellcheck disable=SC2086
    systemctl enable $u >/dev/null 2>&1
    say "enabled $u (not started: --no-start)"
    return
  fi
  # shellcheck disable=SC2086
  systemctl enable $u >/dev/null 2>&1
  # shellcheck disable=SC2086
  systemctl restart $u
  if "$BIN" server health --data "$DATA" --timeout 60s; then
    say "armageddon is running"
    return
  fi
  systemctl --no-pager status armageddon.service || true
  die "the server did not become healthy; see journalctl -u armageddon"
}

detect_os
check_requirements
ensure_git
get_release
install_layout
detect_helper
ensure_user
ensure_dirs
install_units
init_config
start_services

cat <<EOF

Armageddon $VERSION is installed.

Next, choose how clients reach this server, then restart it:

  sudo armageddon server init --domain dev.example.com --acme-email you@example.com
  sudo armageddon server init --ip-only          # no domain: self-signed, prints a fingerprint
  sudo systemctl restart armageddon

The one-time URL for the first admin is in the log:  journalctl -u armageddon | grep setup
Health check:  sudo armageddon doctor
EOF
