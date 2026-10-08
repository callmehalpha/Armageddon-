# Disaster suite

Contract §10 lists eighteen ways things go wrong (F1–F18) and what Armageddon must do in each. Every one of them is an automated test. They run on every pull request and again nightly on `main` (`.github/workflows/ci.yml` and `nightly.yml`).

Each scenario ends with the same invariant, contract §12 item 7: **every checkpoint the server acknowledged is still there, in order, with the same content.** Unacknowledged work may be retried or quarantined, never silently dropped.

## Where each failure is tested

| | Failure | Test | What it checks |
|---|---------|------|----------------|
| F1 | Laptop dies, server-first workflow | `disaster.sh` | The laptop is killed; the server seat still holds the workspace, work continues there, and a replacement laptop catches up |
| F2 | Laptop dies holding the lease | `write.sh` step 7 | STALE, forced takeover, the server seat continues from the last acknowledged checkpoint |
| F3 | Laptop offline with the lease | `write.sh` steps 5–6 | Checkpoints queue and commit in order on reconnect; if the lease was taken meanwhile, the queue is quarantined |
| F4 | Follower edited offline | `write.sh` step 3 | `work local` fast-forwards when the server has not moved |
| F5 | Network drops mid-upload | `disaster.sh` | A throttled proxy is killed during an upload; the retry with the same `(epoch, parent, oid)` is idempotent |
| F6 | Server crashes mid-commit | `disaster.sh` (a), (b) | `SIGKILL` before and after the database transaction (`commit-before-db`, `commit-after-db`); startup reconciles refs from the database |
| F7 | Server crashes mid-apply | `disaster.sh` | `SIGKILL` at `seat-apply`; on restart the worktree reaches the current checkpoint, and a half-applied file is kept as a quarantine |
| F8 | Server disk full | `disaster.sh` | A small tmpfs is filled; workspaces go DEGRADED with `disk_full`, writes get 507, writers queue; `doctor --repair` after freeing space returns them to READY and the queue drains |
| F9 (a) | Server lost, restore from backup | `ops.sh` step 3 | Backup, fresh data directory, restore: identical refs, worktree and keys |
| F9 (b) | Server lost, rebuild from a replica | `disaster.sh` | A second server is seeded with `workspace seed --from-replica`: history, uncommitted edits and `.env` arrive |
| F10 | `repo.git` corrupted | `disaster.sh` | An object is deleted; the scheduled fsck degrades the workspace (`repo_corrupt`); `workspace repair --from-device` restores it and returns it to READY |
| F11 | Git operation in progress during handoff | `write.sh` step 2 | The handoff is refused with an explanation |
| F12 | Stolen device | `disaster.sh` | Revocation kills the tokens, force-releases the lease and refuses Git access |
| F13 | `git gc --prune=now` on the server seat | `disaster.sh` | Deleted and force-moved branches survive in the trash; `armageddon git trash restore` brings one back |
| F14 | Server-side process edits files while a device writes | `disaster.sh` | Server drift goes to quarantine (`source=server`), then the apply proceeds |
| F15 | Clock skew | `disaster.sh` | A laptop 3 hours ahead still writes (ordering never uses clocks); `status` warns about the skew; `doctor` checks the server's clock |
| F16 | Upgrade fails | `internal/lifecycle` tests | An injected migration failure and a failed health check both roll the binary and database back |
| F17 | Very large file | `disaster.sh` | A file over `sync.max_file_size` is not synced and is listed in `status` and the checkpoint metadata; raising the limit syncs it |
| F18 | Server seat and a follower edit at once | `write.sh` step 4 | The follower's edits are quarantined |

## Running it

On a throwaway Linux VM, as root (it creates `ws-*` users and mounts a tmpfs):

```sh
go build -o armageddon ./cmd/armageddon
sudo install -m 0755 armageddon /usr/local/bin/armageddon
sudo mkdir -p /srv && sudo chmod 755 /srv
sudo test/e2e/disaster.sh /usr/local/bin/armageddon     # a few minutes
sudo test/e2e/write.sh /usr/local/bin/armageddon
go test ./internal/lifecycle -run 'RollsBack|Rollback'
```

`KEEP=1` keeps the data directory and logs of a failed run for inspection.

## Fault injection

`ARMAGEDDON_FAULTS` (comma separated) arms crash points in the server: the process kills itself with `SIGKILL` when it reaches a named point, like a power cut would. The points are `commit-before-db`, `commit-after-db` and `seat-apply`. An entry `clock=+3h` shifts the agent's clock instead (faketime cannot shift a Go binary's clock). Unset, as in production, a point costs one map lookup.
