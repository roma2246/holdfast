package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roma2246/holdfast/internal/config"
)

// The tests in this file check not individual examples but the properties
// Holdfast promises to the user (following an external review):
//
//   - everything a rollback overwrote or deleted is saved in the backup,
//     or else the rollback does not start;
//   - every successful snapshot holds the files as they are on disk, even
//     when several snapshot processes run at once;
//   - a rollback interrupted at any point is reported to the next process,
//     and both finishing it and undoing it give a consistent state;
//   - a selective rollback touches only the requested files, leaving all other files unchanged;
//   - a snapshot of another working tree is never picked;
//   - mass deletion is allowed only if what is deleted is in the restore point;
//   - a failed restore point does not look like a successful one;
//   - a config error does not weaken the protection;
//   - a child shell does not change the parent's context;
//   - equivalent forms of a command get the same decision: path rules
//     apply to the resulting files, push rules to the resulting branch;
//   - the agent cannot change Holdfast's service files in any form.

// diskState reads all files of the working tree (ignored ones included), except .git.
func diskState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRollbackNeverLosesData(t *testing.T) {
	scenarios := []struct {
		name string
		// before is the state at the restore point; after is what happened later.
		before, after func(t *testing.T, dir string)
	}{
		{
			name:   "regular edits, a new file and a deleted file",
			before: func(t *testing.T, dir string) { write(t, dir, "keep.txt", "v1\n") },
			after: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "agent\n")
				write(t, dir, "new/file.txt", "new\n")
				os.Remove(filepath.Join(dir, "b.txt"))
			},
		},
		{
			// The scenario from the review: the file got into .gitignore after the restore point.
			name:   "a file became ignored and changed",
			before: func(t *testing.T, dir string) { write(t, dir, "local.txt", "v1\n") },
			after: func(t *testing.T, dir string) {
				write(t, dir, ".gitignore", "*.log\nlocal.txt\n")
				write(t, dir, "local.txt", "v2-precious\n")
			},
		},
		{
			name:   "a directory with ignored contents where a file was",
			before: func(t *testing.T, dir string) { write(t, dir, "data", "file\n") },
			after: func(t *testing.T, dir string) {
				os.Remove(filepath.Join(dir, "data"))
				write(t, dir, "data/cache.log", "ignored but precious\n")
				write(t, dir, "data/notes.txt", "tracked-ish\n")
			},
		},
		{
			name:   "an ignored file where a directory was",
			before: func(t *testing.T, dir string) { write(t, dir, "conf/app.txt", "v1\n") },
			after: func(t *testing.T, dir string) {
				os.RemoveAll(filepath.Join(dir, "conf"))
				write(t, dir, ".gitignore", "*.log\nconf\n")
				write(t, dir, "conf", "now this is a file\n")
			},
		},
		{
			name:   "an ignored file that was not in the restore point is left alone",
			before: func(t *testing.T, dir string) {},
			after: func(t *testing.T, dir string) {
				write(t, dir, "debug.log", "my log\n")
				write(t, dir, "a.txt", "agent\n")
			},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			dir := gitRepo(t, true)
			sc.before(t, dir)
			g, err := OpenGit(dir)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := g.CreateSnapshot(time.Now(), SnapshotMeta{})
			if err != nil {
				t.Fatal(err)
			}
			atSnapshot := diskState(t, dir)
			sc.after(t, dir)
			beforeRollback := diskState(t, dir)

			_, backup, err := g.Rollback(snap, time.Now())
			if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			afterRollback := diskState(t, dir)

			// Property 1: every file changed or removed by the rollback is
			// saved in the backup with its previous contents.
			for path, old := range beforeRollback {
				if now, ok := afterRollback[path]; ok && now == old {
					continue
				}
				saved, err := g.run(nil, nil, "show", backup.Ref+":"+path)
				if err != nil || saved != old {
					t.Errorf("DATA LOSS: %s was changed by the rollback but is not in the backup (%v)", path, err)
				}
			}
			// Property 2: everything that was in the restore point is back.
			saved, err := g.TreeFiles(snap.Tree, nil)
			if err != nil {
				t.Fatal(err)
			}
			for path := range saved {
				if afterRollback[path] != atSnapshot[path] {
					t.Errorf("%s not restored: %q, the restore point had %q", path, afterRollback[path], atSnapshot[path])
				}
			}
			// Property 3: the rollback can be undone with the regular command.
			undo, err := g.FindSnapshot(backup.Name())
			if err != nil {
				t.Fatalf("the backup can't be found by name: %v", err)
			}
			if _, _, err := g.Rollback(undo, time.Now()); err != nil {
				t.Fatalf("undoing the rollback: %v", err)
			}
			for path, old := range beforeRollback {
				if got := diskState(t, dir)[path]; got != old {
					t.Errorf("after undoing the rollback %s = %q, was %q", path, got, old)
				}
			}
		})
	}
}

// A selective rollback touches only the requested paths and leaves everything else untouched.
func TestSelectiveRollbackPreservesUnselectedFiles(t *testing.T) {
	dir := gitRepo(t, true)
	write(t, dir, "selected_mod.txt", "v1-selected-mod\n")
	write(t, dir, "unselected_mod.txt", "v1-unselected-mod\n")
	write(t, dir, "selected_del.txt", "v1-selected-del\n")
	write(t, dir, "unselected_del.txt", "v1-unselected-del\n")
	write(t, dir, "keep_alone.txt", "v1-keep-alone\n")
	write(t, dir, "sub/nested.txt", "v1-nested\n")
	g, err := OpenGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := g.CreateSnapshot(time.Now(), SnapshotMeta{})
	if err != nil {
		t.Fatal(err)
	}

	// Agent modifications:
	write(t, dir, "selected_mod.txt", "v2-selected-mod-agent\n")
	write(t, dir, "unselected_mod.txt", "v2-unselected-mod-agent\n")
	os.Remove(filepath.Join(dir, "selected_del.txt"))
	os.Remove(filepath.Join(dir, "unselected_del.txt"))
	write(t, dir, "selected_add.txt", "v2-selected-add-agent\n")
	write(t, dir, "unselected_add.txt", "v2-unselected-add-agent\n")

	beforeRollback := diskState(t, dir)

	// Selectively rollback only selected_mod.txt, selected_del.txt, and selected_add.txt
	_, backup, err := g.Rollback(snap, time.Now(), "selected_mod.txt", "selected_del.txt", "selected_add.txt")
	if err != nil {
		t.Fatal(err)
	}
	afterRollback := diskState(t, dir)

	// Property 1: The selected files were rolled back.
	if afterRollback["selected_mod.txt"] != "v1-selected-mod\n" {
		t.Errorf("selected_mod.txt not restored: %q", afterRollback["selected_mod.txt"])
	}
	if afterRollback["selected_del.txt"] != "v1-selected-del\n" {
		t.Errorf("selected_del.txt not restored: %q", afterRollback["selected_del.txt"])
	}
	if _, ok := afterRollback["selected_add.txt"]; ok {
		t.Errorf("selected_add.txt was not deleted: %q", afterRollback["selected_add.txt"])
	}

	// Property 2: ALL unselected files remain exactly as they were before the selective rollback.
	unselectedPaths := []string{
		"unselected_mod.txt",
		"unselected_add.txt",
		"keep_alone.txt",
		"sub/nested.txt",
	}
	for _, p := range unselectedPaths {
		if afterRollback[p] != beforeRollback[p] {
			t.Errorf("unselected file %s changed: before %q, after %q", p, beforeRollback[p], afterRollback[p])
		}
	}
	// unselected_del.txt must still be deleted
	if _, ok := afterRollback["unselected_del.txt"]; ok {
		t.Errorf("unselected_del.txt should remain deleted")
	}

	// Property 3: The backup preserves the state before rollback and can be undone.
	undo, err := g.FindSnapshot(backup.Name())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.Rollback(undo, time.Now()); err != nil {
		t.Fatal(err)
	}
	afterUndo := diskState(t, dir)
	for path, old := range beforeRollback {
		if afterUndo[path] != old {
			t.Errorf("after undoing selective rollback %s = %q, was %q", path, afterUndo[path], old)
		}
	}
}

// If what would be overwritten can't be saved, the rollback does not start.
func TestRollbackStopsWhenBackupImpossible(t *testing.T) {
	dir := gitRepo(t, true)
	write(t, dir, "vendor", "file\n")
	g, _ := OpenGit(dir)
	snap, err := g.CreateSnapshot(time.Now(), SnapshotMeta{})
	if err != nil {
		t.Fatal(err)
	}
	// A nested git repository where a file was: git won't save its contents.
	os.Remove(filepath.Join(dir, "vendor"))
	write(t, dir, "vendor/lib.txt", "someone else's code\n")
	git(t, filepath.Join(dir, "vendor"), "init", "-q")
	git(t, filepath.Join(dir, "vendor"), "add", "-A")
	git(t, filepath.Join(dir, "vendor"), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "nested")
	before := diskState(t, dir)

	if _, _, err := g.Rollback(snap, time.Now()); err == nil || !strings.Contains(err.Error(), "rollback is cancelled") {
		t.Fatalf("expected the rollback to be refused, got: %v", err)
	}
	after := diskState(t, dir)
	if len(before) != len(after) || after["vendor/lib.txt"] != "someone else's code\n" {
		t.Fatalf("the working tree changed after the refusal: %v", after)
	}

	// A nested repository that appeared after the restore point in a new place
	// is left as is by the rollback, and the rest is rolled back.
	dir2 := gitRepo(t, true)
	g2, _ := OpenGit(dir2)
	snap2, _ := g2.CreateSnapshot(time.Now(), SnapshotMeta{})
	write(t, dir2, "cloned/lib.txt", "clone\n")
	git(t, filepath.Join(dir2, "cloned"), "init", "-q")
	git(t, filepath.Join(dir2, "cloned"), "add", "-A")
	git(t, filepath.Join(dir2, "cloned"), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "nested")
	write(t, dir2, "a.txt", "agent\n")
	plan, err := g2.PlanRollback(snap2)
	if err != nil || len(plan.Skipped) != 1 || plan.Skipped[0] != "cloned" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if _, _, err := g2.Rollback(snap2, time.Now()); err != nil {
		t.Fatal(err)
	}
	if read(t, dir2, "a.txt") != "original a\n" || read(t, dir2, "cloned/lib.txt") != "clone\n" {
		t.Fatal("a.txt must be rolled back, the nested repository must stay")
	}
}

func TestWorktreesAreIsolated(t *testing.T) {
	main := gitRepo(t, true)
	other := filepath.Join(t.TempDir(), "second")
	git(t, main, "worktree", "add", "-q", other, "-b", "second")
	write(t, other, "a.txt", "second worktree\n")

	gMain, _ := OpenGit(main)
	gOther, _ := OpenGit(other)
	if gMain.worktreeID() != "main" || !strings.HasPrefix(gOther.worktreeID(), "wt-") {
		t.Fatalf("identifiers: %q %q", gMain.worktreeID(), gOther.worktreeID())
	}
	snapOther, err := gOther.CreateSnapshot(time.Now(), SnapshotMeta{})
	if err != nil {
		t.Fatal(err)
	}
	// The second tree's snapshot is not visible from the first, neither in the list nor by name.
	if snaps, _ := gMain.Snapshots(); len(snaps) != 0 {
		t.Fatalf("a foreign snapshot is visible in the main tree: %+v", snaps)
	}
	if _, err := gMain.FindSnapshot(""); err == nil {
		t.Fatal("FindSnapshot picked another tree's snapshot")
	}
	if _, err := gMain.FindSnapshot(snapOther.Ref); err == nil {
		t.Fatal("another tree's snapshot was found by its full ref")
	}
	// Even when the snapshot is passed in directly, the rollback refuses.
	if _, _, err := gMain.Rollback(snapOther, time.Now()); err == nil || !strings.Contains(err.Error(), "another working tree") {
		t.Fatalf("rollback to a foreign snapshot: %v", err)
	}
	if got := read(t, main, "a.txt"); got != "original a\n" {
		t.Fatalf("the main tree was changed: %q", got)
	}
	// Pruning one tree does not touch the other's snapshots.
	if _, err := gMain.CreateSnapshot(time.Now(), SnapshotMeta{}); err != nil {
		t.Fatal(err)
	}
	write(t, main, "a.txt", "main v2\n")
	if _, err := gMain.CreateSnapshot(time.Now(), SnapshotMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := gMain.Prune(1); err != nil {
		t.Fatal(err)
	}
	mainSnaps, _ := gMain.Snapshots()
	otherSnaps, _ := gOther.Snapshots()
	if len(mainSnaps) != 1 || len(otherSnaps) != 1 {
		t.Fatalf("after pruning: main=%d other=%d", len(mainSnaps), len(otherSnaps))
	}
}

// Parallel snapshots and checkpoints (with pruning) on one repository: each
// one either fails visibly or holds exactly what is on disk (roadmap B1).
// The files differ from HEAD, so a snapshot built from an index that another
// run reset to HEAD would show up here.
func TestParallelSnapshotsMatchDisk(t *testing.T) {
	dir := gitRepo(t, true)
	write(t, dir, "a.txt", "agent edit a\n")
	write(t, dir, "b.txt", "agent edit b\n")
	write(t, dir, "new/c.txt", "untracked c\n")
	want := diskState(t, dir)
	g, err := OpenGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	const workers, rounds = 8, 5
	var (
		mu    sync.Mutex
		snaps []Snapshot
		errs  []error
		wg    sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				var s Snapshot
				var err error
				if (w+r)%2 == 0 {
					s, err = g.CreateSnapshot(time.Now(), SnapshotMeta{})
				} else {
					s, _, err = g.Checkpoint(time.Now(), SnapshotMeta{Kind: KindCheckpoint}, 3)
				}
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				} else {
					snaps = append(snaps, s)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	for _, err := range errs {
		t.Errorf("a parallel snapshot failed: %v", err)
	}
	for _, s := range snaps {
		files, err := g.TreeFiles(s.Tree, nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, content := range want {
			if !files[name] {
				t.Errorf("snapshot %s lacks %s", s.Name(), name)
				continue
			}
			if got := git(t, dir, "cat-file", "-p", s.Tree+":"+name); got != content {
				t.Errorf("snapshot %s holds %q for %s, the disk has %q", s.Name(), got, name, content)
			}
		}
	}
	if diskState(t, dir)["a.txt"] != want["a.txt"] {
		t.Fatal("snapshots changed the working tree")
	}
}

// rollbackFixture: a snapshot of the original state, then an agent edits a
// file, deletes one and adds a few (also in a new directory).
func rollbackFixture(t *testing.T) (dir string, snap Snapshot, original, edited map[string]string) {
	t.Helper()
	dir = gitRepo(t, true)
	g, _ := OpenGit(dir)
	original = diskState(t, dir)
	snap, err := g.CreateSnapshot(time.Now(), SnapshotMeta{})
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "broken by the agent\n")
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "c.txt", "new c\n")
	write(t, dir, "gen/d.txt", "new d\n")
	write(t, dir, "gen/e.txt", "new e\n")
	return dir, snap, original, diskState(t, dir)
}

func sameState(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s:\n got  %v\n want %v", what, got, want)
	}
}

// Roadmap B2: the rollback stops at each point where a crash can happen; a new
// process (a fresh Git value, only the disk is shared) must see it, refuse a
// regular rollback, keep the snapshots it needs, and both --resume and
// --abort must end in a consistent state that is checked file by file.
func TestInterruptedRollbackIsRecoverable(t *testing.T) {
	points := []struct {
		step string
		nth  int // stop at the nth time this step is reached
	}{
		{"journaled", 1},
		{"deleted", 1}, // one added file deleted, two left
		{"deleted", 2},
		{"restored", 1}, // everything written, not verified yet
	}
	for _, p := range points {
		for _, mode := range []string{"resume", "abort"} {
			t.Run(fmt.Sprintf("%s#%d/%s", p.step, p.nth, mode), func(t *testing.T) {
				dir, snap, original, edited := rollbackFixture(t)
				g, _ := OpenGit(dir)
				seen := 0
				rollbackStep = func(step string) error {
					if step == p.step {
						if seen++; seen == p.nth {
							return errors.New("simulated crash")
						}
					}
					return nil
				}
				_, backup, err := g.Rollback(snap, time.Now())
				rollbackStep = nil
				if err == nil || !strings.Contains(err.Error(), "stopped halfway") || !strings.Contains(err.Error(), backup.Name()) {
					t.Fatalf("the interruption must be reported with the backup name: %v", err)
				}

				g2, _ := OpenGit(dir)
				j, err := g2.PendingRollback()
				if err != nil || j == nil || j.Target.Commit != snap.Commit || j.Backup.Commit != backup.Commit {
					t.Fatalf("the next process does not see the interrupted rollback: %+v, %v", j, err)
				}
				if _, _, err := g2.Rollback(snap, time.Now()); !errors.Is(err, ErrRollbackPending) {
					t.Fatalf("a regular rollback must refuse while one is pending: %v", err)
				}
				// Pruning keeps what the pending rollback needs.
				if err := g2.Prune(1); err != nil {
					t.Fatal(err)
				}
				write(t, dir, "trigger.txt", "x")
				if _, err := g2.CreateSnapshot(time.Now(), SnapshotMeta{}); err != nil {
					t.Fatal(err)
				}
				os.Remove(filepath.Join(dir, "trigger.txt"))
				if err := g2.Prune(1); err != nil {
					t.Fatal(err)
				}
				if !g2.refExists(snap.Ref) {
					t.Fatal("pruning deleted the target of the pending rollback")
				}

				target, want := j.Target.snapshot(dir), original
				if mode == "abort" {
					target, want = j.Backup.snapshot(dir), edited
				}
				if _, _, err := g2.ContinueRollback(target, time.Now()); err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
				sameState(t, mode, diskState(t, dir), want)
				if j, err := g2.PendingRollback(); j != nil || err != nil {
					t.Fatalf("the journal stays after a successful %s: %+v %v", mode, j, err)
				}
			})
		}
	}
}

// A restored file that does not match the snapshot (here: overwritten right
// after the restore) is caught by the check, and the rollback is not reported as done.
func TestRollbackVerifiesResult(t *testing.T) {
	dir, snap, original, _ := rollbackFixture(t)
	g, _ := OpenGit(dir)
	rollbackStep = func(step string) error {
		if step == "restored" {
			write(t, dir, "a.txt", "written by someone else\n")
		}
		return nil
	}
	_, _, err := g.Rollback(snap, time.Now())
	rollbackStep = nil
	if err == nil || !strings.Contains(err.Error(), "a.txt (differs from the snapshot)") {
		t.Fatalf("expected a verification error about a.txt: %v", err)
	}
	if j, _ := g.PendingRollback(); j == nil {
		t.Fatal("a failed verification must keep the journal")
	}
	if _, _, err := g.ContinueRollback(snap, time.Now()); err != nil {
		t.Fatal(err)
	}
	sameState(t, "after resume", diskState(t, dir), original)
}

// Roadmap S08: when the journal can't be written, no file is changed.
func TestRollbackDoesNotStartWithoutJournal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir, snap, _, edited := rollbackFixture(t)
	g, _ := OpenGit(dir)
	if _, err := g.WriteTree(); err != nil { // creates .git/holdfast
		t.Fatal(err)
	}
	hf := filepath.Join(gitDir(dir), "holdfast")
	if err := os.Chmod(hf, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(hf, 0o755)
	_, _, err := g.Rollback(snap, time.Now())
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("expected a refusal: %v", err)
	}
	sameState(t, "files after a refused rollback", diskState(t, dir), edited)
}

// A damaged journal still blocks a rollback and is reported, not ignored.
func TestDamagedJournalIsReported(t *testing.T) {
	dir, snap, _, edited := rollbackFixture(t)
	g, _ := OpenGit(dir)
	write(t, filepath.Join(gitDir(dir), "holdfast"), "rollback.json", "{half")
	if _, err := g.PendingRollback(); !errors.Is(err, ErrRollbackPending) {
		t.Fatalf("a damaged journal must be reported: %v", err)
	}
	if _, _, err := g.Rollback(snap, time.Now()); !errors.Is(err, ErrRollbackPending) {
		t.Fatalf("a damaged journal must block a rollback: %v", err)
	}
	sameState(t, "files", diskState(t, dir), edited)
}

// Two rollbacks at once: no error other than "another rollback started", and
// the result is the snapshot with no journal left over.
func TestConcurrentRollbacks(t *testing.T) {
	dir, snap, original, _ := rollbackFixture(t)
	g, _ := OpenGit(dir)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = g.Rollback(snap, time.Now())
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), "another rollback started") || errors.Is(err, ErrRollbackPending):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok == 0 {
		t.Fatal("no rollback succeeded")
	}
	sameState(t, "after concurrent rollbacks", diskState(t, dir), original)
	if j, err := g.PendingRollback(); j != nil || err != nil {
		t.Fatalf("a journal is left: %+v %v", j, err)
	}
}

func TestSweepTemp(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := filepath.Join(dir, fmt.Sprintf("index-%d-1.tmp", now.Add(-2*time.Hour).Unix()))
	fresh := filepath.Join(dir, fmt.Sprintf("index-%d-2.tmp", now.Unix()))
	other := filepath.Join(dir, "index")
	for _, p := range []string{old, fresh, other} {
		write(t, dir, filepath.Base(p), "x")
	}
	sweepTemp(dir, now.Add(-time.Hour))
	for p, want := range map[string]bool{old: false, fresh: true, other: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

func TestSnapshotOrder(t *testing.T) {
	dir := gitRepo(t, true)
	g, _ := OpenGit(dir)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) // all snapshots "at the same instant"
	var last Snapshot
	for i := 0; i < 15; i++ {
		write(t, dir, "a.txt", fmt.Sprintf("v%d\n", i))
		s, err := g.CreateSnapshot(now, SnapshotMeta{})
		if err != nil {
			t.Fatal(err)
		}
		last = s
		newest, err := g.FindSnapshot("")
		if err != nil || newest.Ref != last.Ref {
			t.Fatalf("step %d: %s was picked as the newest, but %s was created last", i, newest.Name(), last.Name())
		}
	}
	// Snapshots in the old format (…-000, …-000-2, …-000-12) are in order too.
	names := []string{"20261114-134625-000", "20261114-134625-000-2", "20261114-134625-000-9", "20261114-134625-000-12"}
	for i := 1; i < len(names); i++ {
		if nameKey(names[i-1]) >= nameKey(names[i]) {
			t.Errorf("order of old names: %s must come before %s", names[i-1], names[i])
		}
	}
	if nameKey("20261114-134625-000-12") >= nameKey("20261114-134626-000000001") {
		t.Error("the new format must sort after an old one from the previous second")
	}
	// An old snapshot in refs/holdfast/<name> is visible in the main tree.
	git(t, dir, "update-ref", "refs/holdfast/20200101-000000-000", last.Commit)
	snaps, _ := g.Snapshots()
	if got := snaps[len(snaps)-1].Name(); got != "20200101-000000-000" {
		t.Errorf("the old snapshot must be last in the list, but %s is", got)
	}
}

// realProject is a real git repository for checking reversibility.
func realProject(t *testing.T) (*Policy, string) {
	t.Helper()
	dir := gitRepo(t, false)
	write(t, dir, ".gitignore", "*.log\nlocal/\nnode_modules/\n")
	for i := 0; i < 7; i++ {
		write(t, dir, fmt.Sprintf("assets/%d.png", i), "tracked")
	}
	git(t, dir, "add", "-A")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	for i := 0; i < 7; i++ {
		write(t, dir, fmt.Sprintf("drafts/%d.md", i), "untracked")
		write(t, dir, fmt.Sprintf("local/%d.bin", i), "ignored")
		write(t, dir, fmt.Sprintf("node_modules/pkg/%d.js", i), "ignored, regenerable")
	}
	p := NewPolicy(dir, dir, &config.GuardConfig{})
	p.Home = fakeHome
	p.Session = "s1"
	p.tempDirs = nil // the fixtures live in a temp directory
	return p, dir
}

func TestMassDeleteOnlyWhenCovered(t *testing.T) {
	p, dir := realProject(t)
	outside := gitRepo(t, true)
	for i := 0; i < 7; i++ {
		write(t, outside, fmt.Sprintf("x%d.txt", i), "x")
	}
	tests := []struct {
		cmd  string
		want Decision
		rule string
	}{
		{"rm -r assets", Allow, ""},                      // tracked by git → will be in the restore point
		{"rm -r drafts", Allow, ""},                      // new, not ignored → will be in the restore point
		{"rm drafts/*.md", Allow, ""},                    // the same through a glob
		{"rm -rf node_modules", Allow, ""},               // a disposable directory
		{"rm -rf local", Deny, "mass-delete"},            // ignored → the restore point won't bring it back
		{"rm -rf assets local", Deny, "mass-delete"},     // a mixed case
		{"find assets -name '*.png' -delete", Allow, ""}, // only tracked files match the filter
		{"find . -name '*.bin' -delete", Deny, "mass-delete"},
		{"find . -type f -delete", Deny, "delete-git-dir"}, // would delete .git with all the restore points too
		{"find . -name 'HEAD' -delete", Deny, "delete-git-dir"},
		{"rm -rf .git", Deny, "delete-git-dir"},
		{"find " + outside + " -type f -delete", Deny, "rm-outside-repo"},
		{"find .. -type f -delete", Deny, "rm-outside-repo"},
		{"find $DIR -delete", Deny, "rm-dynamic"},
		{"git clean -fd", Allow, ""},                            // 7 untracked → in the restore point
		{"git -C " + outside + " clean -fd", Deny, "git-clean"}, // someone else's repository
		{"cd " + outside + " && git clean -fd", Deny, "git-clean"},
		{"git clean -fdx", Deny, "git-clean"},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got := p.CheckCommand(tt.cmd)
			if got.Decision != tt.want || got.Rule != tt.rule {
				t.Fatalf("%q = %s [%s: %s], want %s [%s]", tt.cmd, got.Decision, got.Rule, got.Reason, tt.want, tt.rule)
			}
		})
	}

	// The property: if a mass deletion is allowed, every file being deleted
	// really is in the most recent restore point, the one the hook itself took.
	g, _ := OpenGit(dir)
	snaps, err := g.Snapshots()
	if err != nil || len(snaps) == 0 || snaps[0].Meta.Kind != KindPreDelete || snaps[0].Meta.Session != "s1" {
		t.Fatalf("no restore point was created before the deletion: %+v %v", snaps, err)
	}
	saved, _ := g.TreeFiles(snaps[0].Tree, nil)
	for i := 0; i < 7; i++ {
		for _, f := range []string{fmt.Sprintf("assets/%d.png", i), fmt.Sprintf("drafts/%d.md", i)} {
			if !saved[f] {
				t.Errorf("%s is not in the restore point although the deletion was allowed", f)
			}
		}
		if saved[fmt.Sprintf("local/%d.bin", i)] {
			t.Errorf("an ignored file ended up in the restore point")
		}
	}
	// The hint on denial explains the reason and suggests no workarounds.
	v := p.CheckCommand("rm -rf local")
	if !strings.Contains(v.Reason, "local/0.bin") || !strings.Contains(v.Reason, "ask the user") {
		t.Errorf("hint: %s", v.Reason)
	}
}

func TestFailedCheckpointIsVisibleAndGrantsNothing(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	p, dir := realProject(t)
	env := testEnv(dir)
	in := `{"session_id":"s1","cwd":"` + dir + `","prompt":"first prompt"}`
	var out bytes.Buffer
	RunCheckpoint(strings.NewReader(in), &out, env)
	if out.Len() != 0 || LastCheckpointError(dir) != "" {
		t.Fatalf("a successful restore point must be silent: %q %q", out.String(), LastCheckpointError(dir))
	}

	// The next restore point can't be written.
	refs := filepath.Join(dir, ".git", "refs", "holdfast")
	if err := filepath.WalkDir(refs, func(path string, d fs.DirEntry, _ error) error {
		if d != nil && d.IsDir() {
			return os.Chmod(path, 0o500)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(refs, func(path string, d fs.DirEntry, _ error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	write(t, dir, "assets/0.png", "changed after the first restore point")
	RunCheckpoint(strings.NewReader(in), &out, env)

	var msg struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal(out.Bytes(), &msg); err != nil || !strings.Contains(msg.SystemMessage, "NO restore point was created") {
		t.Fatalf("the user was not warned: %q (%v)", out.String(), err)
	}
	if LastCheckpointError(dir) == "" {
		t.Fatal("the restore point error was not saved for guard list")
	}
	entries, _ := ReadLog(dir)
	if last := entries[len(entries)-1]; last.Rule != "error" {
		t.Fatalf("the log has no error: %+v", last)
	}
	// An old restore point gives no right to a mass deletion: the hook tries
	// to take a new one, can't, and denies.
	if v := p.CheckCommand("rm -r assets"); v.Decision != Deny || !strings.Contains(v.Reason, "could not save a restore point") {
		t.Fatalf("after a failed restore point: %s [%s]", v.Decision, v.Reason)
	}
}

func TestBrokenConfigDoesNotWeakenProtection(t *testing.T) {
	root := t.TempDir()
	write(t, root, "holdfast.toml", "[guard]\nprotected_paths = [\"migrations/**\"]\ndeny_commands = [\"terraform destroy\"]\n")
	target := map[string]any{"file_path": filepath.Join(root, "migrations", "001.sql"), "content": "x"}
	decide := func(tool string, input map[string]any) (string, string) {
		t.Helper()
		var out bytes.Buffer
		RunHook(bytes.NewReader(hookJSON(t, root, tool, input)), &out, testEnv(root))
		if out.Len() == 0 {
			return "allow", ""
		}
		var resp hookOutput
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.HookSpecificOutput.PermissionDecision, resp.HookSpecificOutput.PermissionDecisionReason
	}
	before := map[string]string{}
	cases := map[string]map[string]any{
		"write-protected": target,
		"deny-command":    {"command": "terraform destroy"},
		"plain-command":   {"command": "go test ./..."},
		"builtin-deny":    {"command": "rm -rf ~"},
	}
	tools := map[string]string{"write-protected": "Write", "deny-command": "Bash", "plain-command": "Bash", "builtin-deny": "Bash"}
	for name, input := range cases {
		before[name], _ = decide(tools[name], input)
	}
	if before["write-protected"] != "ask" || before["deny-command"] != "deny" || before["plain-command"] != "allow" {
		t.Fatalf("baseline decisions: %v", before)
	}

	// A typo in the config (two variants): the protection must not get weaker in either case.
	rank := map[string]int{"allow": 0, "ask": 1, "deny": 2}
	for _, broken := range []string{
		"[guard]\nmode = \"typo\"\nprotected_paths = [\"migrations/**\"]\ndeny_commands = [\"terraform destroy\"]\n",
		"[guard\nprotected_paths = [\"migrations/**\"]\n",
	} {
		write(t, root, "holdfast.toml", broken)
		for name, input := range cases {
			got, reason := decide(tools[name], input)
			// deny_commands is unavailable with an unreadable config, but the
			// action at least needs confirmation instead of passing silently.
			floor := before[name]
			if floor == "deny" && name == "deny-command" {
				floor = "ask"
			}
			if rank[got] < rank[floor] || (got == "allow") {
				t.Errorf("%s: with a broken config %s (was %s): the protection got weaker", name, got, before[name])
			}
			if got == "ask" && !strings.Contains(reason, "holdfast.toml") {
				t.Errorf("%s: the reason does not explain the config problem: %s", name, reason)
			}
		}
		if got, _ := decide("Read", map[string]any{"file_path": filepath.Join(root, "README.md")}); got != "allow" {
			t.Errorf("read with a broken config: %s", got)
		}
	}
	entries, _ := ReadLog(root)
	found := false
	for _, e := range entries {
		found = found || e.Rule == "config-error"
	}
	if !found {
		t.Error("the log has no config-error event")
	}
}

// Wrappers and equivalent forms of a command get the same decision.
func TestEquivalentFormsGetSameDecision(t *testing.T) {
	p := autoPolicy(t, fakeRepo(t, "ref: refs/heads/dev"))
	for _, cmd := range []string{
		"rm -rf ~",
		"env rm -rf ~",
		"env -u TEST rm -rf ~",
		"env -i FOO=1 rm -rf ~",
		"env --unset=TEST -C /tmp rm -rf ~",
		`env -S "rm -rf ~"`,
		"sudo env -u X rm -rf ~",
		"command rm -rf ~",
		"nohup env -u A -u B rm -rf ~",
	} {
		if got := p.CheckCommand(cmd); got.Decision != Deny || got.Rule != "rm-home" {
			t.Errorf("%q = %s [%s], want deny [rm-home]", cmd, got.Decision, got.Rule)
		}
	}
	if got := p.CheckCommand("env -u TEST go test ./..."); got.Decision != Allow {
		t.Errorf("a harmless command through env: %s", got.Decision)
	}
}

func TestInstallEdgeCases(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".claude/settings.json", "null")
	if _, changed, err := InstallHook(root, false, ""); err != nil || !changed {
		t.Fatalf("settings.json = null: %v %v", changed, err)
	}
	// A path with a space is quoted: otherwise the shell would split it into two words.
	other := t.TempDir()
	path, _, err := InstallHook(other, false, "/Users/me/My Tools/holdfast")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `'/Users/me/My Tools/holdfast' guard check`) {
		t.Fatalf("the path is not quoted: %s", data)
	}
	if got := shellQuoteArg("it's"); got != `'it'\''s'` {
		t.Errorf("quote: %s", got)
	}
	if _, changed, _ := InstallHook(other, false, "/Users/me/My Tools/holdfast"); changed {
		t.Error("installing again with the same path must change nothing")
	}
}

// A child shell (( … ), $( … ), <( … ), bash -c, part of a pipeline,
// a background command, env -C) does not change the directory for the commands that follow.
func TestChildShellDoesNotChangeParentContext(t *testing.T) {
	p := autoPolicy(t, fakeRepo(t, "ref: refs/heads/dev"))
	children := []string{
		"(cd %s)", "(cd %s; ls)", "bash -c 'cd %s'", `sh -c "cd %s && ls"`, "zsh -c 'cd %s'",
		"echo $(cd %s; pwd)", "cat <(cd %s; ls)", "cd %s &", "cd %s | cat", "ls | cd %s",
		"env -C %s ls", "sudo -D %s ls", "f() { cd %s; }", "pushd %s >/dev/null; popd",
	}
	follow := []string{"rm -rf projects", "rm -rf docs/old", "git push origin dev", "rm -rf build"}
	for _, dir := range []string{"/", "~", "/etc"} {
		for _, c := range children {
			child := fmt.Sprintf(c, dir)
			for _, f := range follow {
				want := p.CheckCommand(f)
				seps := []string{"; ", " && ", "\n"}
				switch {
				case strings.HasSuffix(child, "&"):
					seps = []string{"\n"} // "cmd &; …" is a syntax error
				case strings.HasSuffix(child, "}"):
					// in "f() { … } && x" the parser attaches x to the function definition
					seps = []string{"; ", "\n"}
				}
				for _, sep := range seps {
					cmd := child + sep + f
					if got := p.CheckCommand(cmd); got.Decision != want.Decision || got.Rule != want.Rule {
						t.Errorf("%q = %s [%s], but without the child cd it is %s [%s]", cmd, got.Decision, got.Rule, want.Decision, want.Rule)
					}
				}
			}
		}
	}
	// Control: a cd in the shell itself changes the directory, a child shell
	// inherits it, and calling a function with a cd makes the directory unknown.
	for _, cmd := range []string{
		"cd / && rm -rf projects",
		"cd / && (rm -rf projects)",
		"cd / && bash -c 'rm -rf projects'",
		"pushd / && rm -rf projects",
		"f() { cd /; }; f; rm -rf projects",
		"env -C / rm -rf projects",
	} {
		if got := p.CheckCommand(cmd); got.Decision != Deny {
			t.Errorf("%q = %s [%s], want deny", cmd, got.Decision, got.Rule)
		}
	}
}

// pathProject is a project with a secret, a restriction on go.sum, a protected
// path and symlinks to them.
func pathProject(t *testing.T) (*Policy, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		".env": "SECRET=1", "go.sum": "x", "notes.txt": "n", "migrations/001.sql": "x",
		"other/go.sum": "y", "other/readme.md": "r", "sub/keep.txt": "k",
	} {
		write(t, dir, name, content)
	}
	for link, target := range map[string]string{"alias.txt": ".env", "lock.link": "go.sum", "dangling.txt": ".env.local"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPolicy(dir, dir, &config.GuardConfig{
		ProtectedPaths: []string{"migrations/**"},
		Synced: config.SyncedConfig{Rules: []config.SyncedRule{
			{Kind: config.KindDenyPath, Pattern: "go.sum", Source: "CLAUDE.md:1"},
		}},
	})
	p.Home = fakeHome
	p.tempDirs = nil
	return p, dir
}

// Path and secret restrictions apply to the resulting file: a glob, {…},
// a symlink and cp into a directory get the same decision as the direct path.
func TestEquivalentPathFormsGetSameDecision(t *testing.T) {
	p, _ := pathProject(t)
	groups := []struct {
		want  Decision
		rule  string
		forms []string
	}{
		{Deny, "secret-file", []string{
			"cat .env", "cat .en[v]", "cat .e?v", "cat .e{n,x}v", "cat ./sub/../.env", "cat alias.txt",
			"head -n1 .en*", "cat < .en[v]", "grep SECRET alia?.txt", "cat dangling.txt", "cat *.txt",
		}},
		{Deny, "synced-deny-path", []string{
			"echo x > go.sum", "echo x > go.s[u]m", "echo x > go.{sum,mod}", "rm go.su*", "echo x > lock.link",
			"cp other/go.sum .", "cp other/go.sum ./", "cp other/go.sum sub/..", "cp -t . other/go.sum",
			"cp --target-directory=. other/go.sum", "mv notes.txt go.sum", "cp -r other/. .", "rsync -a other/ .",
			"sed -i s/a/b/ go.su?", "tee lock.link", "ln -sf notes.txt go.sum", "dd if=/dev/zero of=go.sum",
		}},
		{Ask, "protected-path", []string{
			"touch migrations/002.sql", "cp notes.txt migrations/", "cp notes.txt migr*/", "rm -r migrations",
			"echo x > migrations/00[1].sql", "rm -r migr*",
		}},
		{Allow, "", []string{
			"cat notes.txt", "cat 'note?.txt'", "ls -la", "cp other/readme.md .", "rm other/go.sum",
			"echo x > sub/new.txt", "cat sub/*.txt", "cp notes.txt sub/",
		}},
	}
	for _, g := range groups {
		for _, cmd := range g.forms {
			if got := p.CheckCommand(cmd); got.Decision != g.want || got.Rule != g.rule {
				t.Errorf("%q = %s [%s], want %s [%s]", cmd, got.Decision, got.Rule, g.want, g.rule)
			}
		}
	}
	// The Read/Edit/Write tools follow symlinks too.
	if got := p.CheckFile(OpRead, "alias.txt"); got.Rule != "secret-file" {
		t.Errorf("Read alias.txt = %s [%s], want secret-file", got.Decision, got.Rule)
	}
	if got := p.CheckFile(OpWrite, "lock.link"); got.Rule != "synced-deny-path" {
		t.Errorf("Edit lock.link = %s [%s], want synced-deny-path", got.Decision, got.Rule)
	}
}

// Holdfast's service files (the protection settings and the restore points)
// can't be changed by the agent, neither by an edit nor by a command; reading them is fine.
func TestServiceFilesAreProtected(t *testing.T) {
	p, dir := realProject(t)
	write(t, dir, "holdfast.toml", "[guard]\n")
	write(t, dir, ".claude/settings.json", "{}")
	write(t, dir, ".holdfast/checkpoints/s1", "x")
	if err := os.Symlink("holdfast.toml", filepath.Join(dir, "cfg.link")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "update-ref", "refs/holdfast/main/x", "HEAD")

	files := []string{
		"holdfast.toml", ".claude/settings.json", ".claude/settings.local.json", ".holdfast/checkpoints/s1",
		".git/refs/holdfast/main/x", ".git/packed-refs", "~/.claude/settings.json", "cfg.link", "holdfast.tom?",
	}
	forms := []string{
		"echo x > %s", "echo x >> %s", "sed -i s/a/b/ %s", "cp drafts/0.md %s", "mv %s /tmp/x", "rm %s",
		"tee %s", "truncate -s0 %s", "rm -f %s", "chmod 000 %s", "ln -sf /dev/null %s", "(echo x > %s)",
		"bash -c 'echo x > %s'",
	}
	for _, f := range files {
		for _, form := range forms {
			cmd := fmt.Sprintf(form, f)
			if got := p.CheckCommand(cmd); got.Decision != Deny {
				t.Errorf("%q = %s [%s], want deny", cmd, got.Decision, got.Rule)
			}
		}
		if strings.ContainsAny(f, "?") {
			continue
		}
		if got := p.CheckFile(OpWrite, f); got.Decision != Deny || got.Rule != "holdfast-config" {
			t.Errorf("Edit %s = %s [%s], want deny [holdfast-config]", f, got.Decision, got.Rule)
		}
	}
	for cmd, want := range map[string]Decision{
		"rm -rf .claude":                              Deny,
		"mv .claude /tmp/c":                           Deny,
		"rm -rf .holdfast":                            Deny,
		"chmod -R 000 .holdfast":                      Deny,
		"cp -r /tmp/x .holdfast/":                     Deny,
		"holdfast sync":                               Ask,
		"holdfast sync --yes":                         Ask,
		"holdfast -C . init":                          Ask,
		"holdfast guard install":                      Ask,
		"holdfast guard uninstall --local":            Ask,
		"holdfast -C . guard uninstall --purge --yes": Ask,
		"holdfast sync --dry-run":                     Allow,
		"cat holdfast.toml":                           Allow,
		"mkdir -p .claude/agents":                     Allow,
		"echo x > .claude/notes":                      Allow,
	} {
		if got := p.CheckCommand(cmd); got.Decision != want {
			t.Errorf("%q = %s [%s], want %s", cmd, got.Decision, got.Rule, want)
		}
	}
	if got := p.CheckFile(OpRead, "holdfast.toml"); got.Decision != Allow {
		t.Errorf("Read holdfast.toml = %s, want allow", got.Decision)
	}
	// strict: the user decides on the edit.
	strict := NewPolicy(dir, dir, &config.GuardConfig{Mode: config.ModeStrict})
	strict.Home = fakeHome
	if got := strict.CheckFile(OpWrite, "holdfast.toml"); got.Decision != Ask || got.Rule != "holdfast-config" {
		t.Errorf("strict Edit holdfast.toml = %s [%s], want ask", got.Decision, got.Rule)
	}
}

// pushRepo is a "repository" on the branch dev with the branches main and dev
// and, optionally, a .git/config configuration.
func pushRepo(t *testing.T, gitConfig string, extra map[string]string) *Policy {
	t.Helper()
	root := fakeRepo(t, "ref: refs/heads/dev")
	write(t, root, ".git/refs/heads/main", "1111111111111111111111111111111111111111\n")
	write(t, root, ".git/refs/heads/dev", "2222222222222222222222222222222222222222\n")
	if gitConfig != "" {
		write(t, root, ".git/config", gitConfig)
	}
	for name, content := range extra {
		write(t, root, name, content)
	}
	return autoPolicy(t, root)
}

// A push to a protected branch is denied however the destination is given;
// if the destination can't be determined, a separate denial with a hint applies.
func TestPushDestinationIsResolved(t *testing.T) {
	p := pushRepo(t, "", nil)
	cases := []struct {
		want  string // the rule; "" means allowed
		forms []string
	}{
		{"git-push-protected", []string{
			"git push origin main", "git push origin HEAD:main", "git push origin dev:refs/heads/main",
			"git -c remote.origin.push=HEAD:main push", "git -c Remote.origin.PUSH=HEAD:main push",
			"git -c remote.origin.push=refs/heads/dev:refs/heads/main push origin",
			"git -c remote.origin.push=refs/heads/dev:refs/heads/main push origin dev",
			"git -c push.default=upstream -c branch.dev.merge=refs/heads/main push",
			"git switch main && git push", "git checkout main; git push origin HEAD", "git branch -m main && git push",
			"(git switch main); git push", "git -C . switch main && git push",
		}},
		{"git-push-unclear", []string{
			"git -c push.default=matching push", "git push origin $BRANCH", "git push $REMOTE",
			"git -c remote.origin.push=$X push", "git --config-env=remote.origin.push=V push",
			"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=remote.origin.push GIT_CONFIG_VALUE_0=HEAD:main git push",
			"env GIT_DIR=/x git push origin dev", "export GIT_CONFIG_GLOBAL=/tmp/x; git push",
			"git config remote.origin.push HEAD:main && git push", "echo '[push]' >> .git/config && git push",
			"git checkout $B && git push", "git checkout v1.0 && git push", "git checkout - && git push",
			"git push origin 'refs/heads/*:refs/heads/*'", "git push origin :",
			"git switch -c x origin/main && git push",
		}},
		{"git-push-force", []string{"git -c remote.origin.mirror=true push", "git -c remote.origin.push=+HEAD:dev push"}},
		{"git-push-tags", []string{"git -c push.followTags=true push origin dev"}},
		{"", []string{
			"git push origin dev", "git push", "git push origin HEAD", "git -c push.default=current push",
			"git switch -c feature && git push -u origin feature", "git -c core.autocrlf=false push origin dev",
			"git switch feature && git push", "GIT_SSH_COMMAND='ssh -i k' git push origin dev",
			"git -c push.followTags=true push --no-follow-tags origin dev", "git config --get push.default && git push",
			"git checkout notes.md && git push",
		}},
	}
	write(t, p.Root, "notes.md", "x")
	for _, c := range cases {
		for _, cmd := range c.forms {
			got := p.CheckCommand(cmd)
			if c.want == "" && got.Decision != Allow || c.want != "" && (got.Decision != Deny || got.Rule != c.want) {
				t.Errorf("%q = %s [%s], want %q", cmd, got.Decision, got.Rule, c.want)
			}
		}
	}

	// The same destination from git configuration files, include among them.
	for _, tc := range []struct {
		name, config string
		extra        map[string]string
		want         string
	}{
		{"remote.origin.push", "[remote \"origin\"]\n\tpush = HEAD:main\n", nil, "git-push-protected"},
		{"upstream", "[push]\n\tdefault = upstream ; comment\n[branch \"dev\"]\n\tmerge = refs/heads/main\n", nil, "git-push-protected"},
		{"include", "[include]\n\tpath = extra.cfg\n", map[string]string{".git/extra.cfg": "[push]\ndefault = matching\n"}, "git-push-unclear"},
		{"mirror", "[remote \"origin\"]\n\tmirror\n", nil, "git-push-force"},
		{"simple", "[push]\n\tdefault = simple\n[branch \"dev\"]\n\tmerge = refs/heads/main\n", nil, ""},
	} {
		p := pushRepo(t, tc.config, tc.extra)
		got := p.CheckCommand("git push")
		if tc.want == "" && got.Decision != Allow || tc.want != "" && got.Rule != tc.want {
			t.Errorf("%s: git push = %s [%s], want %q", tc.name, got.Decision, got.Rule, tc.want)
		}
	}
}
