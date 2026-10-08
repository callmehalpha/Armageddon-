# Troubleshooting

Start with the two status commands. They name the problem and, in most cases, the fix:

```sh
armageddon status            # on a laptop, in a replica
sudo armageddon doctor       # on the server (add --repair to fix what is safe to fix)
```

## "the server's disk is almost full" (`disk_full`)

**What happened.** Free space on the server's data filesystem fell below the floor (5% by default, `min_free_percent` in the server config). Every workspace went DEGRADED: new checkpoints, commits and pushes are refused, while reading, following and cloning keep working.

**Nothing is lost.** A laptop that is writing keeps its checkpoints queued and retries.

**Fix.** On the server, free space (old backups, `docker system prune`, logs), then:

```sh
sudo armageddon doctor --repair    # re-checks the disk, returns the workspaces to READY
```

The queued checkpoints are then sent in order.

## A workspace is DEGRADED with `repo_corrupt`

**What happened.** The scheduled `git fsck` (daily by default, `fsck_interval_s`) found missing or damaged objects in the workspace's repository. Writes are refused so the damage cannot spread.

**Fix.** On a laptop with a replica of the workspace:

```sh
cd <replica>
armageddon workspace repair --from-device
```

It sends the replica's history; the server keeps what it has, adds what is missing, runs `fsck` again, and returns the workspace to READY when it is clean. If no replica has the lost objects, restore the latest backup (`server restore`, see [operations](operations.md)).

## The server is gone

Replicas keep working read-only, and a laptop that holds the lease keeps queuing. Then either:

- **restore a backup** onto a new server: `armageddon server restore <backup>` (everything: users, workspaces, settings); or
- **rebuild a workspace from a replica**: install a new server, `armageddon login` to it from the laptop, then in the replica:

  ```sh
  armageddon workspace seed --from-replica --name my-project
  armageddon clone my-project
  ```

  The new workspace has the replica's branches, tags, history and uncommitted state, `.env` files included.

## I deleted a branch (or force-pushed over it)

The server kept it. In the workspace's terminal on the server:

```sh
armageddon git trash                 # newest first
armageddon git trash restore 1       # back under its old name
armageddon git trash restore 1 --as rescued
```

Restoring never overwrites an existing branch.

## A file is not syncing: "over the size limit"

Files over 50 MiB are not synced, so a stray video or database dump does not choke every replica. `armageddon status` lists them. To sync bigger files, raise the limit in the workspace's `.armageddon/sync.yaml`:

```yaml
sync:
  max_file_size: 500MB   # or "none"
```

The file then syncs with the next checkpoint. For large binary assets, Git LFS is the better long-term home.

## "clock: WARNING … ahead of / behind the server"

Your laptop's clock differs from the server's by more than 2 minutes. Syncing is not affected (ordering never uses clocks), but TLS certificates may be rejected and logs are hard to compare. Turn on automatic time (NTP) on the laptop. `doctor` checks the server's clock the same way.

## "DIRTY: you edited a read-only replica"

The laptop does not hold the lease, so its replica follows the server. Either take the workspace with `armageddon work local` (if the server has not moved on, your edits become the next checkpoint), or let them go: when the next checkpoint arrives, your edits are set aside in a quarantine instead of being overwritten.

## My changes are "in quarantine"

Armageddon set them aside rather than overwrite anything:

```sh
armageddon quarantine list
armageddon quarantine diff <id>
armageddon quarantine apply <id>     # 3-way merge into your working tree (run `work local` first)
armageddon quarantine drop <id>      # once you are done with it
```

## The laptop holding the workspace died

The lease goes STALE once its heartbeat stops. Take over from the browser, or from another machine with `armageddon work local --force`; you are asked for your password again. Edits after the dead laptop's last acknowledged checkpoint (usually a few seconds) are the only thing lost.

## "the workspace is DEGRADED (…): it is readable but refuses writes"

Another cause than the two above (for example an interrupted import). Run `sudo armageddon doctor` on the server: it lists every degraded workspace with its reason. For a reason other than `disk_full` or `repo_corrupt`, the server log for that workspace says what went wrong.
