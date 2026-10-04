# P1: Capture performance

**Verdict: PASS at 5k and 50k files. FAIL at 200k with full-tree capture, **a marginal pass (896 ms p95 against a 1 s target)** with watcher-scoped capture. FAIL on the storage-growth criterion as worded, which was the wrong metric; a replacement is proposed below.** P1 also found the most important transport bug of Phase 0: stock `git fetch` resends the whole tree for every checkpoint.

Code: [`prototypes/p1-capture-perf`](../../../prototypes/p1-capture-perf/main.go), using [`internal/gitshadow`](../../../prototypes/internal/gitshadow/gitshadow.go).

## Setup

- **Synthetic source trees:** ~20 files per directory, nesting up to 7 levels, text-like content. 60% of files are 0.5–4 KiB, 35% are 4–16 KiB, and 5% are 16–116 KiB.
- **Each tree is committed**, and has an ignored `node_modules/` with 100,000 small files plus a `.env`.
- **Machine:** 4 vCPU, warm page cache, git 2.43.0. Cold-disk performance was not measured.
- **Statistics:** p50/p95 over 30 repetitions unless noted. "Changed" means appended edits to random tracked files.
- **Real repository:** not measured. Cloning one (for example the Linux kernel, ~90k files) was not attempted here.

## Results

| Measure | 5k files | 50k files | 200k files | Criterion |
|---------|---------:|----------:|-----------:|-----------|
| Cold capture (empty shadow) | 1.5 s | 16.9 s | 59.3 s | < 15 s at 50k: **fail (16.9 s)** |
| Re-capture after losing the capture index | 0.29 s | 2.2 s | 10.1 s | — |
| No-op capture (nothing changed) | 30 / 38 ms | 209 / 240 ms | 1004 / 1165 ms | — |
| Incremental capture, 1 file | 39 / 47 ms | 249 / 270 ms | 1145 / 1266 ms | — |
| Incremental capture, 20 files | 66 / 81 ms | 269 / **284 ms** | 1123 / **1194 ms** | p95 < 300 ms at 50k: **pass**; < 1 s at 200k: **fail** |
| Incremental capture, 200 files | 173 / 214 ms | 382 / 425 ms | 1246 / 1381 ms | — |
| Full checkpoint (capture + index + meta + commit), 20 files | 90 / 100 ms | 292 / 317 ms | 1169 / 1231 ms | — |
| Transfer, stock `git fetch`, 20 files | 2.7 s | 25.3 s | **100.8 s** | — |
| Transfer, thin pack vs base tree, 20 files ⟨P-1⟩ | 42 / 56 ms | 93 / 103 ms | 306 / 329 ms | — |
| Average thin-pack size, 20 files | 52 KiB | 104 KiB | 284 KiB | — |
| Apply (verify + write + post-verify + index), 20 files | 128 / 157 ms | 542 / 583 ms | 2183 / 2248 ms | — |
| Replica seed (fresh clone → first checkpoint) | 3.9 s | 19.0 s | 25.9 s | — |

Cells show p50 / p95.

### Where the time goes (20 files changed)

| Phase | 5k | 50k | 200k |
|-------|---:|----:|-----:|
| `git add -A` (stat every file, scan for untracked files) | 31 ms | 114 ms | 510 ms |
| `ls-files -o -i --directory` (find `.env*` to preserve) | 7 ms | 49 ms | 291 ms |
| `ls-files -o --directory` (empty directories) | 7 ms | 49 ms | 259 ms |
| `write-tree` | 25 ms | 53 ms | 200 ms |
| `diff-index --cached HEAD` (index delta ⟨P-3⟩) | 3 ms | 9 ms | 35 ms |

**Capture cost is set by tree size, not by how much changed.** A no-op capture costs almost as much as a 200-file change. Three full directory walks (`add -A` and two `ls-files`) make up about 85% of the time.

The config variants tested made no meaningful difference: `core.untrackedCache=false` and `index.version=2` were within noise or slightly slower.

## Findings

### F1: stock `git fetch` cannot carry checkpoints ⟨P-1⟩

§6.2 made checkpoint commits *parentless*, so retention could delete any checkpoint without rewriting history. Git's fetch negotiation, however, only excludes objects reachable from *common commits*. For unrelated commits it sends the entire tree every time: 5,255 objects for a 2-file change at 5k files, and 100 s for 20 files at 200k.

Packing `new --not <receiver's base checkpoint>^{tree}` instead sends only the changed objects. For a 2-file change that's 7 objects and 22 KB in 13 ms. At 200k files a 20-file change takes 306 ms.

**Proposed:** checkpoints keep no parents. Transfer goes through Armageddon endpoints that carry a thin pack computed against the receiver's base checkpoint, with a full-pack fallback when the base has been pruned. Checkpoint transport never uses `git fetch` or `git push` (§5.1, §6.4).

### F2: capture must be scoped to the changes at large sizes ⟨P-12⟩

Full capture is inherently O(tree) because of the three directory walks. Scoping capture to the paths the watcher reports (`add -A -- <paths>`, plus `ls-files` limited to their directories, plus an incremental empty-directory set) removes those walks:

| Files changed | 50k full | 50k scoped | 200k full | 200k scoped |
|---------------|---------:|-----------:|----------:|------------:|
| 1 | 232 / 251 ms | 167 / 185 ms | 1079 / 1142 ms | 750 / 811 ms |
| 20 | 244 / 286 ms | **202 / 218 ms** | 1098 / 1233 ms | **836 / 896 ms** |
| 200 | 349 / 390 ms | 471 / 515 ms | 1246 / 1330 ms | 1576 / 1748 ms |

Cells show p50 / p95, n=20. Every scoped capture was cross-checked against a full capture right after it: **0 mismatches in 120 comparisons**.

Scoped capture saves only about 25–30% for small changes, and it is *slower* for 200 changed files, because `ls-files` with many pathspecs is expensive. The remaining floor is that every capture reads and rewrites the whole index: at 200k entries, `add` and `write-tree` each touch a ~15 MB index. So:

- Use scoped capture for change sets up to ~50 paths, and full capture above that.
- At 200k+ files, sub-second capture is marginal with Git as the capture engine.

If large monorepos become a target, the next step is a split index (`core.splitIndex`, untested) or an in-process index writer. Neither is needed for v0.1.

**Proposed (§6.3):** the agent and the server seat use watcher-scoped capture. A full capture runs as a backstop: on watcher overflow, on startup, every N minutes, and before any lease flush. Scoped capture is correct only if the watcher misses nothing, which is P3's question. So scoped capture stays behind a flag until P3 passes, and full capture remains the default.

### F3: cold capture

Cold capture is a one-time cost per replica or workspace: 16.9 s at 50k files and 59 s at 200k. It misses the < 15 s criterion at 50k by 1.9 s.

It's dominated by hashing and zlib-compressing every file into loose objects. Re-capture when objects already exist is 4–7× faster.

**Proposed:** accept the cost and show progress. Two possible optimisations exist:

- **Parallelise hashing** across workers, each with its own index, then merge them.
- **Seed the capture index from the user's index.** This is incorrect when `.gitattributes` filters or line-ending conversion apply, because the index stores the normalised blob. So it can only be used when the repository has no such attributes.

Neither is needed for v0.1.

### F4: storage growth — wrong criterion, acceptable absolute cost

Over 1,000 checkpoints at 50k files, each with 20 appended edits (558 KB of edits in total):

| | |
|-|-|
| Growth before gc | 115 MiB (118 KiB per checkpoint, as loose objects) |
| Growth after gc | **11.5 MiB (11.8 KiB per checkpoint)**, 21.7× the edit bytes |
| gc time | 31 s |

The ≤ 1.2× criterion compared against the edit bytes. But each checkpoint rewrites every tree object on the path to each changed file (about 1 KiB per directory level), plus `meta.json` and a commit. So the overhead is a fixed cost per checkpoint and per changed directory, not proportional to bytes edited.

At the expected few hundred checkpoints per active day, that's **3–6 MiB per workspace per day before retention**. The retention tiers (Q6) then cap it.

**Proposed replacement criteria:**
- shadow growth ≤ 20 KiB per checkpoint after gc;
- periodic `git gc` (or `repack -d`) at least daily, because loose objects inflate the size about 10×;
- gc runs off the commit path and must not block captures. It took 31 s here; schedule it during idle time.

### F5: apply cost

Apply runs two captures: verify and post-verify. That's why it costs about twice a capture: 0.54 s at 50k and 2.2 s at 200k.

**Proposed:** the post-verify only needs to check the paths that changed, so use a scoped capture for it, while the pre-verify stays full. That halves apply time at large sizes. Followers are not latency-critical (§6.5), so this is an optimisation, not a blocker.

## What P1 did not measure

- **Cold disk.** All runs used a warm page cache.
- **macOS.** APFS stat performance differs, and macOS has git's built-in fsmonitor (`core.fsmonitor=true`), which could replace the agent watcher for the `add -A` scan. Worth measuring on a Mac (G0.7).
- **A real repository.** Synthetic trees only.
- **Concurrency.** Captures running alongside an active editor, a build, or `npm install`.
