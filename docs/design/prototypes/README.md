# Phase 0 prototype results

Prototypes P1, P2, P4 and P5 from [§11 of the v0.1 design](../v0.1-architecture.md#11-remaining-risky-assumptions-and-prototypes) were run on 2026-10-04.

- **Environment:** Linux x86-64, 4 vCPU, 15 GB RAM, git 2.43.0, Go 1.24.
- **Code:** throwaway, in [`prototypes/`](../../../prototypes/).
- **Contract status:** none of the findings below has been applied to `v0.1-architecture.md` yet. They are **proposed** design changes for review, as agreed: no architecture change merges on the strength of an assumption a prototype was meant to test.

## Verdicts

| Prototype | Question | Verdict | Detail |
|-----------|----------|---------|--------|
| **P1** | Is git-shadow capture fast enough for continuous checkpoints? | **Pass at 5k and 50k; marginal at 200k** (only with scoped capture: 896 ms p95 vs a 1 s target). Found the transport bug ⟨P-1⟩. Storage criterion replaced | [P1-capture-perf.md](P1-capture-perf.md) |
| **P2** | Does capture → apply round-trip byte-exactly? | **Pass on Linux ↔ Linux**; macOS untested | [P2-roundtrip.md](P2-roundtrip.md) |
| **P4** | Can crashes or partitions lose acknowledged or unacknowledged work? | **Pass** after 3 protocol additions (the original protocol was safe but could stall) | [P4-failure-model.md](P4-failure-model.md) |
| **P5** | Can the server host the canonical repo with authz, fencing and trash refs? | **Pass, 13 of 13 scenarios**, after replacing CGI with a native smart-HTTP front end. Overhead +0.3–0.9% | [P5-git-hosting.md](P5-git-hosting.md) |

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

## Still open

- **G0.5:** product-owner review of ⟨P-1⟩–⟨P-14⟩. Each accepted change is applied to `v0.1-architecture.md` in PR #1.
- **P6** (privilege boundary) must run before implementation milestone M3. Every P5 scenario here ran as one OS user.
- **macOS:** P2 on APFS (case folding, NFD), and P1 with git's built-in fsmonitor.
- **P3:** watcher completeness, which gates ⟨P-12⟩.
- **Git client versions other than 2.43**, and protocol v2 on the native endpoint.
- **Large monorepos (200k+ files):** sub-second capture is marginal with Git as the capture engine. Options are recorded in P1 F2; not needed for v0.1.
