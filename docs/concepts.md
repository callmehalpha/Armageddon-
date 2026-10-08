# How Armageddon works

Five ideas explain almost everything Armageddon does: the **workspace**, its **seats**, **checkpoints**, the **lease**, and **quarantine**.

## Workspace

A workspace is one project on your server: a Git repository, its working tree, and everything uncommitted in it. Each workspace runs as its own Linux user (`ws-…`), so workspaces cannot read each other, and none of them can touch the server itself. A small root helper is the only process allowed to create those users; the server runs unprivileged.

A workspace is **READY** (normal) or **DEGRADED**. A degraded workspace stays readable and followable, but refuses writes until it is fixed. The reason tells you what to do: `disk_full` or `repo_corrupt` ([troubleshooting](troubleshooting.md)).

## Seats

A seat is a place where the working tree exists:

- **The server seat** is the copy on your server. You use it from the browser terminal, the browser IDE (code-server) or SSH. It is the default place to work, and the one that matters: if every laptop disappears, it is still there.
- **Device seats** are replicas on your laptops, made with `armageddon clone`. The agent (`armageddon agent run`, or `armageddon follow` for one replica) keeps them current.

## Checkpoints

Git only moves what you commit. Armageddon also moves what you have not committed yet.

A couple of seconds after the working tree changes, the seat that is writing captures a **checkpoint**: every file in the working tree, the staged index and `HEAD`. Checkpoints are stored in a separate internal repository, so your Git history stays clean, and they are numbered in the order the server accepted them. Other seats apply them, and each apply is verified: a replica either matches the checkpoint exactly or reports why not.

The server **acknowledges** a checkpoint once it is stored. Acknowledged checkpoints are never lost, whatever crashes: that is the promise the [disaster suite](disaster-suite.md) tests.

Files that Git ignores are not synced, with one exception: files matching `.env*` are preserved and replicated (see below). Files over the size limit are not synced either; `armageddon status` lists them.

### Your `.env` files are copied to your server and replicas

Deliberately: a workspace you cannot run is not a workspace you can continue on another machine. So `.env`, `.env.local` and every other `.env*` file travel with checkpoints, even though `.gitignore` excludes them from Git. They are stored on your server, in each replica, and in server backups (a backup's passphrase encrypts only the server's keys, so protect backups like the server itself). They never enter Git history and are never pushed to your Git remote.

If a secret must not leave one machine, keep it outside the working tree (for example in your shell profile or a secrets manager) and load it from there.

### The size limit

Files over 50 MiB are not synced by default. A workspace can change that in `.armageddon/sync.yaml`, which is itself synced:

```yaml
sync:
  max_file_size: 200MB   # or "none" for no limit
```

## The lease

Exactly one seat writes at a time; the others follow. Which one is decided by the **lease**, which the server holds by default.

- `armageddon work local` moves the lease to your laptop: it becomes the writer and the server seat follows. Use it to work offline or with local tools.
- `armageddon work remote` hands it back to the server seat.
- Taking the lease is always explicit. Nothing moves it behind your back.

Every lease handoff increments an **epoch**. A checkpoint from an old epoch is never accepted as current, so a laptop that comes back online after someone else took over cannot overwrite their work. Its queued checkpoints go to quarantine instead.

If a laptop holding the lease dies, the lease goes **STALE** after its heartbeat stops, and a member can force a takeover from the browser or with `work local --force`, which asks for their password again. You lose at most the edits made after the laptop's last acknowledged checkpoint, typically a few seconds.

Ordering never depends on clocks. A laptop with a wrong clock still works; `armageddon status` warns about it because it breaks TLS and makes logs hard to read.

## Quarantine

Armageddon never overwrites a change silently. When a change cannot be applied cleanly, it is set aside in a **quarantine** on the server:

- you edited a read-only replica (it did not hold the lease) and a new checkpoint arrived;
- a laptop's offline queue lost the race to someone who took the lease;
- a process on the server edited files while a laptop was writing (`source=server`).

```sh
armageddon quarantine list
armageddon quarantine diff <id>        # what is in it
armageddon quarantine apply <id>       # 3-way merge into your working tree (needs the lease)
armageddon quarantine export <id> DIR  # or copy the files out
armageddon quarantine drop <id>        # when you are done
```

## Branches you delete are kept

When a branch or tag is deleted or force-moved on the server, the old commit is kept in a hidden trash, which even `git gc --prune=now` does not remove. In the server seat:

```sh
armageddon git trash                  # newest first
armageddon git trash restore 1        # or: restore 1 --as rescued-branch
```

## Recovery from a replica

Every replica holds the full history and the latest working state, so every laptop is also a disaster-recovery copy:

- **The server is lost.** Install a new one, log in from the laptop, and run `armageddon workspace seed --from-replica` in the replica. The new server gets the history, the branches and the uncommitted state, `.env` included. (A server backup restores everything else, such as users and other workspaces: `server restore`.)
- **The server's repository is damaged.** `armageddon workspace repair --from-device` sends the replica's history back, and the workspace returns to READY once `git fsck` is clean.
