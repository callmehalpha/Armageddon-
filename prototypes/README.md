# Phase 0 prototypes

Throwaway code that tests the riskiest assumptions in
[`docs/design/v0.1-architecture.md`](../docs/design/v0.1-architecture.md) (§11)
before implementation starts. **Nothing here is production code** and nothing
here is imported by it. Results and proposed design changes are in
[`docs/design/prototypes/`](../docs/design/prototypes/).

| Dir | Prototype | Question |
|-----|-----------|----------|
| `internal/gitshadow` | shared | git-shadow capture / checkpoint / apply / seed (the §6 TreeSync strategy) |
| `p1-capture-perf` | P1 | Is capture fast enough for continuous checkpoints? |
| `p2-roundtrip` | P2 | Does capture → apply round-trip byte-exactly, including crashes mid-apply? |
| `p4-failure-model` | P4 | Do the lease, epoch fencing and CAS commits lose acknowledged or unacknowledged work under crashes and partitions? |
| `p5-git-hosting` | P5 | Can the server host the canonical repo over smart HTTP with per-request authz, fencing and trash refs? |
| `p6-privilege-boundary` | P6 | Does the root helper (typed API) plus unprivileged server plus one user per workspace work? |
| `p8-ssh-endpoint` | P8 | Does an embedded SSH endpoint with device keys give exec, PTY, SFTP and loopback forwarding? |

Requirements: Go ≥ 1.26, git ≥ 2.40 on `PATH`, Linux (P2's cross-platform modes also run on macOS). P6 and P8 run as root and create throwaway users.

```sh
cd prototypes
go run ./p2-roundtrip -seqs 200 -rounds 15          # P2 fuzz (≈3 s per sequence)
go run ./p1-capture-perf -sizes 5000,50000,200000   # P1 benchmark (≈1 h incl. tree generation)
go run ./p4-failure-model -runs 100000              # P4 model + mutants
go build -o /tmp/p5 ./p5-git-hosting && /tmp/p5     # P5 scenarios (hooks exec the binary)
go run ./p2-roundtrip -mode xplat-local             # P2 portability focus cases (also -mode export / import)
sudo ./p6-privilege-boundary/run-e2e.sh 100         # P6 helper end to end + spawn cost (root)
sudo ./p8-ssh-endpoint/run-e2e.sh                   # P8 with the OpenSSH client (root)
```

Every program is deterministic for a given `-seed` except where it measures
wall-clock time.
