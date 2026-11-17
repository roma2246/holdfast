# Roadmap status

Tracks `HOLDFAST_AI_ROADMAP.md` (plan v1.0, 2026-11-17; the plan itself lives outside the
repository). Each row has evidence; "done" means the evidence below exists, not just code.

Baseline: commit `f967e19`, branch `main`. Environment: macOS 26.6.2 (arm64),
Go 1.27.1, git 2.50.1 (Apple Git-155).

Differences from the plan's starting point: none in the code (HEAD is the commit the plan
was checked against). The working tree has untracked `AGENTS.md`, `docs/announce/`,
`docs/logo/` that are not part of this work and were left untouched. `AGENTS.md` describes
an older Russian-language layout and Codex hooks and does not match the repository.

## Stage A: baseline

| Task | Status | Evidence | Next step |
| --- | --- | --- | --- |
| A1. What exists | done | Table below | — |
| A2. Build, tests, coverage | done | `go build ./...` ok; `go test -race -coverpkg=./internal/... ./...` all packages ok; coverage 91.8% | Re-run after each change |
| A3. Shared-index race as a regression test | done | `TestWriteTreeCachedInterleaved` replays the interleaving deterministically: fails on `f967e19` ("the snapshot holds \"original a\\n\""), passes after the fix. `TestParallelSnapshotsMatchDisk` (8 workers × 5, snapshots and checkpoints with prune): 64 wrong file entries in 5 runs on `f967e19` | — |
| A4. Snapshot contract | done | `docs/SNAPSHOT_CONTRACT.md` | Update with B2/B3 |
| A5. Timing baseline | partial | Small text files only, see below | Large/binary files; keep a reproducible script in `docs/benchmarks/` |

### A1: what exists

| Area | State |
| --- | --- |
| Auto snapshot before each prompt (Claude Code hook), dedup, retention | works |
| Manual snapshot, list, diff, rollback, before-rollback backup, undo via backup | works |
| Worktree isolation of snapshots | works (property test) |
| Guard policy, mass-delete protection, doctor, uninstall, lint, sync | works |
| Concurrent snapshots | was broken (A3), fixed in B1 |
| Fast path after re-editing a file | was silently falling back to the slow path, fixed in B1 |
| Recovery of an interrupted rollback | missing (B2) |
| Selective rollback of chosen files | missing (C1) |
| Machine-readable output (JSON) | missing (C7) |
| Hooks for agents other than Claude Code | missing (E) |
| Windows | builds; path policy targets macOS/Linux |

### A5: timing (fast path, `writeTreeCached`)

Synthetic repository: N files of ~10 bytes in 200 directories, all committed; one file
edited before each run; 15 runs; Apple Silicon laptop; one series each, so treat as rough.

| Files | First snapshot | Next, p50 | Next, p95 | Service index |
| --- | --- | --- | --- | --- |
| 1 000 | 109 ms | 48 ms | 58 ms | 84 KB |
| 20 000 | 2.06 s | 99 ms | 289 ms | 1.5 MB |

For comparison, the same series on the slow path (what users got whenever a file was
edited a second time): 20 000 files p50 390 ms, p95 4.1 s.

## Stage B: reliability

| Task | Status | Evidence | Next step |
| --- | --- | --- | --- |
| B1. Snapshot race | done except last item | See below | Test prune during rollback |
| B2. Interrupted rollback | not started | — | Map partial-failure points of `Rollback` |
| B3. Coverage and limitations | not started | — | — |

### B1: what was done

- Choice: independent indexes, not a lock. Each run copies `.git/holdfast/index` to a
  private file in the same directory, runs `read-tree → add → write-tree` on it and
  renames it back. No lock means nothing to wait for, no timeout to pick and no stale
  lock after a crash. The cost is one file copy: on 20 000 files p50 went from 92 ms
  (shared index, no copy) to 99 ms.
- Found along the way: `read-tree -m HEAD` refused ("not uptodate") whenever a file
  already in the service index was edited again, so the fast path silently fell back to a
  full rehash. Replaced with `read-tree --reset HEAD`; covered by a new step in
  `TestWriteTreeCachedMatchesFresh` that fails with `-m`.
- Stress: `TestParallelSnapshotsMatchDisk` + `TestWriteTreeCachedInterleaved`,
  `-race -count=20`: all passed (160 runs of 8 parallel workers, 0 errors, 0 wrong
  snapshots). Checkpoints in that test prune with `keep=3` concurrently, and no
  concurrency errors occurred.
- Known leftover: if a process is killed mid-snapshot, an `index-*.tmp` file may stay in
  `.git/holdfast/`. It is harmless (never read) and not cleaned up yet.
- Not covered yet: pruning a snapshot while a rollback to it runs. The rollback uses the
  commit hash, which outlives the ref until `git gc`, but there is no test.
