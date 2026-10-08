# Operating an Armageddon server

How to install, secure, update, back up and check a server (contract §9,
plan M5). Commands run as root unless noted.

## Install

```sh
curl -fsSL https://github.com/callmehalpha/Armageddon-/releases/latest/download/install.sh | sudo sh
# offline: copy armageddon-<v>-linux-<arch>.tar and install.sh, then
sudo sh install.sh --bundle armageddon-<v>-linux-<arch>.tar
```

The installer supports Debian 12, Ubuntu 24.04 and Fedora 40 on amd64 and
arm64. It checks the kernel, RAM (≥ 1 GB) and disk (≥ 10 GB), installs
git ≥ 2.39 if needed (from the distribution), verifies the
release, and lays out:

```
/opt/armageddon/versions/<v>/armageddon   one directory per version
/opt/armageddon/current → versions/<v>
/usr/local/bin/armageddon → /opt/armageddon/current/armageddon
/etc/armageddon/server.yaml → /var/lib/armageddon/server.json
/var/lib/armageddon/                      data (contract §2.4)
```

**Release verification.** The installer checks every artefact's sha256
against `manifest.json` and the manifest's minisign signature against the
public key stamped into the published `install.sh`. It uses the `minisign`
tool if installed, otherwise OpenSSL (1.1.1 or newer, installed from the
distribution if missing); the downloaded
binary never verifies itself. With neither tool, and for unsigned
releases, it refuses unless you pass `--allow-unsigned`.

**Services.** If the release's binary has `armageddon helper` (Phase 2),
the installer installs `armageddon-helper.service` (root, limited
capability set) and `armageddon.service` (`User=armageddon`,
`NoNewPrivileges`, `ProtectSystem=strict`). Otherwise it installs a single
`armageddon.service` running as root and says so; re-running the
installer after an upgrade to a release with the helper switches layouts.

Re-running the installer is safe. To change versions, use `server update`.

## TLS: domain or IP-only

The installer starts the server on `127.0.0.1:8080` only. Then choose:

```sh
sudo armageddon server init --domain dev.example.com --acme-email you@example.com
sudo armageddon server init --ip-only            # self-signed, prints a SHA-256 fingerprint
sudo systemctl restart armageddon
journalctl -u armageddon | grep setup           # one-time URL for the first admin
```

*ACME* uses Let's Encrypt through certmagic: HTTP-01 on port 80 and
TLS-ALPN-01 on 443. DNS must point at the server and both ports must be
reachable. `--acme-staging` uses the staging CA.

*IP-only* generates a certificate under `keys/`. `armageddon login` shows
its fingerprint; compare it with the one `server init` printed (or
`armageddon server fingerprint`). The device pins it, and Git uses the
pinned certificate as its trust anchor. If the certificate later changes,
the device refuses to connect with a clear error. After a deliberate
change (`server init --ip-only --new-cert`), re-pair with
`armageddon login <url> --fingerprint <new>`.

### Manual check: ACME against the staging CA

This needs a VPS with a public DNS name; it cannot run in CI.

1. Point `staging-test.<your domain>` at the VPS; open ports 80 and 443.
2. `sudo armageddon server init --domain staging-test.<your domain> --acme-email you@<your domain> --acme-staging`
3. `sudo systemctl restart armageddon`, then watch `journalctl -u armageddon -f`
   for `certificate obtained successfully`.
4. `curl -vk https://staging-test.<your domain>/healthz` shows an issuer
   of `(STAGING) ...` and answers `ok`; `curl http://…/x` redirects to https.
5. Re-run init without `--acme-staging` for a real certificate
   (certificates live in `/var/lib/armageddon/keys/acme`).

## Update and rollback

```sh
sudo armageddon server update               # latest release
sudo armageddon server update --to 0.2.0
sudo armageddon server update --bundle armageddon-0.2.0-linux-amd64.tar
sudo armageddon server rollback             # previous version + its pre-update database
```

`update` verifies the release, backs the database up with `VACUUM INTO`
(`backups/pre-update-<from>-to-<to>-<time>.db`), stops the services,
switches `current`, runs the new binary's migrations, starts the services
and waits up to 60 s for `/healthz`. If any of that fails, it switches
back, restores the database backup and starts the old version again.

`rollback` restores the pre-update database: changes made since the
update are lost. `--keep-db` keeps the current database, which is only
safe when the update did not change the schema.

## Backup and restore

```sh
sudo ARMAGEDDON_BACKUP_PASSPHRASE='…' armageddon server backup --to /srv/backups
sudo armageddon server restore /srv/backups/armageddon-backup-<time> --passphrase-file pw.txt   # onto a fresh install
```

A backup holds a `VACUUM INTO` snapshot of the database, and per workspace
a `git bundle` of `repo.git` (branches, tags and trash refs) and of
`checkpoints.git`, plus the configuration. With a passphrase, `keys/`
(TLS and ACME keys) is included, encrypted with scrypt and
XChaCha20-Poly1305. Each workspace's commits pause only while its bundles
are written. Every file has a sha256 in `backup.json`.

Restore needs an empty data directory. It rebuilds each workspace at the
database's current checkpoint, including uncommitted, staged and ignored
`.env` work, and gives every lease to the server at a new epoch, so a
device that held the lease must take it again. Workspace home directories
are not in the backup; they are recreated with defaults.

## Doctor

```sh
sudo armageddon doctor            # add --repair to fix what is safe to fix, --json for scripts
```

It checks free disk, data-directory permissions, helper reachability
(skipped without a helper), the git version, cgroup v2, `openat2`, a
`git fsck` of a sample of workspaces, and whether each workspace's
`refs/checkpoints/current` matches the database. Every finding has a
remedy. `--repair` rewrites checkpoint refs from the database and writes
Git's empty tree object into workspaces created by v0.1.0-mvp, which
referenced it without storing it.

It also checks the clock (NTP synchronisation, contract §10 F15) and lists
DEGRADED workspaces. `--repair` returns a workspace degraded by a full disk
(`disk_full`, F8) to READY once free space is back above the floor
(`min_free_percent`, default 5), and one degraded by a corrupt repository
(`repo_corrupt`, F10) once its `git fsck` is clean again. To get missing
objects back, run `armageddon workspace repair --from-device` on a laptop
that has a replica. See [troubleshooting](troubleshooting.md).

## Uninstall

```sh
sudo armageddon server uninstall            # services and binaries; data stays
sudo armageddon server uninstall --purge    # also data, config and users; type "delete /var/lib/armageddon"
```

## Docker Compose

```sh
docker compose -f deploy/compose.yaml up -d --build
docker compose -f deploy/compose.yaml logs armageddon | grep setup
```

One image with git; `/var/lib/armageddon` on the `armageddon-data`
volume; configuration from `ARMAGEDDON_*` environment variables on first
start (see `deploy/docker-entrypoint.sh`). Workspace users are created
inside the container. The Docker socket is not mounted. Update by
rebuilding or pulling the image.

Known limitation: workspace users live in the container's `/etc/passwd`
and are recreated on every start, in creation order, so their UIDs match
the files on the volume only while that order is stable. Phase 2's helper
should give workspace users UIDs derived from the workspace.

## Tests

| What | Command | Where it runs |
|------|---------|---------------|
| Unit tests (release, TLS, pinning, backup, doctor, update/rollback with an injected migration failure) | `go test ./...` | CI `test` (the backup round trip runs only as non-root) |
| Operations end to end (IP-only TLS, pinning, doctor, backup → restore) | `sudo PORT=8104 test/e2e/ops.sh path/to/armageddon` | CI `e2e` |
| Installer without touching the machine | `test/install/prefix.sh` | CI `installer` |
| Installer, update, rollback, uninstall on systemd | `test/install/container.sh IMAGE R1 R2` | CI `install` (Debian 12, Ubuntu 24.04, Fedora 40) |
| Compose, plus `mvp.sh` inside the image | `test/e2e/compose.sh` | CI `compose` |

## Manual VPS checks (per release)

CI cannot cover these:

1. A fresh Debian 12, Ubuntu 24.04 and Fedora 40 VPS, each with 1 GB RAM:
   `curl … | sudo sh` without `--skip-requirements`, the requirement
   checks pass, and git ≥ 2.39 comes from the distribution.
2. ACME against the staging CA, then production (see above).
3. IP-only mode reached from a laptop over the internet: `armageddon
   login` shows the fingerprint, `clone` works, and replacing the
   certificate is refused.
4. `server update` from the previous release with real workspaces, then
   `server rollback`.
5. Backup on one VPS, restore on a fresh one, then continue work from a
   browser (the Alpha 1 exit test).
