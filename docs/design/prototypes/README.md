# Phase 0 prototype results

Prototypes P1, P2, P4 and P5 from [§11 of the v0.1 design](../v0.1-architecture.md#11-remaining-risky-assumptions-and-prototypes) were run on 2026-10-04.

- **Environment:** Linux x86-64, 4 vCPU, 15 GB RAM, git 2.43.0, Go 1.24.
- **Code:** throwaway, in [`prototypes/`](../../../prototypes/).
- **Contract status:** none of the findings below has been applied to `v0.1-architecture.md` yet. They are **proposed** design changes for review, as agreed: no architecture change merges on the strength of an assumption a prototype was meant to test.

## Verdicts

| Prototype | Question | Verdict | Detail |
|-----------|----------|---------|--------|
| **P1** | Is git-shadow capture fast enough for continuous checkpoints? | **Pass at 5k and 50k; marginal at 200k** (only with scoped capture: 896 ms p95 vs a 1 s target). Found the transport bug ⟨P-1⟩. Storage criterion replaced | [P1-capture-perf.md](P1-capture-perf.md) |
| **P2** | Does capture → apply round-trip byte-exactly? | **Pass on Linux ↔ Linux.** 10,000 sequences, 149,998 rounds, 33,568 crash resumes; 0 working-tree mismatches; 1 index-only edge case originating in Git. **macOS (Phase 1, CI): FAIL.** 0 silent mismatches, but case-only renames captured on APFS keep the old-case path, so Linux replicas get duplicate files, and apply on APFS refuses case-only renames and stays stuck; 11 of 30 fuzz sequences fail at the initial seed (⟨P-20⟩) | [P2-roundtrip.md](P2-roundtrip.md) |
| **P4** | Can crashes or partitions lose acknowledged or unacknowledged work? | **Pass** after 3 protocol additions (the original protocol was safe but could stall) | [P4-failure-model.md](P4-failure-model.md) |
| **P5** | Can the server host the canonical repo with authz, fencing and trash refs? | **Pass, 13 of 13 scenarios**, after replacing CGI with a native smart-HTTP front end. Overhead +0.3–0.9% | [P5-git-hosting.md](P5-git-hosting.md) |
| **P6** *(Phase 1)* | Is the §2.5 boundary practical: root helper with a typed API, unprivileged server, one user per workspace? | **Partial.** Helper works end to end (PTY, git-service and capture run as `ws-<id>` with `NoNewPrivs`); decoder fuzzed; Compose parser rejects the listed fields. +3.5 ms per spawn, mostly the Go trampoline. **Escape matrix E1–E7/E9, code-server, full HTTP push/fetch and the Docker variant not done.** cgroup v2 limits unavailable on this host (hybrid hierarchy) | [P6-privilege-boundary.md](P6-privilege-boundary.md) |
| **P8** *(Phase 1)* | Does an embedded SSH endpoint with device keys support what IDEs need? | **Pass for what OpenSSH can verify** (15 of 15: Ed25519 auth, exec, PTY, SFTP as the workspace user, `-L`/`-D` to loopback, Remote-SSH bootstrap emulation). Real VS Code/Gateway not run. Loopback forwarding is not workspace-isolated | [P8-ssh-endpoint.md](P8-ssh-endpoint.md) |

## Proposed design changes

Each change says where it lands in the contract and what evidence supports it.

| Ref | Change | Contract section | Evidence |
|-----|--------|------------------|----------|
| ⟨P-1⟩ | **Checkpoint transport is a thin pack relative to the receiver's base checkpoint tree**, carried by Armageddon endpoints, with a full-pack fallback when the base is pruned. Checkpoint commits stay parentless; checkpoints never use `git fetch`/`push`. | §5.1, §6.4, §6.5 | P1: stock fetch resends the whole tree (100 s for a 20-file change at 200k files); the thin pack takes 306 ms |
| ⟨P-2⟩ | **New replicas are seeded:** the replica captures itself and applies the diff to the checkpoint. A fresh clone is never assumed equal to the writer's tree. | §3.3 (CLONING), §6.5 | P2: an `eol=lf` attribute rewrites CRLF files on clone |
| ⟨P-3⟩ | **Index stored as a delta against HEAD** (`diff-index --cached` plus unmerged entries), with staged blobs made reachable through a `/staged` subtree. Restore touches only the affected paths. | §6.2, §6.5 | P2 correctness; P1: 35 ms at 200k files vs a ~14 MB full manifest per checkpoint |
| ⟨P-4⟩ | **Apply rules:** resume treats "already in the target state" as done; parent directories are recorded before skipping; leftover temp files are removed; ENOTDIR counts as absent; nested empty directories are expanded to leaves; a directory blocking a put that holds only local ignored files is **displaced** to a replica-private area, not deleted and not blocking. | §6.5 | P2: 7 bugs found by the fuzzer, all fixed |
| ⟨P-5⟩ | **Policy clarifications:** a directory holding only ignored files is not replicated; preserve (`.env*`) applies to files only (`.env/` virtualenvs stay ignored); gitlinks are refused by apply. | §6.6 | P2 |
| ⟨P-6⟩ | **Git HTTP endpoint speaks smart HTTP natively** (spawns `upload-pack`/`receive-pack --stateless-rpc`); no CGI. | §5.1, §9.1 | P5: Go `net/http/cgi` returns 400 on chunked bodies, which git uses for every push over 1 MiB |
| ⟨P-7⟩ | **Resync and heartbeat replies carry lease state** `(holder, epoch)`, so a device that missed its grant learns it holds the lease. | §3.2, §4.4 | P4: missed grant led to a stalled workspace (mutant caught 6.3%) |
| ⟨P-8⟩ | **Commit rejections echo the attempted epoch;** agents ignore rejections for epochs older than their current one. | §6.4 | P4: a stale rejection killed a fresh lease (trace in P4 write-up) |
| ⟨P-9⟩ | **Writer resync:** a heartbeat reply includes current. A writer with nothing pending whose base is behind current at the same epoch adopts current, quarantining any local edits made on the stale base first. | §6.4 | P4 |
| ⟨P-10⟩ | **Keep both epoch fencing and parent CAS.** CAS never fires when fencing works, but it catches ~3/4 of stale-writer commits when fencing fails. | §4.1 (no change; rationale added) | P4 mutants: 3.8% vs 15.8% |
| ⟨P-11⟩ | **Storage criterion:** ≤ 20 KiB per checkpoint after gc; scheduled `gc` at least daily, off the commit path. | §11 P1 criterion | P1: 11.8 KiB per checkpoint; loose objects are 10× larger |
| ⟨P-12⟩ | **Watcher-scoped capture** for change sets up to ~50 paths, with full capture as the backstop (overflow, startup, periodic, before flush). Behind a flag until P3 passes. | §6.3 | P1: required to meet the 200k target; 0 mismatches against full capture in 120 cross-checks |
| ⟨P-13⟩ | **Server-seat trash recording for local Git is best effort** (the workspace can edit its own `repo.git/config`). Device pushes are always protected. | §5.4 | P5 S10 / F3 |
| ⟨P-14⟩ | **Authority and hook sockets live under `/run/armageddon/`**, not inside workspace directories (108-byte path limit). | §2.4 | P5 F4 |
| ⟨P-15⟩ | **`SpawnInWorkspace` reports the exit status** (on the spawn connection, or via a wait operation). | §2.5 | P6: the server cannot otherwise tell a failed `receive-pack`/`fsck` from success |
| ⟨P-16⟩ | **`NoNewPrivileges` is set in the child by a minimal (non-Go) trampoline.** | §2.5 | P6: Go stub 4.1 ms vs `setpriv` 2.3 ms vs bare exec 1.1 ms |
| ⟨P-17⟩ | **Helper start-up capability checks:** no cgroup v2 memory/pids → run without limits, `doctor` warns, `SignalWorkspace(all)` signals by uid; no `openat2` → refuse to start. | §2.5, §9.2 | P6: hybrid hierarchy on the test host |
| ⟨P-18⟩ | **Loopback-only SSH forwarding does not isolate workspaces** (it is dialled in the host network namespace). Target: a per-workspace network namespace; document it as a limitation if v0.1 ships SSH before then. | §2.3, §2.5 | P8 design review |
| ⟨P-19⟩ | **SFTP and every SSH session channel run as spawned `ws-<id>` processes,** never inside the server. | §2.5 | P8 checks 8–9 |
| ⟨P-20⟩ | **Case- and normalisation-aware capture and apply:** capture reconciles index entries with real `readdir` names on insensitive filesystems; apply does case-only renames through a temporary name, and refuses up front any checkpoint whose paths collide under the replica's filesystem rules. | §6.3, §6.5 | P2 macOS CI: old-case paths persist in macOS checkpoints; APFS apply stuck on case-only rename |

## Still open

- **G0.5:** product-owner review of ⟨P-1⟩–⟨P-14⟩. Each accepted change is applied to `v0.1-architecture.md` in PR #1.
- **G1:** product-owner review of ⟨P-15⟩–⟨P-20⟩ (Phase 1).
- **P2 on macOS:** fix per ⟨P-20⟩ and rerun; diagnose the 11 of 30 initial-seed failures (rerun with `-keep`); run the full 10,000-sequence `workflow_dispatch` budget once seeding passes.
- **P6** (privilege boundary) must complete before implementation milestone M3. Still open: the escape matrix (E1–E7, E9), code-server through the helper, full HTTP push/fetch through the helper, and the Docker variant.
- **P8 with real IDEs** (VS Code Remote-SSH, JetBrains Gateway).
- **macOS:** P2 on APFS now runs in CI (the `p2-macos` workflow; results are in the macOS section of [P2-roundtrip.md](P2-roundtrip.md)). P1 with git's built-in fsmonitor is still open.
- **P3:** watcher completeness, which gates ⟨P-12⟩.
- **Git client versions other than 2.43**, and protocol v2 on the native endpoint.
- **Large monorepos (200k+ files):** sub-second capture is marginal with Git as the capture engine. Options are recorded in P1 F2; not needed for v0.1.
