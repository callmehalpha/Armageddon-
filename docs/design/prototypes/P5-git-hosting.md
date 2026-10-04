# P5: Server-hosted canonical repository over smart HTTP

**Verdict: PASS. All 13 scenarios pass with one architectural correction.** Wrapping `git http-backend` as CGI from Go does not work for real pushes. The front end has to speak Git's smart-HTTP protocol itself, spawning `git upload-pack` / `git receive-pack --stateless-rpc`. That's the approach Gitea and Gogs use.

Code: [`prototypes/p5-git-hosting`](../../../prototypes/p5-git-hosting/main.go). The binary is both the server and its own hook entry point (`p5 hook <name>`).

## What was built

A Go HTTP front end per workspace with:

- **Device authentication.** Basic auth with device tokens stands in for the §7.3 credential helper.
- **Per-request authorization.** Any member device may fetch. Only the lease holder may push, and the push must carry `X-Armageddon-Epoch`.
- **The fencing lock (§4.2).** `receive-pack` holds the workspace fence as a reader. A lease transfer takes it as a writer.
- **Server-owned Git settings via `GIT_CONFIG_*` environment variables:** `core.hooksPath`, `receive.denyCurrentBranch=ignore`, `receive.fsckObjects`. Environment config outranks the workspace-writable `repo.git/config`.
- **A `pre-receive` hook** that re-checks the lease with the authority over a unix socket (defence in depth). It records a **trash ref** for every deleted or force-moved branch *before* the ref moves. It does this by pushing the old tip to an internal, token-protected endpoint for `checkpoints.git`, never by writing into that repository's files.
- **A `reference-transaction` hook** that does the same for local Git commands run on the server seat (§5.4).
- **An internal `checkpoints.git` endpoint.** It requires a per-workspace hook token and accepts only creation of `refs/trash/*`.

## Results

git 2.43.0 client and server. The throughput repository is 300 MiB of incompressible content in 3 commits.

| # | Scenario | Result |
|---|----------|--------|
| S1 | Paired devices clone; a wrong token is refused | PASS |
| S2 | Holder pushes to the branch checked out in the server worktree (`denyCurrentBranch=ignore`) | PASS. The repo and worktree `HEAD` move; worktree *files* are left for checkpoint apply, as designed |
| S3 | Holder pushes with a stale epoch | PASS. Rejected with `lease_lost: holder=d1 epoch=2 (you sent 1)` |
| S4 | Non-holder device pushes | PASS. Rejected |
| S5 | `--atomic` push of two refs where the authority vetoes one | PASS. Nothing lands |
| S6 | Pushed branch deletion | PASS. Trash ref created; objects present in `checkpoints.git` |
| S7 | Force push (non-fast-forward) | PASS. Old tip preserved as a trash ref |
| S8 | Server-seat local `git branch -D` and `git reset --hard HEAD~1` | PASS. Both old tips trashed via `reference-transaction` |
| S9 | Internal endpoint: no token, or a ref outside `refs/trash/*` | PASS. Both refused |
| S10 | Workspace sets `core.hooksPath` to a malicious directory in `repo.git/config`, then a device pushes | PASS. The server-owned value wins; the malicious hook never ran |
| S11 | **Fencing:** forced takeover while a 100 MiB push is in flight | PASS. The takeover blocked 5.6 s and acquired the fence 115 µs after `receive-pack` finished. The push landed in full, and the pusher's next push with the old epoch was rejected |
| S12 | Clone throughput against a baseline* | PASS. 3.455 s vs 3.425 s (**+0.9%**), median of 3 |
| S12b | Push throughput (100 MiB) against a baseline* | PASS. 13.90 s vs 13.85 s (**+0.3%**, including the hook and authority callback) |

\*The baseline is the same native front end with authentication, hooks and fencing disabled. Stock `git http-backend` could not be used as a baseline, for the reason in finding F1.

## Findings

### F1: CGI wrapping of `git http-backend` is not viable from Go (architecture correction)

Go's `net/http/cgi` rejects request bodies sent with `Transfer-Encoding: chunked` (HTTP 400). Git sends every push larger than `http.postBuffer` (1 MiB by default) chunked. The small pushes in S2–S10 passed through CGI, and the first realistic push failed:

```
error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400
```

**Proposed (§5.1, §9.1):** the Git HTTP endpoint implements smart HTTP natively:

- `GET …/info/refs?service=…` runs `git <svc> --stateless-rpc --advertise-refs <repo>`, prefixed with the `# service=` pkt-line.
- `POST …/git-<svc>` streams the (optionally gzip-encoded) body into `git <svc> --stateless-rpc <repo>`.

This is about 60 lines (`serveSmartHTTP` in the prototype). It also removes the CGI layer from the privilege boundary: the spawned process is exactly what `SpawnInWorkspace(git-service)` runs as `ws-<id>` (§2.5).

The prototype serves protocol v0/v1 (it ignores the `Git-Protocol` header). Supporting protocol v2 for fetch (`GIT_PROTOCOL=version=2`, no service pkt-line) is a small follow-up and worth doing for large repositories.

### F2: the authority re-check in `pre-receive` is redundant with the fence, and should stay

The front end checks the lease, takes the fence, and re-checks under the fence before spawning `receive-pack`. So the `pre-receive` re-check never fired in any scenario. It costs one unix-socket round trip per push (unmeasurable in S12b), and it would catch a front-end bug. Keep it as defence in depth.

### F3: server-seat trash protection is best effort by construction

The `reference-transaction` hook for local Git on the server seat is configured through `repo.git/config`, and the workspace user can edit that file (S10 shows only the *HTTP* path is protected by environment-injected config). A workspace process can therefore disable its own trash recording for *local* operations. That's self-harm only: it cannot affect other workspaces or the server. Device pushes are always protected.

**Proposed:** document §5.4 trash recording on the server seat as best effort. Optionally, inject `core.hooksPath` through the environment of shells the helper spawns, which covers the common case of interactive terminals without making it a guarantee.

### F4: unix socket path length

Socket paths are limited to 108 bytes. Deep data directories overflow that limit (the prototype hit it). **Proposed:** authority and hook sockets live under a short fixed directory such as `/run/armageddon/<ws-id>.sock`, not inside the workspace directory.

### Cosmetic

The prototype's `reference-transaction` hook reads the current ref value in the `committed` phase too, so its event log shows `old → old` there. Only the `prepared` phase uses the old value, so trash recording is unaffected. The real implementation should cache the old values from `prepared`.

## Not covered

- **Git client versions other than 2.43.0.** The criterion asked for 2.30–2.47; only 2.43 was available. GIT_CONFIG_* injection needs a server-side git ≥ 2.31.
- **Protocol v2.**
- **Running `upload-pack` / `receive-pack` as a separate workspace user through the helper.** That's P6; here everything ran as one user.
- **The push time limit** that kills `receive-pack` (§4.2). Not implemented in the prototype.
- **TLS, and the embedded ACME edge.**
