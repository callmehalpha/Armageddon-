# P4: Failure safety of lease, epoch fencing and CAS commits

**Verdict: PASS, after three protocol additions.** The protocol in §3–§4 and §6.4 as originally written was safe: no lost work in any run. It was not *live*: under some fault sequences the workspace stopped making progress. Adding the three rules below made it both safe and live across 100,000 randomized runs. These rules are design changes and are proposed in [README.md § Proposed design changes](README.md#proposed-design-changes).

Code: [`prototypes/p4-failure-model`](../../../prototypes/p4-failure-model/main.go)

## What was modelled

A deterministic, seeded simulation with three components:

- **Authority:** durable lease (holder, epoch, handoff), the current checkpoint, committed checkpoints, and quarantines.
- **Server seat:** in-process. It commits synchronously when it holds the lease and follows synchronously when it doesn't.
- **Two devices:** each has a durable tree, base checkpoint, epoch, pending queue, quarantine outbox and `releasing` flag, plus volatile retry state.

**Messages** are randomly delivered, dropped (10%), duplicated (5%), reordered (delivery order is random) and partitioned per device.

**User actions:**
- edits on any seat, including edits to replicas that don't hold the lease, and edits on the server seat while it doesn't hold the lease (server drift);
- `work local`, `work remote`, and forced takeover by either side.

**Faults:**
- device crash and restart (volatile state lost);
- server crash and restart;
- partition and heal;
- permanent device destruction (the laptop dies).

**Checker.** Work is modelled as unique edit tokens. The checker shares no code with the protocol and runs after **every** step:

| Invariant | Check |
|-----------|-------|
| I1 single writer | every accepted commit was authored by the seat granted that epoch; no epoch granted to two seats |
| I3 no acknowledged loss | the current checkpoint contains every token of every acknowledged checkpoint |
| I4 quarantine before overwrite | every token made on a seat that still exists is durable somewhere: server current, a server quarantine, or that seat's own durable state (tree, base, pending, outbox) |
| Liveness | once faults stop, the system converges: no stuck handoff, outboxes drained, the writer committed, clean followers at current |

## Results (final protocol)

```
100000 runs × 300 fault steps + convergence (23 s)
steps=30,000,000  messages=4,213,552  dropped=1,479,582  duplicated=84,050
edits=1,683,334  commits=455,016  rejected(lease_lost)=22,999  rejected(parent)=0  idempotent re-acks=6,501
handoffs=128,638  handoff timeouts=11,572  forced takeovers=284,352  quarantines=196,778  server drift edits=10,081
device crashes=908,556  server crashes=655,749  partitions=1,260,007  devices destroyed=81,366
tokens lost only with a destroyed device (accepted RPO)=201,839
tokens left local on unresolved dirty followers=66,889
VIOLATIONS=0
```

- **"Lost only with a destroyed device"** is the accepted RPO (§4.5). These edits were never captured or acknowledged before the device was permanently lost. No acknowledged token was ever lost (I3).
- **"Left local on unresolved dirty followers"** are edits on a replica that doesn't hold the lease, where the user never ran `work local` (decision Q2). They are still on that device's disk and are quarantined as soon as a new checkpoint arrives.

## Mutants: is the checker able to see failures?

Each mutant breaks one rule. 20,000 runs each:

| Mutant | Caught | How |
|--------|-------:|-----|
| follower overwrites a dirty tree without quarantine | 81.0% | I4 |
| quarantine outbox not durable across agent crash | 63.2% | I4 |
| grant keeps a stale tree (no reconcile on takeover) | 15.7% | I3 |
| authority ignores epoch **and** skips parent CAS | 15.8% | I3 |
| authority ignores epoch (CAS still on) | 3.8% | I3 |
| resync reply omits lease state | 6.3% | liveness |
| writer sends no heartbeat | 0.1% | liveness |
| authority skips parent CAS (epoch check on) | 0% | — |
| force takeover keeps the epoch | ~0% (3 runs) | liveness |
| retry after lost ack not idempotent | 0% | — |
| stale-epoch rejection honoured | 0% | — |
| writer never resyncs its base | 0% | — |

### Reading the mutant results

- **The checker detects real failures.** Every mutant that removes a safety mechanism *with no backup* is caught.
- **The epoch check and parent CAS are two layers.** With the epoch check alone removed, violations appear in 3.8% of runs. With both removed, they appear in 15.8%. So CAS catches roughly three quarters of stale-writer commits when fencing fails. The correct protocol never triggered CAS (`rejected(parent)=0`). It's pure defence in depth, and the design keeps both layers.
- **Mutants at 0% are masked by another layer in this model**, not proven unnecessary:
  - *Force keeps epoch* is masked by the holder-identity check.
  - *Idempotency* is masked by CAS: a duplicate commit fails the parent check and causes a spurious quarantine, which is safe but noisy.
  - *Stale rejection honoured* and *writer never resyncs* each recreate the stall in the trace below only when the other is also missing.

  These stay in the design for the efficiency and liveness reasons noted, and P4 does not claim to test them.

## What the model found (protocol changes)

Each of these was a real failure of the protocol as written in §4 and §6.4. They were found by the checker before the fix and are absent after it.

1. **A missed grant stalls the workspace.** If the `granted` message is lost, or the device crashes before processing it, the authority records the device as holder but the device believes it's a follower. Nobody writes, and nothing recovers it.
   **Fix:** every resync or heartbeat reply carries `(holder, epoch)`. A device that finds itself holder at a newer epoch processes it as a grant.
   **Mutant:** *resync omits lease state*, 6.3%.
2. **A stale rejection kills a fresh lease.** This happened with a device that took over its own lease (epoch 4 → 5). A retry of a pending commit, sent under epoch 4, came back `lease_lost`. The device treated that as losing its *new* lease, quarantined its work and resynced. A later retry of the same checkpoint then landed, leaving the writer's base behind current.
   **Fix:** rejections echo the epoch of the attempt, and the agent ignores rejections for epochs older than its current one.
   Trace (seed 1905, before the fix):
   ```
   277 GRANT e4 server → d1
   294 GRANT e5 d1 → d1            (forced takeover of its own lease)
   301 d1 quarantine q6 (rejected:lease_lost, 8 tokens)     ← rejection of an e4 attempt
   301 d1 now writer e5 (tree=6 tokens)
   301 COMMIT cp5 by d1 e5 seq4 parent=3 tokens=8           ← in-flight retry lands; d1's base is now behind
   ```
3. **A writer can fall behind its own commits.** This follows from (2) or any similar race: the writer holds the lease, but current has moved past its base through one of its own late-landing commits. Without a fix, the next capture's commit is rejected by CAS, which is safe but stalls.
   **Fix:** the heartbeat reply includes the current checkpoint. A writer with nothing pending whose base is behind current at the same epoch adopts current, quarantining any local edits made on the stale base first.
4. **Implementation note:** after losing the lease, an agent must accept the next resync even if its sequence number is not newer than its last acknowledged one. Its local state was already quarantined. Without this, a device stayed stuck on a pseudo-base.

## What P4 does not cover

- **Git refs.** The model covers the checkpoint pointer and working-tree tokens. Branch pushes under fencing are covered by P5 (S11).
- **Apply partial failure inside a single apply.** That's covered by P2's injected crashes. Here an apply is atomic.
- **Timing-based lease expiry.** STALE is informational in the design, and takeovers are explicit, so the model needs no clocks.
- **More than two devices,** and multiple users sharing the server seat.
- **SQLite durability under real crashes.** The model assumes an authority transaction is atomic and durable. That is SQLite's job; it was not re-verified here.
