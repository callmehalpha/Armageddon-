#!/usr/bin/env bash
# install.sh in a systemd-enabled container (plan M5.2, M5.4 on real
# systemd). Used by the CI install job for Debian 12, Ubuntu 24.04 and
# Fedora 40:
#
#   test/install/container.sh IMAGE DIST1 DIST2
#
# DIST1 and DIST2 are two releases from deploy/make-release.sh signed with
# the same key (DIST2 newer). Steps: install DIST1 from its bundle, check
# the services, user, layout and git; re-run (idempotent); IP-only TLS;
# `server update --bundle` to DIST2 and `server rollback`; doctor;
# uninstall, then uninstall --purge with the typed confirmation.
#
# Sandbox-only knobs: LOCAL_CA (a CA bundle to trust inside the container)
# and LOCAL_PROXY (an HTTPS proxy for package downloads).
set -euo pipefail
IMAGE=$1 D1=$(cd "$2" && pwd) D2=$(cd "$3" && pwd)
NAME=armageddon-install-$$
ARCH=$(uname -m); case $ARCH in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac
V1=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$D1/manifest.json")
V2=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$D2/manifest.json")
pass() { printf '\033[32mPASS\033[0m [%s] %s\n' "$IMAGE" "$*"; }
fail() { printf '\033[31mFAIL\033[0m [%s] %s\n' "$IMAGE" "$*"; docker exec "$NAME" journalctl -u armageddon --no-pager -n 30 2>/dev/null || true; exit 1; }
check() { local d=$1; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
dx() { docker exec -i "$NAME" "$@"; }
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; docker rmi "$NAME-img" >/dev/null 2>&1 || true; }
trap cleanup EXIT

ctx=$(mktemp -d)
pre=""
if [ -n "${LOCAL_CA:-}" ]; then
  cp "$LOCAL_CA" "$ctx/ca.crt"
  pre="COPY ca.crt /usr/local/share/armageddon-test-ca.crt
ENV https_proxy=${LOCAL_PROXY:-} SSL_CERT_FILE=/usr/local/share/armageddon-test-ca.crt CURL_CA_BUNDLE=/usr/local/share/armageddon-test-ca.crt
RUN if [ -f /etc/apt/sources.list.d/ubuntu.sources ]; then sed -i 's#http://#https://#g' /etc/apt/sources.list.d/ubuntu.sources; fi; \
    if [ -f /etc/apt/sources.list.d/debian.sources ]; then sed -i 's#http://#https://#g' /etc/apt/sources.list.d/debian.sources; fi; \
    mkdir -p /etc/apt/apt.conf.d && echo 'Acquire::https::CAInfo \"/usr/local/share/armageddon-test-ca.crt\";' >/etc/apt/apt.conf.d/99ca; \
    if [ -d /etc/dnf ]; then echo 'sslcacert=/usr/local/share/armageddon-test-ca.crt' >>/etc/dnf/dnf.conf; fi"
fi
case $IMAGE in
  fedora*) prep="dnf install -y -q systemd procps-ng iproute curl python3 && dnf clean all" ;;
  *) prep="apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq systemd systemd-sysv curl ca-certificates iproute2 python3 >/dev/null && rm -rf /var/lib/apt/lists/*" ;;
esac
cat >"$ctx/Dockerfile" <<EOF
FROM $IMAGE
$pre
RUN $prep
RUN systemctl mask systemd-resolved.service 2>/dev/null || true
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
EOF
docker build -q --network host -t "$NAME-img" "$ctx" >/dev/null
docker run -d --name "$NAME" --privileged --cgroupns=host --network host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock "$NAME-img" >/dev/null
for _ in $(seq 60); do
  st=$(dx systemctl is-system-running 2>/dev/null || true)
  case $st in running|degraded) break ;; esac
  sleep 1
done
check "systemd is up in the container ($st)" bash -c "case '$st' in running|degraded) true ;; *) false ;; esac"
# DNS: with host networking the container must use the runner's upstream
# resolvers, not a 127.0.0.53 stub (the image's own systemd-resolved is
# masked below). /etc/resolv.conf is usually a Docker bind mount (write
# through it) but may be a dangling link into the /run tmpfs (replace it).
resolv=/etc/resolv.conf
[ -f /run/systemd/resolve/resolv.conf ] && resolv=/run/systemd/resolve/resolv.conf
dx sh -c 'if [ -L /etc/resolv.conf ]; then rm -f /etc/resolv.conf; fi; cat >/etc/resolv.conf' <"$resolv"

docker cp "$D1/armageddon-$V1-linux-$ARCH.tar" "$NAME:/root/r1.tar"
docker cp "$D2/armageddon-$V2-linux-$ARCH.tar" "$NAME:/root/r2.tar"
docker cp "$D1/install.sh" "$NAME:/root/install.sh"

# Requirements are checked and reported; CI runners can be short on disk,
# so they only warn here. A real VPS run must pass them (see the PR).
dx sh /root/install.sh --bundle /root/r1.tar --skip-requirements >/tmp/"$NAME".1 2>&1 || { cat /tmp/"$NAME".1; fail "install.sh"; }
pass "install.sh --bundle finished"
check "armageddon.service is active" dx systemctl is-active --quiet armageddon.service
check "server answers /healthz" dx armageddon server health --data /var/lib/armageddon --timeout 30s
check "armageddon system user exists" dx id armageddon
# shellcheck disable=SC2016 # expanded inside the container
check "git >= 2.39 installed" dx sh -c 'v=$(git --version | cut -d" " -f3); [ "$(printf "%s\n2.39\n" "$v" | sort -V | head -1)" = 2.39 ]'
check "layout: current → versions/$V1" test "$(dx readlink /opt/armageddon/current)" = "versions/$V1"
check "/usr/local/bin/armageddon runs $V1" bash -c "docker exec $NAME armageddon version | grep -q '$V1'"
if dx /opt/armageddon/current/armageddon helper --help >/dev/null 2>&1; then
  check "helper layout: armageddon-helper.service active" dx systemctl is-active --quiet armageddon-helper.service
  check "server runs as armageddon" test "$(dx ps -o user= -C armageddon | sort -u | tr '\n' ' ')" != "root "
else
  check "single-process layout with the notice" grep -q "NOTICE: this release has no 'armageddon helper'" /tmp/"$NAME".1
fi

dx sh /root/install.sh --bundle /root/r1.tar --skip-requirements >/tmp/"$NAME".2 2>&1 || { cat /tmp/"$NAME".2; fail "re-run"; }
check "re-run is idempotent and keeps the service healthy" bash -c "grep -q 'already installed' /tmp/$NAME.2 && docker exec $NAME armageddon server health --data /var/lib/armageddon --timeout 30s"

dx armageddon server init --ip-only --ip 127.0.0.1 --listen 127.0.0.1:8443 >/tmp/"$NAME".3
dx systemctl restart armageddon.service
check "IP-only TLS after server init + restart" dx armageddon server health --data /var/lib/armageddon --timeout 30s
check "fingerprint printed by init matches server fingerprint" grep -q "$(dx armageddon server fingerprint)" /tmp/"$NAME".3

dx armageddon server update --bundle /root/r2.tar >/tmp/"$NAME".4 2>&1 || { cat /tmp/"$NAME".4; fail "server update"; }
check "server update → $V2 (current switched, healthy)" bash -c "[ \"\$(docker exec $NAME readlink /opt/armageddon/current)\" = versions/$V2 ] && docker exec $NAME armageddon server health --data /var/lib/armageddon --timeout 30s"
dx armageddon server rollback >/tmp/"$NAME".5 2>&1 || { cat /tmp/"$NAME".5; fail "server rollback"; }
check "server rollback → $V1, healthy" bash -c "[ \"\$(docker exec $NAME readlink /opt/armageddon/current)\" = versions/$V1 ] && docker exec $NAME armageddon server health --data /var/lib/armageddon --timeout 30s"

dx armageddon doctor >/tmp/"$NAME".6 2>&1 || true
cat /tmp/"$NAME".6
check "doctor reports no failures" bash -c "! grep -q '\[FAIL\]' /tmp/$NAME.6"

dx cp /opt/armageddon/current/armageddon /root/armageddon # uninstall removes the installed copy
dx armageddon server uninstall >/dev/null
check "uninstall removes the service and binaries, keeps data" bash -c "! docker exec $NAME test -e /etc/systemd/system/armageddon.service && ! docker exec $NAME test -e /opt/armageddon && docker exec $NAME test -f /var/lib/armageddon/armageddon.db"
check "--purge without the typed phrase removes nothing" bash -c "echo yes | docker exec -i $NAME /root/armageddon server uninstall --purge >/dev/null 2>&1; docker exec $NAME test -f /var/lib/armageddon/armageddon.db"
echo "delete /var/lib/armageddon" | dx /root/armageddon server uninstall --purge >/dev/null
check "--purge with the typed phrase removes data and the user" bash -c "! docker exec $NAME test -e /var/lib/armageddon && ! docker exec $NAME id armageddon >/dev/null 2>&1"
printf '\n\033[32mALL INSTALL CHECKS PASSED [%s]\033[0m\n' "$IMAGE"
