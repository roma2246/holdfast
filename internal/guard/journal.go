package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RollbackJournal marks a rollback that has started changing files and has not
// been verified yet. It lives in <git dir>/holdfast/rollback.json (per working
// tree) and is removed only after the result has been checked, so a rollback
// interrupted at any point (an error, a crash, a killed process) is still
// visible to the next Holdfast process, which can finish or undo it.
type RollbackJournal struct {
	Target  JournalSnap `json:"target"` // the snapshot the files are being returned to
	Backup  JournalSnap `json:"backup"` // the state right before the rollback
	Started time.Time   `json:"started"`
	PID     int         `json:"pid"`
}

// JournalSnap is just enough of a snapshot to roll back to it again, even if
// its ref has been pruned in the meantime (the commit outlives the ref).
type JournalSnap struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

func (s JournalSnap) snapshot(worktree string) Snapshot {
	return Snapshot{Ref: s.Ref, Commit: s.Commit, Meta: SnapshotMeta{Worktree: worktree}}
}

// Name is the snapshot's short name.
func (s JournalSnap) Name() string { return s.Ref[strings.LastIndex(s.Ref, "/")+1:] }

// ErrRollbackPending means a previous rollback was interrupted.
var ErrRollbackPending = errors.New("a previous rollback was interrupted")

func (g *Git) journalPath() string {
	gd := gitDir(g.Dir)
	if gd == "" {
		return ""
	}
	return filepath.Join(gd, "holdfast", "rollback.json")
}

// PendingRollback returns the journal of an interrupted rollback, or nil.
// An unreadable journal is still reported: the rollback did not finish, and
// that must not look like a clean state.
func (g *Git) PendingRollback() (*RollbackJournal, error) {
	p := g.journalPath()
	if p == "" {
		return nil, nil
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w, and its journal %s can't be read: %w", ErrRollbackPending, p, err)
	}
	var j RollbackJournal
	if err := json.Unmarshal(data, &j); err != nil || j.Target.Commit == "" || j.Backup.Commit == "" {
		return nil, fmt.Errorf("%w, and its journal %s is damaged; the state before it is among the backups: holdfast guard list", ErrRollbackPending, p)
	}
	return &j, nil
}

// PendingHelp is what to tell the user about an interrupted rollback.
func (j *RollbackJournal) PendingHelp() string {
	return fmt.Sprintf("a rollback to %s (started %s) was interrupted, so the files may be half restored.\n"+
		"  Finish it:           holdfast guard rollback --resume\n"+
		"  Undo it (back to the state before it): holdfast guard rollback --abort  (backup %s)",
		j.Target.Name(), j.Started.Local().Format("2006-01-02 15:04:05"), j.Backup.Name())
}

// writeJournal saves the journal durably before any file is changed. With
// replace=false it fails if a journal already exists: that keeps two
// rollbacks from running at once and an interrupted one from being forgotten.
// The content is written and synced to a temporary file first and then
// linked (or renamed) into place, so the journal is never half written.
func (g *Git) writeJournal(j RollbackJournal, replace bool) error {
	p := g.journalPath()
	if p == "" {
		return errors.New("no .git directory for the rollback journal")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), tempPattern("rollback", time.Now()))
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if replace {
		return os.Rename(tmp, p)
	}
	// os.Link fails when the target exists: an atomic "create if absent".
	if err := os.Link(tmp, p); err != nil {
		if _, statErr := os.Stat(p); statErr == nil {
			return ErrRollbackPending
		}
		return err
	}
	return nil
}

func (g *Git) clearJournal() error {
	err := os.Remove(g.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// verifyRollback checks the result on disk against the snapshot: every
// restored path holds exactly the snapshot's content (git computes the hashes)
// and every path to delete is gone. A rollback is reported as done only after this.
func (g *Git) verifyRollback(snapTree string, ch Changes) error {
	var wrong []string
	// A deleted path may legitimately exist again as a directory when the
	// snapshot has files under it (a file was put where a directory was).
	restoredDirs := map[string]bool{}
	for _, rel := range ch.Modified {
		for d := filepath.ToSlash(filepath.Dir(rel)); d != "." && d != "/"; d = filepath.ToSlash(filepath.Dir(d)) {
			restoredDirs[d] = true
		}
	}
	for _, rel := range ch.Added {
		fi, err := os.Lstat(filepath.Join(g.Dir, filepath.FromSlash(rel)))
		if err == nil && (!fi.IsDir() || !restoredDirs[rel]) {
			wrong = append(wrong, rel+" (still exists)")
		}
	}
	if len(ch.Modified) > 0 {
		// Hash the restored paths on top of the snapshot's own tree: if they all
		// match, the resulting tree is the snapshot's tree.
		var tree string
		err := g.withTempIndex(func(env []string) error {
			if _, err := g.run(env, nil, "read-tree", snapTree); err != nil {
				return err
			}
			env = append(env, "GIT_LITERAL_PATHSPECS=1")
			stdin := strings.NewReader(strings.Join(ch.Modified, "\x00") + "\x00")
			if _, err := g.run(env, stdin, "add", "-f", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
				return err
			}
			out, err := g.run(env, nil, "write-tree")
			tree = strings.TrimSpace(out)
			return err
		})
		if err != nil {
			return fmt.Errorf("could not check the restored files: %w", err)
		}
		out, err := g.run(nil, nil, "diff", "--no-renames", "--name-only", "-z", snapTree, tree)
		if err != nil {
			return fmt.Errorf("could not check the restored files: %w", err)
		}
		for _, rel := range splitZ(out) {
			wrong = append(wrong, rel+" (differs from the snapshot)")
		}
	}
	if len(wrong) > 0 {
		return fmt.Errorf("after the rollback %d path(s) do not match the snapshot: %s", len(wrong), strings.Join(wrong, ", "))
	}
	return nil
}
