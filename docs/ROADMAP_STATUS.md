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
| Recovery of an interrupted rollback | added in B2: journal, `--resume`/`--abort`, hash check |
| Selective rollback of chosen files | added (C1, PR #1 by Deadpool2000 plus fixes) |
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
| B1. Snapshot race | done | See below | — |
| B2. Interrupted rollback | done | See below | — |
| B3. Coverage and limitations | not started | — | Snapshot status/size in `guard list`; binary, symlink, LFS, sparse-checkout tests |

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
- Temporary files a killed process leaves in `.git/holdfast/` are removed by the next
  snapshot once they are over an hour old; the creation time is in the file name, because
  a private index copy carries the shared index's old modification time
  [`TestSweepTemp`].
- Pruning never deletes the target or the backup of a pending rollback
  [`TestInterruptedRollbackIsRecoverable`]. Between planning and writing the journal a
  prune can still delete the target ref; the rollback uses the commit hash, which stays
  until `git gc`, so that window is not a data risk.

### B2: what was done

- Partial-failure points in `Rollback`: after the backup, during deletions, during
  `checkout-index`, after restoring but before reporting success. Before this change an
  error there printed only the git error (the backup name was lost) and the next process
  had no idea a rollback had happened.
- A journal (`<git dir>/holdfast/rollback.json`) is written, synced and atomically
  linked into place before the first file changes, and removed only after the result is
  verified. Writing it is also the lock that keeps two rollbacks from running at once.
  A failure to write it stops the rollback with nothing changed.
- The backup is read back before files change.
- After the rollback git hashes every restored path against the snapshot and every deleted
  path must be gone. The first run of this check found that a path deleted as a file can
  legitimately come back as a directory (file↔directory changes); handled and covered by
  the existing `TestRollbackNeverLosesData` cases.
- `guard list` and `doctor` warn about a pending rollback; `guard rollback` refuses and
  names both ways out; `--resume` and `--abort` finish or undo it, also when the snapshot
  ref has been pruned (the journal keeps the commit).
- Tests: `TestInterruptedRollbackIsRecoverable` (4 interruption points × resume/abort,
  each from a fresh process value, checked file by file), `TestRollbackVerifiesResult`,
  `TestRollbackDoesNotStartWithoutJournal` (scenario S08), `TestDamagedJournalIsReported`,
  `TestConcurrentRollbacks`, `TestGuardInterruptedRollback` (CLI, doctor).
- Not simulated: a crash in the middle of `git checkout-index` itself. The recovery path
  is the same, but that exact moment has no test.
- Found, not fixed (belongs to C3): the CLI shows a plan, then `Rollback` computes the
  plan again; if the tree changes in between, the executed plan can differ from the one
  shown.

## Stage C: selective rollback

| Task | Status | Evidence | Next step |
| --- | --- | --- | --- |
| C1. Roll back chosen files | done | See below | — |
| C2. Exact plan before rollback | partial | The plan lists restore/delete/keep per path; no "excluded paths" section yet | Show unselected changes that stay |
| C3. Plan shown = plan executed | not started | The CLI plans, then `Rollback` plans again | Pass the shown plan's trees to `Rollback`, refuse if the tree changed |
| C4. Warn about later edits | not started | — | — |

### C1: what was done

- Merged PR #1 (Deadpool2000): `guard rollback PATH...` / `--path`, `guard diff PATH...`,
  `MatchesPath`, tests `TestSelectiveRollback`, `TestSelectiveRollbackPreservesUnselectedFiles`,
  `TestGuardSelectiveRollback`.
- Fixed after the merge:
  - `guard diff PATH` passed the patterns to git as pathspecs, where `*` crosses
    directories, so it could show files the rollback would not touch. It now passes exactly
    the planned paths, literally [`TestSelectiveDiffMatchesRollback`].
  - When a file and a directory swapped places, `checkout-index -f` silently deleted an
    unselected path. Such a selection is now refused with the blocking paths named
    [`TestSelectiveRollbackRefusesHiddenDeletion`, both directions].
  - The rollback journal keeps the selection, so `--resume`/`--abort` continue the same
    selective rollback instead of the whole tree [`TestInterruptedSelectiveRollbackKeepsSelection`].
  - A backslash in a pattern is a separator only on Windows; `[` is no longer treated as a
    glob character (character classes are not supported); `.gitignore` regained its final newline.
