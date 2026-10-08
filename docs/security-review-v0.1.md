# Security review, v0.1 (M9.2)

Date: 2026-10-08. Scope: the privilege boundary of contract §2.5 (root helper, unprivileged server, per-workspace users), everything that crosses it, and the paths an attacker reaches from the network, from a paired device, or from inside a workspace.

**Result:** two high and three medium findings, all fixed in M9. Of the seven low findings, three are fixed and four are accepted for v0.1, with the reasoning below. B13 and B21 (the IDE and workspace ports served on the Armageddon origin) stay **accepted risks** for v0.1: see `LIMITATIONS.md`.

## Threat model in one paragraph

A member can run any code as their workspace user, and a paired device can send any bytes the API accepts. Neither may (a) act as the server user or root, (b) read or write another workspace, or (c) make the server write outside the workspace it is acting for. An unauthenticated network client may not cost the server more than a bounded amount of work. The release pipeline must not let a tampered download install.

## Findings

| # | Severity | Finding | Status |
|---|----------|---------|--------|
| 1 | High | **Apply and quarantine paths were not validated.** A checkpoint or quarantine is built by another seat, so its tree is untrusted. A tree entry named `.git/hooks/post-checkout`, or a path through a symlink the same tree creates, would make apply or `quarantine export/apply` write into a Git directory (code execution on the next Git command) or outside the working tree. | **Fixed.** `gitshadow.CheckPath` rejects absolute and `..` paths, any component that names a Git directory (`.git` in any case, with the trailing dots, spaces and ignorable Unicode that macOS and Windows strip, and the 8.3 name `git~1`), and parents that are symlinks on disk. It runs on every apply change, on preserved empty directories, again right before each write, in `untar` and in the 3-way quarantine merge. Packs from devices are indexed with `index-pack --strict` (Git's own fsck rejects `.git` tree entries on arrival). Tests: `TestCheckPath`, `TestUntarRefusesUnsafePaths`. |
| 2 | High | **`install.sh` verified the release signature with the binary it had just downloaded** when `minisign` was missing. Whoever controls the download controls the verifier. | **Fixed.** Without `minisign`, the installer verifies the minisign signature with OpenSSL (key id, Ed25519 signature over the manifest, and the global signature over the trusted comment). With neither tool it refuses, unless `--allow-unsigned`. `test/install/prefix.sh` covers signed, unsigned and tampered releases. |
| 3 | Medium | **The helper's socket `chown`/`chmod` followed symlinks.** Between `listen` and `chown`, a process able to write the socket directory could swap the socket for a symlink and have root chown an arbitrary file to the server user. | **Fixed.** `os.Lchown`, and no `chmod`: the umask (0177) already creates the socket 0600. |
| 4 | Medium | **The updater followed symlinks in the server-owned data directory.** Rollback opened `<db>.restore.tmp` with `O_CREATE\|O_TRUNC` as root, and the pre-update backup went into `backups/` without checking what it was. A compromised server user could plant links there and have root truncate or overwrite any file. | **Fixed.** The temporary file is removed, then created with `O_EXCL\|O_NOFOLLOW` and chowned through the open descriptor; `backups/` must be a real directory. |
| 5 | Medium | **Unauthenticated argon2 work.** Login and invite acceptance hashed a password (64 MiB, about 100 ms of CPU) for every request, before checking anything. A small flood exhausted memory. | **Fixed.** Invite and setup links are checked before hashing. All argon2 work goes through a bounded pool (half the CPUs, at least 2) that waits for the request's context, so a flood queues instead of allocating. Login keeps its 300 ms delay on failure. |
| 6 | Low | The authority socket read a hook message without a size bound. | **Fixed:** 16 MiB limit. |
| 7 | Low | Checkpoint and recovery packs are buffered in memory (up to 2 GiB per request). | Accepted for v0.1: only authenticated devices of members can send them, and uploads from one device are serialized. Streaming to disk is a post-v0.1 item. |
| 8 | Low | `server update` does not refuse a downgrade on its own; the manifest's `min_upgrade_from` and the signature are what protect it. | Accepted: a signed older release is the operator's choice, and `rollback` relies on it. |
| 9 | Low | A dev server bound to `[::]` was dialled on `127.0.0.1`, which misses an `IPV6_V6ONLY` socket. | **Fixed:** `[::]` is dialled on `::1`. Test: `TestReachableIPUnspecified`. |
| 10 | Low | Revoking a device and changing stored Git credentials do not ask for the password again. | Accepted for v0.1: both need a session with a CSRF token. Re-authentication for sensitive actions is post-v0.1. |
| 11 | Low | `RepairDataOwnership` (the one-time MVP upgrade) chowned root-owned files with more than one hard link. Where `fs.protected_hardlinks` is off, the server user could link a root file into the data directory and receive it. | **Fixed:** root-owned non-directories with more than one link are skipped and logged. The walk already never followed symlinks. |
| 12 | Low | `armageddon compose` runs Docker with root's `DOCKER_CONFIG`. | Accepted: the helper refuses images that need a login it does not have, and the configuration holds no workspace secrets. |

## Checked and found sound

- The helper's API is a closed allowlist; every request is typed, and `SO_PEERCRED` pins the caller to the server user.
- Workspace directories are created with `openat2(RESOLVE_NO_SYMLINKS|RESOLVE_BENEATH)`; the escape matrix (`test/integration/escape.sh`, E1–E6) passes.
- Git on `repo.git` always runs as the workspace user; `checkpoints.git` is server-owned and never exposed.
- Hooks report to the authority socket, which checks the peer uid against the workspace user.
- Device authentication is a signed challenge (Ed25519) with short-lived tokens; refresh is bound to the device key.
- Sessions use `HttpOnly`, `SameSite=Lax` cookies and a CSRF token on every state-changing request.
- Seeding and repair from a device index the packs as the workspace user with `--strict`, accept only `refs/heads/*` and `refs/tags/*` names, and never trust the device's working state without the usual verified apply.

## How to re-run the checks

```sh
go test ./internal/treesync/gitshadow -run 'CheckPath' ./internal/agent -run Untar
bash test/install/prefix.sh
sudo test/integration/escape.sh /usr/local/bin/armageddon
sudo test/e2e/disaster.sh /usr/local/bin/armageddon
```
