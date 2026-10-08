# Performance (M9.3)

Date: 2026-10-08. The performance pass checks capture, transfer and apply against the P1 targets (architecture §14) on a reference repository, and runs nightly so a regression shows up the next morning (`.github/workflows/nightly.yml`, job `perf`).

## Reference repository

- 50,000 tracked files in nested directories, 200 B to 8 KiB each;
- an ignored `node_modules` with 100,000 files (it must cost nothing);
- 50 rounds of 20 changed files, each captured on a writer, transferred as a thin pack and applied on a follower with full verification.

## Results

Measured in the cloud sandbox the project is built in (4 vCPUs); the nightly job records the same numbers on GitHub's runners in its summary.

| Measure | Result | P1 target |
|---------|--------|-----------|
| Incremental capture, 20 files, p95 | **273 ms** (p50 229 ms) | < 300 ms: **pass** |
| Cold capture (first checkpoint) | **11.1 s** | < 15 s: **pass** |
| Transfer of one checkpoint, p95 | 174 ms, about 100 KiB | — |
| Verified apply on a follower, p95 | 403 ms (p50 362 ms) | — |
| Capture + transfer + apply, p95 | 818 ms | — |
| Seeding a follower (first full checkpoint) | 22.8 s | — |

Apply costs more than capture because it verifies twice, before and after writing (P1 found the same ratio).

## What changed to get there

The first run missed the capture target: **517 ms p95**. The size limit added in M9 (F17, `max_file_size`) scanned the whole tree for big files before every capture with `git ls-files -m -o`, which stats every entry on one thread; then `git add -A` walked the tree again. Three changes brought it to 273 ms, faster than capture without the size limit (315 ms):

1. The scan runs three Git processes at once: the policy lookup, `diff-files` (changed tracked files, with Git's parallel stat) and `ls-files -o` (new files).
2. Capture then adds exactly the paths the scan found, instead of walking the tree a second time, and adds nothing when nothing changed. Above 2,000 changed paths (a branch switch, say) it falls back to the full walk, because matching every index entry against a long path list costs more.
3. The empty-directory listing runs alongside the preserved-file (`.env*`) step.

## Running it

```sh
ARMAGEDDON_PERF=1 go test ./internal/treesync/gitshadow -run TestPerfReference -v -timeout 40m
```

`ARMAGEDDON_PERF_FILES` and `ARMAGEDDON_PERF_IGNORED` change the sizes, `ARMAGEDDON_PERF_NOLIMIT=1` turns the size limit off for comparison, and `ARMAGEDDON_PERF_OUT=file.json` writes the numbers. At 50k files the test fails if either P1 target is missed.

## Limits

Above about 50k tracked files, capture slows down linearly (P1 measured 1.2 s p95 at 200k files with full-tree capture). The server seat still polls every 2 s (B5); devices use a file watcher. See `LIMITATIONS.md` B5 and B6.
