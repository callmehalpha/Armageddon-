#!/bin/sh
# Container entry point (contract §9.2, Docker Compose deployment).
#
# First start: writes the configuration from the environment:
#   ARMAGEDDON_PUBLIC_URL   how clients reach the server (default http://localhost:8080)
#   ARMAGEDDON_LISTEN       listen address (default :8080, or :443 with TLS)
#   ARMAGEDDON_DOMAIN + ARMAGEDDON_ACME_EMAIL [+ ARMAGEDDON_ACME_STAGING=1]   ACME
#   ARMAGEDDON_IP_ONLY=IP   self-signed certificate for IP (fingerprint in the log)
#
# Then runs the helper as root and the server as `armageddon` when the
# binary has `armageddon helper` (Phase 2); otherwise the server runs as
# root, as on a single-process host install. Per-workspace users are created
# inside the container. The Docker socket is never mounted into this
# container; Compose operations (a later phase) give it to the helper only.
set -eu
DATA=/var/lib/armageddon
CMD=${1:-run}

if [ "$CMD" != run ]; then
  exec armageddon "$@"
fi

mkdir -p "$DATA"
chmod 0755 "$DATA"
helper=
if armageddon helper --help >/dev/null 2>&1; then helper=1; fi

if [ ! -f "$DATA/server.json" ]; then
  set -- --data "$DATA"
  if [ -n "${ARMAGEDDON_DOMAIN:-}" ]; then
    set -- "$@" --domain "$ARMAGEDDON_DOMAIN" --acme-email "${ARMAGEDDON_ACME_EMAIL:?ARMAGEDDON_ACME_EMAIL is required with ARMAGEDDON_DOMAIN}"
    [ -z "${ARMAGEDDON_ACME_STAGING:-}" ] || set -- "$@" --acme-staging
  elif [ -n "${ARMAGEDDON_IP_ONLY:-}" ]; then
    set -- "$@" --ip-only --ip "$ARMAGEDDON_IP_ONLY"
  else
    set -- "$@" --listen "${ARMAGEDDON_LISTEN:-:8080}" --public-url "${ARMAGEDDON_PUBLIC_URL:-http://localhost:8080}"
  fi
  if [ -n "${ARMAGEDDON_LISTEN:-}" ]; then set -- "$@" --listen "$ARMAGEDDON_LISTEN"; fi
  if [ -n "${ARMAGEDDON_PUBLIC_URL:-}" ]; then set -- "$@" --public-url "$ARMAGEDDON_PUBLIC_URL"; fi
  [ -z "$helper" ] || set -- "$@" --owner armageddon
  armageddon server init "$@"
fi

if [ -n "$helper" ]; then
  chown armageddon:armageddon "$DATA"
  sock=/run/armageddon/helper.sock
  armageddon helper --data "$DATA" --socket "$sock" --server-user armageddon &
  # Wait for the socket: the server must never start without the helper.
  i=0
  while [ ! -S "$sock" ]; do
    i=$((i + 1))
    [ "$i" -le 100 ] || { echo "the privileged helper did not start" >&2; exit 1; }
    sleep 0.1
  done
  exec setpriv --reuid=armageddon --regid=armageddon --init-groups armageddon server run --data "$DATA" --helper-socket "$sock"
fi
echo "NOTICE: this armageddon binary has no privileged helper yet; the server runs as root in the container."
exec armageddon server run --data "$DATA"
