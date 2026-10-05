# Known limitations, and how to resolve them

Status: **2026-10-05, living document.** It is updated as each post-MVP phase reports. Sections marked *pending* will be filled in when the phase agent that owns them finishes.

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
| git | ≥ 2.42 | Server repos, capture, smart HTTP |
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
git --version                  # want: ≥ 2.42
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

The commands for P6 (privilege boundary) and P8 (SSH endpoint) are *pending* the Phase 1 report.

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
| A9 | **Adversarial boundary tests:** an automated safety check interrupted the agent while it wrote the P6 escape tests | Some P6 checks may be marked "not done here" | The checks are plain allow/deny assertions (for example, as `ws-A`, reading the DB must fail). Run them yourself on a VM; commands are *pending* the Phase 1 report |
| A10 | **Agent usage limits** | Phase agents pause when the account's usage limit is reached and resume after it resets | No action needed. Work in progress is committed and pushed as it goes |

---

## 3. B — Product limits of v0.1

| # | Limitation | Workaround now | Planned fix |
|---|------------|----------------|-------------|
| B1 | **The server runs as root** and drops to workspace users in-process | Run on a dedicated VM | Phase 2: `armageddon helper` as root with only the allowlisted API; the server runs as `armageddon` |
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

---

## 4. C — Open decisions and admin items

| # | Item | Default if you don't decide | Where |
|---|------|------------------------------|-------|
| C1 | **GitGuardian flags** two old MVP commits with throwaway test passwords (now on `main`) | None. Mark them as false positives in the GitGuardian dashboard; both tests now generate random passwords | GitGuardian |
| C2 | Who holds the **release signing key** (D1) | You hold a minisign key offline; CI signs only when the key is supplied as a secret at release time | Phase 4 |
| C3 | **Install URL** (D2) | `install.sh` attached to GitHub Releases | Phase 4 |
| C4 | **Upgrade MVP installs in place** (D3) | Yes: ownership is migrated automatically at startup | Phase 2 |
| C5 | **If P8 fails** (D5) | Document using the system `sshd` with a forced command | Phase 5 |

---

## 5. Per-phase findings

Each phase adds its own limitations here when its PR opens.

- **Phase 1 (validate):** partial so far.
  - P2 cross-platform focus cases: Linux ↔ Linux 7/7 match, 0 silent mismatches. macOS results come from CI.
  - P6 helper core (typed protocol, socket peer check, `openat2`, Compose validator) is built and unit-tested. The end-to-end run, escape checks, verdict and P8 are in progress.
- **Phase 2 (privilege split):** *pending*.
- **Phase 3 (local write mode):**
  - `work remote --restart` only records `runtime.restart_requested`: there are no runtimes to restart until Phase 4.
  - Stopping the server's dev processes on handoff (Q1) kills every process of the workspace user, from the server process. It moves into the helper's `SignalWorkspace` with Phase 2. In development mode (server not root) nothing is stopped.
  - Local commits on a read-only replica are kept as local refs (`refs/armageddon/quarantine/...` in the replica), not uploaded to `checkpoints.git` as §5.5 describes. Working-tree changes are uploaded.
  - macOS: fsnotify uses kqueue (one descriptor per watched directory), not FSEvents; the 30 s full capture is the backstop. Not run on macOS in this phase.
  - The conformance suite (`test/conformance`) runs a small budget on every PR and a large one nightly; see the PR for the budgets run so far.
- **Phase 4 (install and ops):** *pending*.
- **Phase 5 (remote IDE):** *pending*.
