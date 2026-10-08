# Known limitations, and how to resolve them

Status: **2026-10-08, living document.** It now includes the findings of all five post-MVP phases (PRs #7, #9, #10, #11 and #8), of M8 (runtimes and Docker) and of M9 (hardening).

There are three kinds of limitation, and they need different responses:

| Kind | Meaning | What to do |
|------|---------|------------|
| **A. Not verifiable in the cloud sandbox** | The code exists or is planned, but the container it was built in can't exercise it | Run it on your own machine or a VM (§1) |
| **B. Product limits of v0.1** | Deliberate scope cuts, or known gaps in the design | Workaround now; fix planned (milestone given) |
| **C. Open decisions and admin items** | Need the project owner, not code | Decide or act (§4) |

---

## 1. Running it on your own machine

### What you need

| | Requirement | Why |
|---|-------------|-----|
| OS (server side) | **Linux**: a VM or bare metal running Ubuntu 24.04, Debian 12 or Fedora 40. WSL2 is untested | Per-workspace Unix users, cgroups, `openat2` |
| OS (laptop side) | Linux or macOS | The agent and CLI (`login`, `clone`, `follow`) |
| Go | 1.26 (from `go.mod`; the toolchain auto-downloads) | Build |
| git | ≥ 2.39 | Server repos, capture, smart HTTP |
| Privileges | `sudo` / root | The acceptance test creates real OS users (`ws-*`) |
| Node.js and Playwright | Node ≥ 20, `npx playwright install chromium` | Only for the browser UI test |
| Docker | Optional | Only for the Docker deployment and the P6 Docker variant |
| Network | Outbound HTTPS to github.com | The acceptance test clones `octocat/Hello-World` (override with `SOURCE_URL=`) |

**Use a throwaway VM, not your daily machine.** The tests run as root, create system users named `ws-…`, and write to `/srv`. They don't remove those users afterwards (see B9).

A quick way to get a suitable VM:

```sh
# Multipass (macOS, Linux or Windows host)
multipass launch 24.04 --name arm --cpus 4 --memory 4G --disk 20G
multipass shell arm
sudo apt-get update && sudo apt-get install -y git build-essential
# then install Go 1.26 from https://go.dev/dl/
```

### Check the host first

```sh
stat -fc %T /sys/fs/cgroup     # want: cgroup2fs   (tmpfs means a hybrid/v1 host: limits can't be fully tested, see A1)
uname -r                       # want: ≥ 5.6 for openat2 (A2)
git --version                  # want: ≥ 2.39
```

### Build and test

```sh
git clone https://github.com/callmehalpha/Armageddon-.git && cd Armageddon-
git checkout <branch>          # main, or a phase branch, e.g. phase2/privilege-split

go test ./...                                  # unit tests
go build -o armageddon ./cmd/armageddon
sudo install -m 0755 armageddon /usr/local/bin/armageddon
sudo mkdir -p /srv && sudo chmod 755 /srv
sudo test/e2e/mvp.sh /usr/local/bin/armageddon # acceptance test: expect "ALL MVP ACCEPTANCE CHECKS PASSED"
sudo test/integration/escape.sh /usr/local/bin/armageddon  # privilege boundary (phase 2 on): expect "ALL ESCAPE-MATRIX CHECKS PASSED"
```

Optional browser test, run against a fresh server. The setup URL is printed by `server run`:

```sh
npm i playwright && npx playwright install chromium
node test/e2e/ui.mjs <setup-url>
```

### Prototypes

The prototypes live in a separate Go module:

```sh
cd prototypes
go run ./p2-roundtrip -seqs 200            # P2 capture→apply fuzzer (raise -seqs for a long run)
go run ./p2-roundtrip -mode xplat-local -dir /tmp/p2x   # P2 cross-platform focus cases on one machine (phase1/validate branch)
```

To check the P2 cross-platform case between a Linux box and a Mac, export on one machine and import on the other. The `-mode` options exist on the `phase1/validate` branch until it merges.

```sh
go run ./p2-roundtrip -mode export -dir /tmp/p2x      # on machine 1; copy /tmp/p2x to machine 2
go run ./p2-roundtrip -mode import -dir /tmp/p2x      # on machine 2; reports match / refused / mismatch per step
```

Prototype runs for P6 (privilege boundary) and P8 (SSH endpoint), on the `phase1/validate` branch, as root:

```sh
sudo prototypes/p6-privilege-boundary/run-e2e.sh   # helper end to end + spawn-cost benchmark
sudo prototypes/p8-ssh-endpoint/run-e2e.sh         # SSH endpoint against the OpenSSH client
```

### Test suites added by the phases

These live on the phase branches until they merge. Run each as root on a VM:

| Branch | Command | Checks |
|---|---|---|
| `phase2/privilege-split` | `sudo test/integration/escape.sh /usr/local/bin/armageddon` | Privilege-boundary escape tests (70 checks with Phase 5) |
| `phase3/local-write` | `sudo test/e2e/write.sh /usr/local/bin/armageddon` | Local write mode and failure scenarios (59 checks) |
| `phase3/local-write` | `go test ./test/conformance/...` | The P4 failure model against the real server (see the PR for flags) |
| `phase4/install-ops` | `sudo test/e2e/ops.sh /usr/local/bin/armageddon` | TLS pinning, doctor, backup and restore |
| `phase4/install-ops` | `test/install/container.sh ubuntu:24.04 …` | Real systemd install, update, rollback (needs Docker) |
| `phase5/remote-ide` | `sudo test/e2e/ide.sh /usr/local/bin/armageddon ./fake-code-server` | code-server proxy, SSH, credentials, escapes from a second workspace (needs `openssh-client`; helper as root, server as `armageddon`) |
| M8 (runtimes and Docker) | `sudo test/e2e/runtime.sh /usr/local/bin/armageddon` | Next.js, PHP and Laravel runtimes, the port proxy, Compose refusals, Postgres and Redis (needs network; PHP, Composer and a running Docker daemon for their parts) |

---

## 2. A — Not verifiable in the cloud sandbox

| # | Limitation | Effect | How to resolve |
|---|------------|--------|----------------|
| A1 | **The cgroup hierarchy is hybrid.** cgroup v2 offers only `hugetlb`; memory and pids limits work only in v1 | CPU, memory and pids limits per workspace (helper `SetWorkspaceLimits`, P6 check E9) can't be verified as the contract specifies | Run on a host where `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs` (Ubuntu 22.04+, Debian 12, Fedora 31+). For delegation, run the helper under systemd with `Delegate=yes` (Phase 4 units). Then re-run the P6 limits check |
| A2 | `openat2` exists here (kernel 6.18), but older kernels lack it | Older hosts would fail `PrepareWorkspaceDirs` | Use kernel ≥ 5.6 (all recommended distributions qualify). `armageddon doctor` reports it (Phase 4) |
| A3 | **Docker daemon not running** in the sandbox | P6 Docker variant and the Phase 4 Compose deployment are unverified | On a machine with Docker: `docker compose -f deploy/compose.yaml up` (Phase 4), then run the acceptance test against it |
| A4 | **No Mac available** | P2 on APFS (case-insensitive file system, NFD names) runs only on GitHub's `macos-latest` runner. The macOS agent pieces (FSEvents watcher, launchd unit, Keychain) are untested on real hardware | The CI workflow `.github/workflows/p2-macos.yml` covers the file-system question. On your Mac, run the P2 commands in §1 and `armageddon login/clone/follow` against a Linux server |
| A5 | **No IDE clients** (VS Code Remote-SSH, JetBrains Gateway) | P8 SSH verdict is limited to the OpenSSH client (exec, PTY, SFTP, port forwarding) | Manual check: `armageddon ssh-config`, then connect VS Code Remote-SSH and open a folder; also try Gateway. Record the result in `P8-ssh-endpoint.md` |
| A6 | **No public domain or ports 80/443** | ACME (Let's Encrypt) can't be exercised; only the config wiring and the IP-only self-signed mode are tested | On a VPS with a DNS name pointing at it: `armageddon server init --domain … --acme-email … --acme-staging`, then repeat without the staging flag |
| A7 | **Can't install real systemd units** in the sandbox | The installer and services are tested only in containers or with a temp `--prefix` | Fresh Debian 12, Ubuntu 24.04 and Fedora 40 VMs: run `deploy/install.sh`, reboot, and check that both services come back up |
| A8 | **Real code-server download** may be blocked by the sandbox proxy | Phase 5 tests use a fake code-server; the real one is verified only if the download worked | `armageddon server components install code-server`, open the workspace IDE in a browser, edit a file, and check that the checkpoint appears |
| A9 | **Escape tests:** an automated safety check interrupted the Phase 1 agent on the prototype's escape tests | Covered instead by Phase 2's `test/integration/escape.sh`: 62/62 pass against the real helper. Not covered: CPU and memory limits (A1) | Run `sudo test/integration/escape.sh` on a VM with cgroup v2 |
| A10 | **Agent usage limits** | Phase agents pause when the account's usage limit is reached and resume after it resets | No action needed. Work in progress is committed and pushed as it goes |

---

## 3. B — Product limits of v0.1

| # | Limitation | Workaround now | Planned fix |
|---|------------|----------------|-------------|
| B1 | ~~**The server runs as root**~~ Fixed in Phase 2: `armageddon helper` runs as root with only the allowlisted API; the server runs as `armageddon` | — | Done (M3.1); merges after the P6 verdict |
| B2 | **Local write mode needs a running agent** (`armageddon agent run` or `follow`); `work local` refuses otherwise. Takeover is always explicit (Q2) | `armageddon agent install` writes a systemd --user unit or launchd plist; enable it once | Phase 3 (done in its PR) |
| B3 | **No installer, ACME, update, backup or restore** | `server init/run` with `--tls-cert/--tls-key` or a reverse proxy; back up `/var/lib/armageddon` while the server is stopped | Phase 4 |
| B4 | **No code-server or SSH endpoint** | Browser terminal | Phase 5. SSH stays off by default until P8 passes |
| B5 | **Server-seat capture is polled every 2 s** (devices use a watcher with a 30 s full-capture backstop since Phase 3) | Fine up to about 50k files | Watcher-based capture ⟨P-12⟩ after P3 passes |
| B6 | **Large repositories (200k+ files):** capture is marginal (896 ms p95 against a 1 s target) | Exclude generated directories; keep workspaces under about 50k tracked files | Scoped capture ⟨P-12⟩; other options in P1 F2 |
| B7 | **Not supported:** Windows, Git submodules, Git LFS content, concurrent writers, repositories over about 5 GB | LFS pointers sync, and each seat fetches LFS content itself; use separate workspaces per writer | Post-v0.1 (contract §11) |
| B8 | **P2 edge case:** one index-only mismatch caused by Git itself (a file/directory conflict in the index). Working-tree data is not affected | None needed | Tracked in `P2-roundtrip.md` |
| B9 | **The acceptance test leaves `ws-*` users behind** | Use a throwaway VM, or clean up: `getent passwd \| cut -d: -f1 \| grep '^ws-' \| xargs -r -n1 sudo userdel` | Add cleanup to `mvp.sh` |
| B10 | **Server-seat trash recording for local Git is best effort** (P-13): a workspace can edit its own `repo.git/config` | Device pushes are always protected; server-seat trash refs cover normal use | By design (contract §5.4) |
| B11 | **Follow uses long polling,** not WebSockets | None needed | WebSocket event bus after v0.1 (decision D4) |
| B12 | ~~**macOS case-only and Unicode renames**~~ Fixed: capture (full and scoped) drops index entries not spelled as `readdir()` reports them, and apply refuses a checkpoint whose paths collide on the replica with an explicit collision error | — | Done (⟨P-20⟩, ⟨P-21⟩, `internal/treesync/gitshadow/fold.go`); tested on the macOS CI leg |
| B13 | **The IDE is served on the Armageddon origin** (Phase 5): JavaScript in a workspace's IDE, such as a malicious extension, runs with the viewer's Armageddon session | Single-user servers, or trusted members only | Serve code-server from a separate origin (wildcard subdomain plus a one-time ticket) before multi-user use |
| B14 | **No network isolation between workspaces** (P8 ⟨P-18⟩, Phase 5): SSH forwards and workspace processes share the host loopback. Forwards are dialled by the server process, so they also reach loopback services of the server and of other workspaces | Trusted members only | A per-workspace network namespace |
| B15 | **Stored Git credentials are reachable by every member of the workspace** while the owner's session is open, because they share the OS user (contract §7.5). Other workspaces' users are refused by `SO_PEERCRED` on the credential and agent sockets | Store credentials only in workspaces you don't share | Inherent in v0.1; per-user seats later |
| B16 | **SSH membership is checked at connect time,** so removing a member doesn't drop live sessions (revoking a device does). Logging out doesn't close open IDE WebSockets | Revoke the device | Session invalidation on membership change |
| B17 | **The helper adds about 3.5 ms per spawned process** (P6), and each workspace command costs one extra shim process (Phase 2) | None needed for interactive use | ⟨P-16⟩: a minimal non-Go trampoline (about 2.3 ms) |
| B18 | **The encryption scheme for stored Git credentials differs:** AES-256-GCM in the code, XChaCha20-Poly1305 in contract §7.5 | None needed; both are sound | Align the contract or the code |
| B19 | **Releases are unsigned** until a signing key is configured (C2), and none is published yet | Build from source. `install.sh` and `server update` refuse unsigned releases unless given `--allow-unsigned`; `install.sh` checks signatures with `minisign` or OpenSSL, never with the downloaded binary (M9.2) | Run `armageddon release keygen` and set the `MINISIGN_SECRET_KEY` and `MINISIGN_PASSWORD` secrets |
| B20 | **Docker Compose:** workspace users are recreated in creation order on each container start, so their IDs only match the volume while that order is stable | Don't delete workspace users by hand inside the container | Persist the uid map on the volume |
| B21 | **Workspace ports are served on the Armageddon origin** (M8.6), like the IDE (B13): JavaScript of an app in the workspace runs with the viewer's Armageddon session. Apps also see a path prefix (`/api/workspaces/<id>/ports/<port>/`), so absolute asset URLs such as Next.js's `/_next/…` don't load through the proxy | Trusted members only. For full apps, forward the port over SSH (`ssh -L 3000:localhost:3000 ws-<slug>@server`) or set the framework's base path | A separate origin per port (wildcard subdomain plus a one-time ticket), with B13 |
| B22 | **Dev servers that listen on all interfaces** (Next.js does by default) are reachable from the network directly, without Armageddon's authentication, if the host firewall lets the port through. Compose ports are always bound to 127.0.0.1 | Open only 80/443 and SSH in the host firewall | A per-workspace network namespace (with B14) |
| B23 | **Compose is deliberately narrow:** bind mounts and image builds are refused (both would be read by root and are open to a symlink race), as are host namespaces, devices, extra capabilities and external volumes or networks; containers have only the resource limits the file sets | Use named volumes and prebuilt images (for example an image with your seed SQL built elsewhere) | Bind mounts through a helper-held directory descriptor; builds in a rootless builder |
| B24 | **Runtime process state lives in the server's memory:** a server restart ends the install and the dev server (they are helper children) and forgets their output. Compose services keep running | `armageddon runtime start` again after a restart | Persist the desired state (`runtime_instances`) and restart on boot |
| B25 | **PHP comes from the server:** a version the server lacks is reported, not installed. Bun is not supported (npm is used instead). A Laravel app's asset build (`npm run dev`) is not started | Install the PHP version on the server; run `npm run dev` in the terminal | Per-workspace PHP (static builds) if there is demand |
| B26 | **`work remote --restart` restarts only the dev server** that the handoff stopped, during this server process's lifetime; an interrupted install is not repeated | `armageddon runtime install` again | With B24 |
| B27 | **Files over `sync.max_file_size` (50 MiB by default) are not synced** (F17). A file that grows past the limit while it is being captured is caught on the same capture and keeps its previous version | `status` lists them; raise the limit in `.armageddon/sync.yaml` | Git LFS integration after v0.1 |
| B28 | **`workspace seed --from-replica` rebuilds one workspace;** users, members and other workspaces come only from a backup (F9) | Keep server backups (`server backup`) as well as replicas | — |
| B29 | **Accepted low findings of the security review** (findings 7, 8, 10 and 12): packs are buffered in memory, `server update` relies on the signature rather than a downgrade check, revoking a device needs no password re-entry, Compose uses root's Docker config | See `docs/security-review-v0.1.md` | Post-v0.1 |

---

## 4. C — Open decisions and admin items

| # | Item | Default if you don't decide | Where |
|---|------|------------------------------|-------|
| C1 | **GitGuardian flags:** two old MVP commits, and Phase 4 commit `9e1ca2e`, contain throwaway test passwords | None. Mark them as false positives in the GitGuardian dashboard; all tests now generate random passwords | GitGuardian |
| C2 | Who holds the **release signing key** (D1) | You hold a minisign key offline; CI signs only when the key is supplied as a secret at release time | Phase 4 |
| C3 | **Install URL** (D2) | `install.sh` attached to GitHub Releases | Phase 4 |
| C4 | **Upgrade MVP installs in place** (D3) | Yes: ownership is migrated automatically at startup | Phase 2 |
| C5 | **If P8 fails** (D5) | Document using the system `sshd` with a forced command | Phase 5 |

---

## 5. Per-phase findings

- **M9 (hardening):**
  - Disaster suite: `test/e2e/disaster.sh` covers F1, F5–F10, F12–F15 and F17 (67 checks, nothing skipped); `write.sh` covers F2–F4, F11 and F18; the lifecycle tests cover F16. See `docs/disaster-suite.md`.
  - New commands: `workspace seed --from-replica` (F9 b), `workspace repair --from-device` (F10), `git trash` (F13). New server settings: `min_free_percent` (F8, default 5) and `fsck_interval_s` (F10, default daily).
  - Security review: `docs/security-review-v0.1.md`. No open high or critical findings.
  - Performance at 50k files: capture p95 273 ms, cold capture 11.1 s, both within P1 (`docs/performance.md`).
  - Not done here: M9.5 (a week of dogfooding) is for the owner, as are C1–C5.

- **Phase 1 (validate, #7):**
  - **P2 on macOS:** found a real capture bug (B12), fixed in the prototype. Afterwards: macOS fuzzing 30/30, focus cases 7/7 in both directions, genuine collisions refused. The full 10,000-sequence run on macOS has not been done yet (start it with `workflow_dispatch`).
  - **P6:** the helper works end to end and costs +3.5 ms per spawn. Escape checks: see A9.
  - **P8:** 15/15 checks pass with OpenSSH. Real VS Code and JetBrains are untested (A5).
  - **Proposed contract changes:** ⟨P-15⟩–⟨P-21⟩, for review.
- **Phase 2 (privilege split, #9):**
  - cgroup v2 limits ran only in their fallback mode (A1).
  - The server has no `fsck` path in v0.1, so the escape test E5 covers push, fetch and capture.
  - The upgrade test fakes the MVP layout with `chown`.
  - Each workspace command costs one extra process (the `helper-exec` shim) and one helper round trip; not yet measured.
  - Other e2e scripts must use the two-process setup, because `server run` as root refuses to start without a helper unless given `--dev`.
- **Phase 3 (local write mode, #10):**
  - `work remote --restart` only records `runtime.restart_requested`: there are no runtimes to restart until Phase 4.
  - Stopping the server's dev processes on handoff (Q1) goes through the helper's `SignalWorkspace(all)`: SIGTERM, 3 s grace, SIGKILL. In development mode (`--dev`) only helper-spawned processes are signalled.
  - Local commits on a read-only replica are kept as local refs (`refs/armageddon/quarantine/...` in the replica), not uploaded to `checkpoints.git` as §5.5 describes. Working-tree changes are uploaded.
  - macOS: fsnotify uses kqueue (one descriptor per watched directory), not FSEvents; the 30 s full capture is the backstop. Not run on macOS in this phase.
  - The conformance suite (`test/conformance`) runs a small budget on every PR and a large one nightly; see the PR for the budgets run so far.
- **Phase 4 (install and ops, #11):**
  - Debian 12, Fedora 40 and the Debian-based image are untested here, because their package mirrors are blocked; CI runs them.
  - A real VPS install and ACME against a real CA are manual checks (A6, A7).
  - The release pin in `deploy/code-server.json` and Phase 5's pin in `internal/components` are both code-server 4.118.0, with the same checksums; `TestReleasePinMatches` fails if they drift apart. Because the pin file carries the checksums, the release build checks the downloads against the repository.
  - The helper unit doesn't restrict its network, because that would also cut off the workspace processes it spawns.
  - Fixed an MVP bug: empty workspaces failed `git fsck`. `doctor --repair` fixes existing ones.
- **M8 (runtimes and Docker):**
  - Verified here: the providers' unit tests (version ranges, detection, a checked Node download from a fake mirror, a tampered tarball refused, Laravel install steps), a real Node.js 20 download from nodejs.org running a dev server, the API end to end with PHP's built-in server (install, start, health, the proxy refusing anonymous and non-member callers, a handoff stopping the dev server, the restart), and the Compose checks against `docker compose config` output.
  - Not verifiable here: `docker compose up` (no Docker daemon in the sandbox, A3) and a full Laravel install (GitHub downloads of Composer packages are blocked). Both run in CI (`e2e-runtime`).
  - The root-only acceptance script `test/e2e/runtime.sh` was not run in the sandbox; CI runs it.
  - The helper gained `ComposeUp/Down/Ps` (contract §2.5, ⟨P-22⟩). There is no `ConfigurePortProxy` (⟨P-24⟩).
- **Phase 5 (remote IDE, #8):**
  - See B13–B16 and B18.
  - Integrated with the privilege split: code-server, SSH sessions and SFTP are started by the helper as the workspace user (`code-server`, `ssh-session` and, for SSH with a terminal, `pty-shell`). `ide.sh` runs the helper as root and the server as `armageddon`, and attacks the new sockets from a second workspace.
  - The helper only starts the code-server it resolves itself, and only if the program, its symlink target and every directory above them belong to root or `armageddon` and are not group/other-writable. It checks the entry point only, not the files it loads (the real code-server's `lib/node` and `out/`), so install code-server as one root-owned tree (the component installer does). `code_server.path` must be absolute, and code-server is searched on the fixed workspace `PATH`, not the server's.
  - The code-server socket is in the workspace's `run/` (owned by `ws-<id>`, group `armageddon`, mode 2750, socket 0660). The workspace user can still replace it; the server's `SO_PEERCRED` check (the supervised process or its child, as the workspace uid) refuses that, and the IDE then answers 502 until it restarts.
  - Credential and agent sockets are no longer in the workspace: they are in `/run/armageddon/seat/` (0711, random names), mode 0666, served only to the workspace's uid.
  - SSH is off by default until P8 is checked with real IDEs.
  - code-server has no access to the Git credential sockets, because one instance is shared by every member.
  - The pinned code-server checksums were computed from downloads; upstream publishes no checksum file to cross-check against.
