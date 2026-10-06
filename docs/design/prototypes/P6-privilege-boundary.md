# P6: Privilege boundary (root helper, unprivileged server, one user per workspace)

**Verdict: PARTIAL. The mechanism works; the escape matrix was not run here.**

- The §2.5 helper API is implementable as specified. Run end to end as root with a separate unprivileged server user, PTY shells, Git service processes and seat capture all work through the helper. Every spawned process runs as `ws-<id>` with `NoNewPrivs: 1`, no capabilities and no supplementary groups.
- The request decoder is fuzzed: a closed operation set, with no request that runs a program as root.
- The helper costs **about 3.5 ms per spawn**. Almost all of it is the Go trampoline that sets `NO_NEW_PRIVS`, not the socket round trip (⟨P-16⟩).
- **Not done here:**
  - the escape matrix E1–E7 and E9 (the deny checks run from a workspace shell);
  - code-server;
  - a full smart-HTTP push/fetch through the front end;
  - the Docker-deployment variant.

  The §11 criterion "every attempt fails" is therefore **not yet demonstrated**.

Code: [`prototypes/p6-privilege-boundary`](../../../prototypes/p6-privilege-boundary/), run with `sudo prototypes/p6-privilege-boundary/run-e2e.sh [iterations]`.

## What was built

| Part | Where | Notes |
|------|-------|-------|
| Wire protocol | `proto/` | 4-byte length prefix plus a JSON body. Six operations; an unknown op or unknown field is rejected. Workspace ID `[a-z0-9]{8,24}` is the only source of user names and paths. Environment variables are allowlisted (`TERM`, `LANG`, `GIT_CONFIG_*`, …; no `LD_*`, no other `GIT_*`) |
| Helper (root) | `helper/` | Unix socket, mode 0600, `chown` to the server user. **`SO_PEERCRED`**: any peer uid other than the server's is rejected before a request is read |
| `CreateWorkspaceUser` / `DeleteWorkspaceUser` | | `useradd --system`, shell `nologin`, no home creation, no groups. Delete kills the workspace's processes first |
| `PrepareWorkspaceDirs` | | The workspace dir is root-owned 0755. `repo.git`, `tree`, `home` are opened with **`openat2(RESOLVE_BENEATH\|RESOLVE_NO_SYMLINKS)`**, then `fchown`ed and `fchmod`ed **by descriptor** (0700). A symlink in the way gives ELOOP rather than being followed |
| `SpawnInWorkspace` | | Kinds `pty-shell`, `git-service`, `runtime-command`. Stdio arrives as **`SCM_RIGHTS`** descriptors. The child gets `Credential{ws uid, gid, Groups: []}` and `Setsid`, is placed in the cgroup v2 at clone time (`UseCgroupFD`) when one is delegated, then execs the **stub**. The stub sets `PR_SET_NO_NEW_PRIVS`, claims the PTY as controlling tty if asked, and execs argv |
| `SignalWorkspace` | | By handle or `all`. Uses the cgroup v2 `cgroup.kill`/`cgroup.procs` when available, plus the tracked process groups |
| `SetWorkspaceLimits` | | `memory.max` (+ `memory.swap.max=0`), `pids.max`, `cpu.weight` on cgroup v2. Returns an explicit error naming what is missing when v2 is not delegated |
| Server half | `client/`, `main.go demo` | Runs as the unprivileged user and holds no privilege. It allocates the PTY itself and passes the slave, so a resize is a `TIOCSWINSZ` on the master the server already holds: no helper operation needed |
| Compose validator (E8) | `compose/` | Parser only. Covered under E8 below |

## Results

Environment: Linux 6.18 x86-64, 4 vCPU, git 2.43.0, Go 1.26, running as root in a container. The server user and workspace user were created fresh for the run.

### Functional (helper end to end)

| Check | Result |
|-------|--------|
| Helper socket is mode 0600, owned by the server user | PASS |
| `CreateWorkspaceUser`, `PrepareWorkspaceDirs` from the unprivileged server | PASS |
| `runtime-command` runs as the workspace uid (not the server uid, not 0) | PASS |
| Spawned process: `NoNewPrivs: 1`, `CapEff: 0`, `Groups:` empty | PASS |
| `pty-shell`: has a controlling tty, runs as the workspace uid | PASS |
| `git-service`: `upload-pack --stateless-rpc --advertise-refs` on `repo.git` | PASS |
| Seat capture: `pack-objects` as `ws-<id>` piped into `index-pack` run by the server user in its own `checkpoints.git` | PASS |

### Spawn cost: helper vs direct `setuid` spawn

n = 100 each. "Direct" is what the MVP does today: a root process `exec`s with `SysProcAttr.Credential`.

| Operation | Direct p50 / p95 | Through helper p50 / p95 | Added p50 |
|-----------|-----------------:|-------------------------:|----------:|
| Git request (`upload-pack --advertise-refs`) | 1.69 / 2.39 ms | 5.20 / 7.03 ms | +3.5 ms |
| Capture (`pack-objects` → `index-pack`) | 6.38 / 7.75 ms | 10.35 / 12.91 ms | +4.0 ms |
| Spawn `/bin/true` | 1.21 / 4.36 ms | 4.85 / 5.69 ms | +3.6 ms |

Where the time goes (n = 200, `/bin/true`, from root):

| Path | p50 |
|------|----:|
| bare exec | 1.09 ms |
| `setpriv --no-new-privs` (C, as a trampoline) | 2.34 ms |
| the Go stub | 4.10 ms |

So about 3 ms of the overhead is the Go runtime starting up in the stub, and well under 1 ms is the socket round trip with descriptor passing.

For a Git request that is +3.5 ms on a round trip that is dominated by network latency, which is acceptable. For a capture loop running every few seconds it is negligible next to P1's capture times (tens to hundreds of ms).

### E8: Compose rejection (parser only)

PASS on the cases tested (`compose/compose_test.go`). Rejected:
- `privileged: true`, `cap_add`;
- host `network_mode`/`pid`/`ipc`;
- `devices`;
- bind mounts outside the workspace dir (absolute, or `..` relative), in both short and long syntax;
- the Docker socket in both syntaxes;
- an unparseable file (fails closed).

Accepted: relative binds inside the workspace, named volumes, bridge networking.

Not handled, and needed before `ComposeUp` exists:
- variable interpolation (`${HOME}:/h`);
- `extends`/`include`;
- `env_file`, `secrets`/`configs` with `file:` sources;
- `build.context` outside the workspace;
- `userns_mode: host`, `security_opt` (`seccomp=unconfined`), `cgroup_parent`;
- top-level volumes with `driver_opts` that bind host paths.

The robust approach is to validate the output of `docker compose config` (fully resolved), not the raw file.

### Escape matrix (E1–E7, E9): not done here

The plan was to write these as plain allow/deny assertions in the style of `test/e2e/mvp.sh` (`runuser -u ws-A -- …` expecting failure, or a uid marker for E5). This part was **not completed in this session** and is open. What exists today:

- **E1/E2 (DB, keys, `checkpoints.git`, workspace B):** not run against the P6 helper. The MVP acceptance test already asserts the equivalent for the MVP's in-process stand-in (`test/e2e/mvp.sh`: the workspace user cannot read the DB, `checkpoints.git`, or another workspace's tree).
- **E3 (signals), E4 (socket with loosened mode), E5 (planted Git config runs only as `ws-A`), E6 (symlink swap), E7 (Docker socket):** not run. The defences are implemented:
  - E4: the `SO_PEERCRED` uid check is independent of the socket mode;
  - E5: every Git command on `repo.git`/`tree` runs through `git-service` as `ws-<id>`, and the server's only Git process (`index-pack`) uses its own `--git-dir` and never reads `repo.git`;
  - E6: `openat2` plus `fchown` by descriptor.

  None of them has been shown to hold by a test.
- **E9 (cgroup limits):** **not testable here.** See below.

## Behaviour when kernel features are missing

| Feature | This host | Behaviour implemented | Recommended |
|---------|-----------|-----------------------|-------------|
| cgroup v2 with memory+pids | **Absent.** Hybrid hierarchy: v2 is mounted at `/sys/fs/cgroup/unified` with controllers = `hugetlb` only; memory/pids/cpu are bound to v1 | Spawn works without a cgroup; `SetWorkspaceLimits` returns `cgroup v2 unavailable: …` | Run without limits, with a `doctor` warning (limits are resource fairness, not the security boundary). `SignalWorkspace(all)` falls back to signalling **by uid**, which is unique per workspace (⟨P-17⟩) |
| cgroup v1 pids/memory | Writable | Not used (the contract is v2-only) | A v1 demo was possible but was not run, so E9 has no result |
| `openat2` (Linux ≥ 5.6) | Present | `PrepareWorkspaceDirs` requires it | **Refuse to start** the helper if `openat2` returns ENOSYS. Do not fall back to path-based `chown`, which reopens E6 (⟨P-17⟩). Not exercised here |

## Docker-deployment variant: not run

`dockerd` is installed in this container but not running. It was deliberately not started, to avoid interfering with other agents working in the same container.

To run it:
1. Build an image with Go, git and util-linux.
2. Run it with
   ```
   docker run --rm --cgroupns=private \
     --cap-drop ALL \
     --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
     --cap-add SETUID --cap-add SETGID --cap-add KILL \
     -v "$PWD:/src" IMAGE /src/prototypes/p6-privilege-boundary/run-e2e.sh
   ```
3. Repeat with cgroup v2 delegated into the container (a host with a unified hierarchy).

Things to check:
- whether `useradd` and `openat2` work under the default seccomp profile;
- whether `UseCgroupFD` placement works inside the container's cgroup namespace;
- that no Docker socket is mounted.

## Review: no code path executes caller-chosen programs as root

- The helper's only `exec`s as root are `useradd` and `userdel`, with fixed arguments derived from a validated workspace ID.
- Every `SpawnInWorkspace` path sets `Credential` to the workspace user before exec; the first program run is the stub, already as that user.
- argv and env are never interpreted with root privilege.
- The fuzz tests (`proto/fuzz_test.go`) check two things: the decoder never accepts an op outside the closed set, an invalid workspace ID, or a non-allowlisted environment entry; and the frame reader never over-allocates.

Gaps in the prototype that a real helper must close:
- Passed descriptors are not type-checked.
- There is no per-connection rate limit.
- The process table is in memory only.

## Proposed design changes

| Ref | Change | Contract section | Evidence |
|-----|--------|------------------|----------|
| ⟨P-15⟩ | **`SpawnInWorkspace` reports the exit status.** Either the spawn connection stays open and the helper sends `{handle, exit_code \| signal}` when the child exits, or there is a `WaitWorkspaceProcess(handle)` operation. §2.5 says only "returns a handle". | §2.5 | The server needs `receive-pack`/`fsck`/`pack-objects` results. The prototype had to infer completion from EOF on stdout, which cannot tell success from failure |
| ⟨P-16⟩ | **`NoNewPrivileges` is set in the child by a minimal trampoline** (Go's `SysProcAttr` has no field for it, and it must not be set on the root helper). Use a small static or C trampoline rather than a Go binary. | §2.5 | Go stub 4.10 ms vs `setpriv` 2.34 ms vs bare exec 1.09 ms. The Go stub is most of the +3.5 ms per spawn |
| ⟨P-17⟩ | **Capability detection at helper start.**<br>• cgroup v2 with memory+pids missing: run without limits, `doctor` warns, and `SignalWorkspace(all)` signals by uid.<br>• `openat2` missing: the helper refuses to start.<br>The contract states both behaviours. | §2.5, §9.2 (doctor) | This host: hybrid hierarchy, v2 controllers = `hugetlb` only |

⟨P-18⟩ and ⟨P-19⟩ come from P8 ([P8-ssh-endpoint.md](P8-ssh-endpoint.md)).

## Still open

- The escape matrix E1–E7 and E9, as `mvp.sh`-style deny assertions.
- code-server through the helper.
- Full smart-HTTP push and fetch through the front end with spawned `receive-pack`/`upload-pack`.
- The Docker variant.
- E9 on a host with cgroup v2 delegation.
