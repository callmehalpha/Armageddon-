# P2: Byte-exact capture → apply round-trip

**Verdict: PASS on Linux ↔ Linux** (__SEQS__ sequences, 0 mismatches) **after six fixes to the apply algorithm and two design changes.** macOS was **not tested**: there is no macOS host in this environment. The case-folding and Unicode-normalisation collision *detector* is implemented and self-tested, but behaviour on APFS remains open.

Code:
- [`prototypes/p2-roundtrip`](../../../prototypes/p2-roundtrip/main.go): the fuzz harness;
- [`prototypes/internal/gitshadow`](../../../prototypes/internal/gitshadow/gitshadow.go): capture, apply and seed under test.

## Method

Each sequence works like this:

1. **Setup.** Create a Git repository A with a `.gitignore` (`ignored/`, `*.log`, `.env*`) and a deliberately hostile `.gitattributes` (`*.txt text eol=lf`). Commit it, clone it to B, and give each side a git-shadow.
2. **Seed.** B is seeded from A's first checkpoint.
3. **Rounds.** Each round:
   - applies 1–8 random operations to the current writer;
   - captures a checkpoint;
   - transfers it as a thin pack (the P1 transport);
   - applies it on the other side;
   - compares the two trees with an **independent oracle**: a plain filesystem walk that compares type, exec bit, bytes and symlink targets, with its own copy of the ignore rules and no Git involved;
   - compares the two indexes with `git ls-files -s`.

**Operation mix:**
- create, modify, delete, rename, case-only rename;
- `chmod ±x`;
- symlinks: relative, dangling and absolute;
- file↔symlink, file→directory and directory→file;
- empty and nested-empty directories, `rm -r`;
- ignored noise, `.env*` files;
- staging changes: full, partial (staged ≠ worktree), unstage, `rm --cached`;
- injected merge conflicts (stages 1–3).

**Names and contents:**
- names include NFC/NFD variants, `ß`/`SS`, CJK, emoji, spaces, a leading `-`, `#`, backslash, tab, newline, quotes, and 160-character segments;
- contents include empty, CRLF, mixed endings, binary, Unicode, and up to 320 KB.

**Also exercised:**
- **writer swaps (handoff):** 20% of rounds;
- **injected crashes mid-apply** followed by resume: 30% of applies;
- **divergence checks (I7):** the destination is modified before apply, and the apply must refuse with `ErrDiverged`, then succeed after undo;
- **ignored-file tolerance:** local ignored files on the destination must not block an apply.

## Results

```
__RESULTS__
```

## Bugs found and fixed

The fuzzer found these within the first ~400 rounds. All are fixed in `gitshadow` and covered by the run above.

| # | Bug | Fix |
|---|-----|-----|
| 1 | A directory containing only empty directories is reported by Git as one collapsed entry, so nested empty directories were lost | Expand each reported directory to its empty leaves (cheap: it contains no files) |
| 2 | Removing an empty directory left its newly empty parents behind | Removals of stale empty directories feed the parent-prune set |
| 3 | A fresh `git clone` is **not byte-identical** to the writer's working tree: `eol=lf` rewrote CRLF files on checkout, so "clone == checkpoint" verification failed | **Design change:** new `Seed` operation. A new replica captures *itself* and applies the diff to the checkpoint, never assuming equality (§3.3 CLONING) |
| 4 | Resume after a crash: deletes that had already happened were skipped *before* their parent directory was recorded, so a directory that should become a file was never pruned | Record the parent before deciding to skip |
| 5 | Resume after a crash: a delete whose path had already been replaced (by a directory a later put created, or by its own type-change put) looked like a conflict | On resume, "already in the target state" counts as done |
| 6 | Directory → file on the writer, while the replica's directory still held **local ignored files**: apply refused forever | Move the blocking directory to a replica-private `displaced/` area (outside the working tree), report it, and continue. Ignored files are moved, never deleted (I4) |
| — | Temporary files a real crash leaves between write and rename | Resume removes `.armageddon-tmp-*` in the affected directories |

Separately, P2 surfaced two **design changes** in the index format (§6.2):

- **Index as a delta against HEAD, not a full manifest.** `git diff-index --cached HEAD` plus `git ls-files -u` captures the staged state in O(staged changes). A full `ls-files -s` manifest would be ~14 MB per checkpoint at 200k files. Restore touches only the affected paths (`git reset -- <paths>` plus `update-index --index-info`), so the stat cache survives and `git status` stays fast after apply.
- **Staged blobs must be reachable from the checkpoint.** A staged blob that differs from the working tree is referenced only from the manifest. So the checkpoint tree gets a `/staged` subtree that makes those blobs reachable, and transfer carries them.

## Policy clarifications (behaviour that is correct, now documented)

- **A directory holding only ignored files is not replicated.** Git treats it as ignored content; whatever regenerates the files regenerates the directory. Truly empty directories *are* replicated (`meta.empty_dirs`).
- **Preserve (`.env*`) applies to files only.** A *directory* matching `.env*` stays ignored with all its contents. This is deliberate: `.env/` is a common name for Python virtualenvs.
- **Gitlinks (submodules, nested repositories) are refused by apply** with an explicit error, as §11 states for v0.1.

## Portability (macOS / Windows): not tested here

`Collisions()` detects paths that collide under full Unicode case folding plus NFC normalisation, including directory-prefix collisions. Its self-test cases: `README.md`/`readme.md`, `café` NFC/NFD, `straße`/`STRASSE`, `A/x`/`a/y`. The fuzz trees produced __COLL__ real collisions, and all were detected.

What happens when such a checkpoint is applied *on* APFS is untested. The v0.1 policy proposed in the README is to refuse to apply a checkpoint containing collisions on a case-insensitive replica, with a clear error, rather than letting the filesystem merge two files. This needs a macOS run before implementation.
