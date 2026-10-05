# P8: Embedded SSH endpoint

**Verdict: PASS for everything that can be checked without a real IDE. VS Code Remote-SSH and JetBrains Gateway themselves were not run.**

- An embedded server (gliderlabs/ssh) with Ed25519 device-key authentication works with the stock OpenSSH client for exec, PTY, SFTP, `ssh -L` and `ssh -D`.
- An emulation of the Remote-SSH bootstrap also works: a bash script sent over exec downloads and starts a server, which the client then reaches through a forward. All 15 checks pass.
- Two design changes come out of it. Loopback forwarding is not workspace-isolated (⟨P-18⟩). SFTP must be a spawned process running as the workspace user (⟨P-19⟩).

Code: [`prototypes/p8-ssh-endpoint`](../../../prototypes/p8-ssh-endpoint/main.go), run with `sudo prototypes/p8-ssh-endpoint/run-e2e.sh`.

## What was built

- **Authentication.** Only Ed25519 public keys are accepted, and only those listed for the login user. The login user must match `ws-[a-z0-9]{8,24}`. Password and keyboard-interactive auth are not offered.
- **Sessions.** Exec runs `bash -c <command>`; an interactive session runs `bash -l`. With a PTY request the session gets a PTY, and window changes are forwarded. Every process runs as the OS user named by the login.
  - The spike drops privileges directly from a root process.
  - In the product this is `SpawnInWorkspace(kind=ssh-session)`, using the same descriptor-passing path P6 demonstrated for `pty-shell`.
- **Client environment.** Only `LC_*`, `TERM` and `VSCODE_*` are passed through.
- **SFTP.** The subsystem execs `p8 sftp-server` as the workspace user, which serves `pkg/sftp` over stdio. It is **not** served in-process. In-process would mean file access with the server's (root's) privileges.
- **Port forwarding.** `direct-tcpip` channels are allowed only to loopback destinations (`localhost`, `127.0.0.0/8`, `::1`). The same channel type carries `ssh -L` and `ssh -D`.

## Results (OpenSSH 9.6p1 client, Linux)

| # | Check | Result |
|---|-------|--------|
| 1 | Paired Ed25519 device key logs in | PASS |
| 2 | Unpaired Ed25519 key refused | PASS |
| 3 | RSA key refused even when listed | PASS |
| 4 | Unknown user refused | PASS |
| 5 | Exec runs as the workspace uid | PASS |
| 6 | Exec exit status propagates (`exit 7` → 7) | PASS |
| 7 | `ssh -tt` session has a `/dev/pts` tty | PASS |
| 8 | `sftp` put and get round-trip byte-exactly | PASS |
| 9 | SFTP-written file is owned by the workspace user | PASS |
| 10 | `ssh -L` to a loopback port served by the workspace user | PASS |
| 11 | `ssh -D` (SOCKS) to loopback | PASS |
| 12 | Forward to a non-loopback destination (`ssh -W 192.0.2.1:80`) refused | PASS |
| 13 | **Remote-SSH emulation:** `ssh -T host bash` with a script on stdin downloads a tarball from a stand-in update server, unpacks it into `~/.vscode-server/bin/<commit>`, starts the server under `nohup`, and prints `listeningOn==PORT==` | PASS |
| 14 | Client reaches that server through `-L`; it reports the workspace uid | PASS |
| 15 | The server outlives the bootstrap session and is reachable through `-D` | PASS |

## Still needs a manual check with real IDEs

- **VS Code Remote-SSH:**
  - the real bootstrap script (it probes `uname`, `glibc`, existing installs and lock files);
  - downloading from `update.code.visualstudio.com`, or the "download locally, copy over SCP/SFTP" mode;
  - opening a folder, extension host start-up, reconnect after a network drop;
  - `remote.SSH.useLocalServer` true and false (`-D` vs `-L`).
- **JetBrains Gateway:** it uses SFTP and exec to install its backend, then forwards a port. Not run.
- **Agent forwarding and `ssh -R` (remote forwarding):** not implemented. Neither IDE needs them for the basic flow.
- **Host key distribution:** how a device learns and pins the endpoint's host key (`armageddon ssh-config` writing a `known_hosts` entry) is not designed yet.

## Proposed design changes

| Ref | Change | Contract section | Evidence |
|-----|--------|------------------|----------|
| ⟨P-18⟩ | **Loopback-only forwarding does not isolate workspaces.** The forward is dialled by the SSH server process in the host's network namespace. "127.0.0.1:PORT" therefore reaches *any* loopback listener on the host: other workspaces' dev servers and Armageddon's own internal ports included. Unix users do not restrict TCP connects. Options:<br>(a) a per-workspace network namespace (the dev server binds loopback inside it; the forward is dialled from inside it via the helper);<br>(b) forwarding allowed only to ports owned by the workspace's uid (checked from `/proc/net/tcp` at dial time, racy);<br>(c) document as a v0.1 limitation for single-tenant installs.<br>Recommended: (a) as the target, (c) for v0.1 if it ships. | §2.3, §2.5 (`ConfigurePortProxy`), P6 | Design review during P8; checks 10, 11 and 14 show the forward is dialled by the server |
| ⟨P-19⟩ | **SFTP and every session channel run as spawned processes as `ws-<id>`** (`SpawnInWorkspace(kind=ssh-session)`), never in the server process. The SSH endpoint itself (key exchange, auth, channel mux) runs as `armageddon`. | §2.5 | SFTP in-process would do file I/O with the server's privileges. Checks 8 and 9 show the subprocess design works with `pkg/sftp` |

## Verdict against §11

The pass criterion is "VS Code Remote-SSH connects, installs its server, opens a folder and forwards a port. Gateway connects". It is **not met yet**, because neither IDE was run.

Every protocol feature those IDEs use in their basic flow does work: exec with a script on stdin, a long-lived process surviving the session, SFTP, `-L`, `-D` and PTY. That makes it likely they will connect. A manual run with each IDE is the remaining gate before Q4 (SSH in v0.1) is confirmed.
