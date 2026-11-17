# Snapshot contract

What a Holdfast restore point is, what it holds and what happens when it can't be
made. Every statement here is either checked by a test (named in brackets) or listed
as not verified.

## What a snapshot holds

- Every file of the working tree that git would see with `git add -A`: tracked files
  (edited or not) and untracked files that `.gitignore` does not exclude
  [`TestWriteTreeCachedMatchesFresh`, `TestRollbackNeverLosesData`].
- Deleted tracked files are absent from the snapshot, so a rollback brings them back.
- The executable bit and symbolic links (as links), because that is what a git tree stores.

## What it does not hold

- Files excluded by `.gitignore`. Exception: when a rollback would overwrite such a file,
  the before-rollback backup force-adds it, or the rollback does not start
  [`TestRollbackNeverLosesData`, `TestRollbackStopsWhenBackupImpossible`].
- The contents of nested git repositories and submodules: they are stored as a link to a
  commit. A rollback leaves a nested repository in place and refuses to write into it.
- Permissions other than the executable bit, empty directories, anything outside the
  project folder (databases, remote services, other repositories).
- The user's git index (staged state), HEAD and branches: snapshots never change them.

## When a snapshot counts as successful

A snapshot is successful only when its ref was created under
`refs/holdfast/<worktree>/<name>`. Its tree then equals the files on disk at the moment
of capture, also when several Holdfast processes take snapshots of one repository at
the same time [`TestParallelSnapshotsMatchDisk`, `TestWriteTreeCachedInterleaved`].

Snapshots of different working trees (`git worktree`) live under different prefixes and
are never picked across trees [`TestWorktreesAreIsolated`].

## When it fails

- Any git error is returned to the caller; there is no partial snapshot ref.
- A failed automatic checkpoint is written to the log and to `.holdfast/checkpoint-error`,
  shown by `holdfast guard list`, and reported to the user through a Claude Code
  `systemMessage`. A failed checkpoint grants nothing that needs a restore point
  (mass deletion stays denied) [`TestFailedCheckpointIsVisibleAndGrantsNothing`].
- A nested git repository without commits makes git refuse; the error says how to fix it.

## How many are kept

- Automatic checkpoints: a new one is made only if the tree changed since the newest
  snapshot; beyond `keep_snapshots` (default 50) the oldest refs of this working tree
  are deleted [`TestCheckpointDedupAndPrune`].
- Before-rollback backups: the newest 20.
- Deleting a ref does not delete the objects at once: git removes them later with `git gc`.

## Not guaranteed

- A snapshot is not a freeze. If another program writes a file while the snapshot is
  being taken, either version of that file may land in it. Holdfast coordinates its own
  processes, but it cannot stop an external writer: stop the agent before rolling back.
- A multi-file rollback is not one atomic file-system operation. What happens after an
  interrupted rollback is roadmap item B2 and is not verified yet.
- Pruning while a rollback to the pruned snapshot is running is not covered by a test yet
  (roadmap B1, last item).

## Implementation notes

The fast path keeps a service index in `.git/holdfast/index`, so git re-hashes only
changed files. Each run copies it to a private file next to it, works on the copy and
renames the copy back. Git locks the index per command, not for the
`read-tree → add → write-tree` sequence, so a shared file let one run reset the index in
the middle of another run's sequence (fixed in roadmap B1). When the fast path fails,
the snapshot is built from scratch in a temporary index; the result is the same tree.
