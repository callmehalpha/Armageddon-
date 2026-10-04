# Phase 0 prototype results

Prototypes P1, P2, P4 and P5 from [§11 of the v0.1 design](../v0.1-architecture.md#11-remaining-risky-assumptions-and-prototypes) were run on 2026-10-04.

- **Environment:** Linux x86-64, 4 vCPU, 15 GB RAM, git 2.43.0, Go 1.24.
- **Code:** throwaway, in [`prototypes/`](../../../prototypes/).
- **Contract status:** none of the findings below has been applied to `v0.1-architecture.md` yet. They are **proposed** design changes for review, as agreed: no architecture change merges on the strength of an assumption a prototype was meant to test.

## Verdicts

| Prototype | Question | Verdict | Detail |
|-----------|----------|---------|--------|
| **P1** | Is git-shadow capture fast enough for continuous checkpoints? | __P1_VERDICT__ | [P1-capture-perf.md](P1-capture-perf.md) |
| **P2** | Does capture → apply round-trip byte-exactly? | **Pass on Linux ↔ Linux**; macOS untested | [P2-roundtrip.md](P2-roundtrip.md) |
| **P4** | Can crashes or partitions lose acknowledged or unacknowledged work? | **Pass** after 3 protocol additions (the original protocol was safe but could stall) | [P4-failure-model.md](P4-failure-model.md) |
| **P5** | Can the server host the canonical repo with authz, fencing and trash refs? | __P5_VERDICT__ | [P5-git-hosting.md](P5-git-hosting.md) |

## Proposed design changes

Each change says where it lands in the contract and what evidence supports it.

__CHANGES__

## Still open

__OPEN__
