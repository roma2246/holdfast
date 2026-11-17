package guard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Git ref namespaces. Refs under refs/holdfast/ do not show up as branches
// and do not appear in `git stash list`, so they stay out of the user's way.
//
// Refs are shared by all working trees (git worktree) of a repository, so the
// snapshots of each tree live in their own subdirectory: refs/holdfast/main/…
// for the main one and refs/holdfast/wt-<name>/… for the others. Otherwise a
// rollback in one tree could pick up a snapshot of another.
const (
	SnapshotRefPrefix = "refs/holdfast/"
	BackupRefPrefix   = "refs/holdfast-backup/"
)

// KeepBackups is how many "before rollback" backups to keep.
const KeepBackups = 20

// ErrNoSnapshot means there are no snapshots yet.
var ErrNoSnapshot = errors.New("no snapshot found: run holdfast guard start first")

// Snapshot is a saved state of the working tree.
type Snapshot struct {
	Ref    string // refs/holdfast/main/20261113-204901-123456789
	Commit string
	Tree   string // tree hash: identical for snapshots of identical states
	Time   time.Time
	Meta   SnapshotMeta
}

// Snapshot kinds.
const (
	KindManual     = "manual"     // holdfast guard start
	KindCheckpoint = "checkpoint" // automatic, before a prompt to the agent
	KindPreDelete  = "pre-delete" // automatic, before a mass deletion
	KindBackup     = "backup"     // the state before a rollback
)

// SnapshotMeta is stored in the message of the snapshot's service commit.
type SnapshotMeta struct {
	Kind     string
	Session  string // Claude Code session_id
	Prompt   string // the beginning of the user's prompt or a description of the action
	Worktree string // root of the working tree the snapshot was taken in
}

func (m SnapshotMeta) message(now time.Time) string {
	kind := m.Kind
	if kind == "" {
		kind = KindManual
	}
	var b strings.Builder
	fmt.Fprintf(&b, "holdfast %s %s\n\nkind: %s\n", kind, now.Format(time.RFC3339), kind)
	if m.Session != "" {
		fmt.Fprintf(&b, "session: %s\n", m.Session)
	}
	if m.Worktree != "" {
		fmt.Fprintf(&b, "worktree: %s\n", m.Worktree)
	}
	if m.Prompt != "" {
		fmt.Fprintf(&b, "prompt: %s\n", oneLine(m.Prompt, 120))
	}
	return b.String()
}

func parseMeta(body string) SnapshotMeta {
	var m SnapshotMeta
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch k {
		case "kind":
			m.Kind = v
		case "session":
			m.Session = v
		case "prompt":
			m.Prompt = v
		case "worktree":
			m.Worktree = v
		}
	}
	if m.Kind == "" {
		m.Kind = KindManual // snapshots from older versions
	}
	return m
}

// oneLine squeezes text into a single line of at most n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Name is the snapshot's short name: the last component of the ref.
func (s Snapshot) Name() string {
	return s.Ref[strings.LastIndex(s.Ref, "/")+1:]
}

// Git is a thin wrapper around the git command (through os/exec).
type Git struct {
	Dir string // root of the working tree
}

// OpenGit finds the root of the git repository that contains dir.
func OpenGit(dir string) (*Git, error) {
	out, err := (&Git{Dir: dir}).run(nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository: snapshots work through git", dir)
	}
	return &Git{Dir: strings.TrimSpace(out)}, nil
}

var unsafeRefRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// worktreeID is "main" for the main working tree and "wt-<name>" for one
// added with `git worktree add` (the name is whatever git itself calls it).
func (g *Git) worktreeID() string {
	gd := gitDir(g.Dir)
	if gd == "" || filepath.Clean(commonDir(gd)) == filepath.Clean(gd) {
		return "main"
	}
	return "wt-" + unsafeRefRe.ReplaceAllString(filepath.Base(gd), "-")
}

func (g *Git) snapPrefix() string   { return SnapshotRefPrefix + g.worktreeID() + "/" }
func (g *Git) backupPrefix() string { return BackupRefPrefix + g.worktreeID() + "/" }

// run executes git with extra environment variables and stdin.
func (g *Git) run(env []string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Dir
	cmd.Stdin = stdin
	// Snapshots are service commits, so the author is set explicitly: that way
	// they work even without user.name/user.email configured.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=holdfast", "GIT_AUTHOR_EMAIL=holdfast@localhost",
		"GIT_COMMITTER_NAME=holdfast", "GIT_COMMITTER_EMAIL=holdfast@localhost",
		"LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// withTempIndex calls fn with GIT_INDEX_FILE pointing at a temporary index.
// The user's real index (staged changes) is left untouched.
func (g *Git) withTempIndex(fn func(env []string) error) error {
	f, err := os.CreateTemp("", "holdfast-index-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	os.Remove(name) // git needs either a missing file or a valid index
	defer os.Remove(name)
	return fn([]string{"GIT_INDEX_FILE=" + name})
}

func (g *Git) head() (string, bool) {
	out, err := g.run(nil, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// WriteTree saves the current working tree (including untracked files,
// except those ignored by .gitignore) as a git tree and returns its hash.
//
// Why not `git stash create`: it leaves out untracked files.
// A temporary index + `git add -A` + `git write-tree` does what stash does,
// but with new files included and without touching the stash, branches or the index.
func (g *Git) WriteTree() (string, error) {
	// Fast path: a persistent service index. If it is unavailable (no
	// commits, a parallel run holds the lock), take the honest slow path.
	if tree, err := g.writeTreeCached(); err == nil {
		return tree, nil
	}
	tree, err := g.writeTreeFresh()
	if err != nil && strings.Contains(err.Error(), "does not have a commit checked out") {
		return "", fmt.Errorf("the project contains a nested git repository without commits, so git cannot save a snapshot; "+
			"make a commit in it, delete it or add it to .gitignore (%w)", err)
	}
	return tree, err
}

// writeTreeFresh builds the tree from scratch in a temporary index: git
// re-reads and re-hashes every file of the project.
func (g *Git) writeTreeFresh() (string, error) {
	var tree string
	err := g.withTempIndex(func(env []string) error {
		if _, ok := g.head(); ok {
			if _, err := g.run(env, nil, "read-tree", "HEAD"); err != nil {
				return err
			}
		}
		if _, err := g.run(env, nil, "add", "-A", "--", "."); err != nil {
			return err
		}
		out, err := g.run(env, nil, "write-tree")
		tree = strings.TrimSpace(out)
		return err
	})
	return tree, err
}

// cachedIndexStep is a test seam: when set, it is called after each git step
// of writeTreeCached, so a test can interleave another process at that point.
var cachedIndexStep func(step string)

// writeTreeCached gives the same result but keeps the service index in
// .git/holdfast/index between runs. In the index git remembers the size and
// modification time of every file, so it re-reads only the changed ones, which
// is several times faster on large projects. `read-tree --reset HEAD` resets
// the index to the state of HEAD while keeping those stamps for untouched
// files. Not `-m`: it refuses with "not uptodate" once a file that is already
// in the index (from the previous snapshot) is edited again, which silently
// sent every such snapshot down the slow path.
//
// The shared index is only a seed. Every run works on its own private copy
// and then atomically renames the copy back. Git locks the index per command,
// not for the whole read-tree → add → write-tree sequence, so two runs on one
// shared file could interleave: one run's read-tree reset the index between
// the other run's add and write-tree, and that snapshot silently got the HEAD
// version of edited files. Private copies need no lock, so there is no lock to
// wait for or to leave behind after a crash; the last run to finish simply
// leaves its (equally valid) index as the next seed.
func (g *Git) writeTreeCached() (string, error) {
	gd := gitDir(g.Dir)
	if _, ok := g.head(); gd == "" || !ok {
		return "", errors.New("no .git or no commits")
	}
	dir := filepath.Join(gd, "holdfast")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	shared := filepath.Join(dir, "index")
	// The private copy sits next to the shared index, so the final rename
	// stays on one file system and is atomic.
	f, err := os.CreateTemp(dir, "index-*.tmp")
	if err != nil {
		return "", err
	}
	private := f.Name()
	defer os.Remove(private) // a no-op after a successful rename
	if err := seedIndex(f, shared); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + private}
	for _, args := range [][]string{{"read-tree", "--reset", "HEAD"}, {"add", "-A", "--", "."}} {
		if _, err := g.run(env, nil, args...); err != nil {
			return "", err
		}
		if cachedIndexStep != nil {
			cachedIndexStep(args[0])
		}
	}
	out, err := g.run(env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	// Publishing the cache is best effort: the tree is already correct, and a
	// failed rename (e.g. the file is busy on Windows) only costs speed next time.
	_ = os.Rename(private, shared)
	return strings.TrimSpace(out), nil
}

// seedIndex fills the freshly created private index f with a copy of the
// shared one. Without a shared index yet, f is removed, because git accepts
// either a valid index or a missing file. The copy keeps the original's
// modification time, which git compares with the file stamps to spot racily
// clean entries.
func seedIndex(f *os.File, shared string) error {
	defer f.Close()
	src, err := os.Open(shared)
	if errors.Is(err, os.ErrNotExist) {
		f.Close()
		return os.Remove(f.Name())
	}
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := io.Copy(f, src); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if fi, err := src.Stat(); err == nil {
		_ = os.Chtimes(f.Name(), fi.ModTime(), fi.ModTime())
	}
	return nil
}

// CreateSnapshot saves the current working tree as a snapshot.
func (g *Git) CreateSnapshot(now time.Time, meta SnapshotMeta) (Snapshot, error) {
	tree, err := g.WriteTree()
	if err != nil {
		return Snapshot{}, err
	}
	return g.snapshotFromTree(g.snapPrefix(), now, tree, meta)
}

func (g *Git) snapshotFromTree(prefix string, now time.Time, tree string, meta SnapshotMeta) (Snapshot, error) {
	if meta.Kind == "" {
		meta.Kind = KindManual
	}
	meta.Worktree = g.Dir
	args := []string{"commit-tree", tree, "-m", meta.message(now)}
	if head, ok := g.head(); ok {
		args = append(args, "-p", head)
	}
	out, err := g.run(nil, nil, args...)
	if err != nil {
		return Snapshot{}, err
	}
	commit := strings.TrimSpace(out)

	// The name is the time with fixed-width nanoseconds: name order matches
	// creation order. An empty "old value" for update-ref means "the ref does
	// not exist yet": creation is atomic, and on a name clash we take the next one.
	t := now.UTC()
	for attempt := 0; ; attempt++ {
		ref := prefix + t.Format("20060102-150405") + fmt.Sprintf("-%09d", t.Nanosecond())
		_, err := g.run(nil, nil, "update-ref", ref, commit, "")
		if err == nil {
			return Snapshot{Ref: ref, Commit: commit, Tree: tree, Time: now, Meta: meta}, nil
		}
		if attempt >= 20 || !g.refExists(ref) {
			return Snapshot{}, err
		}
		t = t.Add(time.Nanosecond)
	}
}

// Checkpoint is an automatic restore point. If the working tree has not
// changed since the last snapshot, no new one is created: the last one is
// returned (created=false). Old snapshots beyond keep are deleted.
func (g *Git) Checkpoint(now time.Time, meta SnapshotMeta, keep int) (snap Snapshot, created bool, err error) {
	tree, err := g.WriteTree()
	if err != nil {
		return Snapshot{}, false, err
	}
	snaps, err := g.Snapshots()
	if err != nil {
		return Snapshot{}, false, err
	}
	if len(snaps) > 0 && snaps[0].Tree == tree {
		return snaps[0], false, nil
	}
	snap, err = g.snapshotFromTree(g.snapPrefix(), now, tree, meta)
	if err != nil {
		return Snapshot{}, false, err
	}
	return snap, true, g.Prune(keep)
}

// Prune keeps the keep most recent snapshots of this working tree
// (keep <= 0 means delete nothing).
func (g *Git) Prune(keep int) error {
	snaps, err := g.Snapshots()
	if err != nil {
		return err
	}
	return g.deleteOld(snaps, keep)
}

func (g *Git) deleteOld(snaps []Snapshot, keep int) error {
	if keep <= 0 || len(snaps) <= keep {
		return nil
	}
	// update-ref --stdin deletes all the extra refs with a single git command.
	var b strings.Builder
	for _, s := range snaps[keep:] {
		fmt.Fprintf(&b, "delete %s\n", s.Ref)
	}
	_, err := g.run(nil, strings.NewReader(b.String()), "update-ref", "--stdin")
	return err
}

func (g *Git) refExists(ref string) bool {
	_, err := g.run(nil, nil, "show-ref", "--verify", "-q", ref)
	return err == nil
}

// Snapshots returns the restore points of this working tree, newest first.
func (g *Git) Snapshots() ([]Snapshot, error) { return g.list(SnapshotRefPrefix) }

// Backups returns the "before rollback" backups, newest first.
func (g *Git) Backups() ([]Snapshot, error) { return g.list(BackupRefPrefix) }

func (g *Git) list(root string) ([]Snapshot, error) {
	// %00 and %1e in the for-each-ref format are the NUL and RS bytes: field
	// and record separators that occur neither in ref names nor in prompt text.
	out, err := g.run(nil, nil, "for-each-ref",
		"--format=%(refname)%00%(objectname)%00%(committerdate:unix)%00%(tree)%00%(contents:body)%1e", root)
	if err != nil {
		return nil, err
	}
	wt := g.worktreeID()
	var snaps []Snapshot
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.Split(strings.TrimLeft(rec, "\n"), "\x00")
		if len(f) != 5 {
			continue
		}
		// Our own snapshots live in <root><wt>/<name>. Snapshots from older
		// versions lived right in <root><name>: those are shown only in the main tree.
		rest := strings.TrimPrefix(f[0], root)
		own := strings.HasPrefix(rest, wt+"/") && strings.Count(rest, "/") == 1
		legacy := wt == "main" && !strings.Contains(rest, "/")
		if !own && !legacy {
			continue
		}
		unix, _ := strconv.ParseInt(f[2], 10, 64) // an error gives time 0, which is not critical
		snaps = append(snaps, Snapshot{Ref: f[0], Commit: f[1], Tree: f[3], Time: time.Unix(unix, 0), Meta: parseMeta(f[4])})
	}
	sort.SliceStable(snaps, func(i, j int) bool { return nameKey(snaps[i].Name()) > nameKey(snaps[j].Name()) })
	return snaps, nil
}

var legacyNameRe = regexp.MustCompile(`^(\d{8}-\d{6})-000(?:-(\d+))?$`)

// nameKey turns a snapshot name into a string whose order matches creation
// order. For names in the old format (…-000, …-000-2, …-000-12) the number is
// zero-padded, otherwise "-12" would sort before "-2".
func nameKey(name string) string {
	if m := legacyNameRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return fmt.Sprintf("%s-000000000-%06d", m[1], n)
	}
	return name + "-000000"
}

// SnapshotAt returns the n-th snapshot from the end (1 is the most recent).
func (g *Git) SnapshotAt(n int) (Snapshot, error) {
	snaps, err := g.Snapshots()
	if err != nil {
		return Snapshot{}, err
	}
	if len(snaps) == 0 {
		return Snapshot{}, ErrNoSnapshot
	}
	if n < 1 || n > len(snaps) {
		return Snapshot{}, fmt.Errorf("there is no restore point %d: only %d exist (see holdfast guard list)", n, len(snaps))
	}
	return snaps[n-1], nil
}

// FindSnapshot looks a snapshot up by name or full ref; an empty name means
// the most recent one. "Before rollback" backups (refs/holdfast-backup/…) are
// found by name too, which is how a rollback can be undone.
func (g *Git) FindSnapshot(name string) (Snapshot, error) {
	snaps, err := g.Snapshots()
	if err != nil {
		return Snapshot{}, err
	}
	if name == "" {
		if len(snaps) == 0 {
			return Snapshot{}, ErrNoSnapshot
		}
		return snaps[0], nil
	}
	backups, err := g.Backups()
	if err != nil {
		return Snapshot{}, err
	}
	for _, s := range append(snaps, backups...) {
		if s.Name() == name || s.Ref == name {
			return s, nil
		}
	}
	return Snapshot{}, fmt.Errorf("snapshot %q not found in this working tree", name)
}

// TreeFiles returns the set of files of the tree under paths
// (relative to the repository root; without paths, all files).
func (g *Git) TreeFiles(tree string, paths []string) (map[string]bool, error) {
	args := append([]string{"ls-tree", "-r", "-z", "--name-only", tree, "--"}, paths...)
	// GIT_LITERAL_PATHSPECS: paths are taken literally, without glob characters.
	out, err := g.run([]string{"GIT_LITERAL_PATHSPECS=1"}, nil, args...)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, f := range splitZ(out) {
		set[f] = true
	}
	return set, nil
}

// Untracked returns untracked, non-ignored files (what git clean -f would delete).
func (g *Git) Untracked() ([]string, error) {
	out, err := g.run(nil, nil, "ls-files", "--others", "--exclude-standard", "-z")
	return splitZ(out), err
}

// DiffOptions is the diff output parameters.
type DiffOptions struct {
	Stat       bool
	NameStatus bool
	Color      bool
}

// Diff prints the working-tree changes since the snapshot: exactly what a
// rollback to it would undo.
func (g *Git) Diff(w io.Writer, snap Snapshot, opt DiffOptions) error {
	plan, err := g.PlanRollback(snap)
	if err != nil {
		return err
	}
	args := []string{"diff", "--no-ext-diff"}
	if opt.Color {
		args = append(args, "--color=always")
	} else {
		args = append(args, "--color=never")
	}
	if opt.Stat {
		args = append(args, "--stat")
	}
	if opt.NameStatus {
		args = append(args, "--name-status")
	}
	args = append(args, snap.Commit+"^{tree}", plan.Tree)
	out, err := g.run(nil, nil, args...)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, out)
	return err
}

// Changes is a summary of the differences between a snapshot and the current tree.
type Changes struct {
	Added    []string // appeared after the snapshot (will be deleted)
	Modified []string // changed or deleted (will be restored)
}

// Empty reports that there are no changes.
func (c Changes) Empty() bool { return len(c.Added)+len(c.Modified) == 0 }

// ChangesSince works out what has changed since the snapshot (what a rollback would undo).
func (g *Git) ChangesSince(snap Snapshot) (Changes, error) {
	plan, err := g.PlanRollback(snap)
	return plan.Changes, err
}

// ChangesTo is what changed between the snapshot and the tree cur (from WriteTree).
func (g *Git) ChangesTo(snap Snapshot, cur string) (Changes, error) {
	return g.changesBetween(snap.Commit+"^{tree}", cur)
}

func (g *Git) changesBetween(from, to string) (Changes, error) {
	var c Changes
	// -z: names are separated by a NUL byte, which is reliable for paths with spaces.
	// --no-renames: a rename = a deletion + an addition.
	added, err := g.run(nil, nil, "diff", "--no-renames", "--name-only", "-z", "--diff-filter=A", from, to)
	if err != nil {
		return c, err
	}
	other, err := g.run(nil, nil, "diff", "--no-renames", "--name-only", "-z", "--diff-filter=MDT", from, to)
	if err != nil {
		return c, err
	}
	c.Added = splitZ(added)
	c.Modified = splitZ(other)
	return c, nil
}

func splitZ(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// RollbackPlan is what exactly a rollback will do. It is built before any changes are made.
type RollbackPlan struct {
	Snapshot Snapshot
	// Tree is the current state, including everything the rollback will
	// overwrite or delete. This is what gets saved as the backup.
	Tree    string
	Changes Changes
	// Hidden is the paths a regular snapshot does not see (git ignores them
	// right now) but the rollback will touch. They are force-added to Tree.
	Hidden []string
	// Skipped is the nested git repositories the rollback leaves as they are.
	Skipped []string
}

// PlanRollback computes the rollback plan and guarantees the main property:
// everything the rollback will overwrite or delete is saved in plan.Tree. If
// it can't be saved, an error is returned and the rollback does not start.
//
// A regular snapshot (WriteTree) honours .gitignore. But a file may have been
// added to .gitignore after the restore point: then it is in the restore
// point, the rollback will overwrite it, and a regular snapshot would not
// hold its current version. Such paths are force-added to the backup (git add -f).
func (g *Git) PlanRollback(snap Snapshot) (RollbackPlan, error) {
	plan := RollbackPlan{Snapshot: snap}
	if wt := snap.Meta.Worktree; wt != "" && !samePath(wt, g.Dir) {
		return plan, fmt.Errorf("snapshot %s was taken in another working tree (%s); rollback cancelled", snap.Name(), wt)
	}
	snapTree := snap.Commit + "^{tree}"
	base, err := g.WriteTree()
	if err != nil {
		return plan, err
	}
	// What is in the restore point but missing from a regular snapshot of the current state.
	out, err := g.run(nil, nil, "diff", "--no-renames", "--name-only", "-z", "--diff-filter=D", snapTree, base)
	if err != nil {
		return plan, err
	}
	hidden := map[string]bool{}
	for _, rel := range splitZ(out) {
		if _, err := os.Lstat(filepath.Join(g.Dir, filepath.FromSlash(rel))); err == nil {
			hidden[rel] = true // something sits at this path: a file, a symlink or a directory
			continue
		}
		// A file may be sitting where one of the parent directories should be.
		for parent := filepath.ToSlash(filepath.Dir(rel)); parent != "." && parent != "/"; parent = filepath.ToSlash(filepath.Dir(parent)) {
			fi, err := os.Lstat(filepath.Join(g.Dir, filepath.FromSlash(parent)))
			if err != nil {
				continue
			}
			if !fi.IsDir() {
				hidden[parent] = true
			}
			break
		}
	}
	plan.Tree = base
	if len(hidden) > 0 {
		for rel := range hidden {
			plan.Hidden = append(plan.Hidden, rel)
		}
		sort.Strings(plan.Hidden)
		if plan.Tree, err = g.treeWithForced(base, plan.Hidden); err != nil {
			return plan, fmt.Errorf("could not save the files the rollback would overwrite (%s): %w; rollback cancelled",
				strings.Join(plan.Hidden, ", "), err)
		}
	}
	if plan.Changes, err = g.changesBetween(snapTree, plan.Tree); err != nil {
		return plan, err
	}
	return plan, g.guardNestedRepos(snapTree, &plan)
}

// guardNestedRepos keeps a rollback from destroying nested git repositories.
// In a tree such a repository is a single link entry without contents, so the
// backup does not hold its files. A nested repository that appeared after the
// restore point is left in place; but if the rollback needs to write a file
// in its place or inside it, the rollback is cancelled.
func (g *Git) guardNestedRepos(snapTree string, plan *RollbackPlan) error {
	nested, err := g.gitlinks(plan.Tree)
	if err != nil || len(nested) == 0 {
		return err
	}
	inSnap, err := g.gitlinks(snapTree)
	if err != nil {
		return err
	}
	for _, m := range plan.Changes.Modified {
		for repo := range nested {
			if (m == repo && !inSnap[repo]) || strings.HasPrefix(m, repo+"/") {
				return fmt.Errorf("%s is currently occupied by a nested git repository (%s): its contents can't be saved in the backup, so the rollback is cancelled; "+
					"move or delete that repository and try again", m, repo)
			}
		}
	}
	keep := func(paths []string) []string {
		var out []string
		for _, p := range paths {
			if nested[p] {
				plan.Skipped = append(plan.Skipped, p)
				continue
			}
			out = append(out, p)
		}
		return out
	}
	plan.Changes.Added = keep(plan.Changes.Added)
	plan.Changes.Modified = keep(plan.Changes.Modified)
	return nil
}

// gitlinks returns the paths of nested repositories (mode 160000 entries) in a tree.
func (g *Git) gitlinks(tree string) (map[string]bool, error) {
	out, err := g.run(nil, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, err
	}
	links := map[string]bool{}
	for _, rec := range splitZ(out) {
		// "<mode> <type> <hash>\t<path>"
		meta, path, ok := strings.Cut(rec, "\t")
		if ok && strings.HasPrefix(meta, "160000 ") {
			links[path] = true
		}
	}
	return links, nil
}

// treeWithForced builds the tree base + force-added paths (bypassing
// .gitignore) and verifies that every file under them was saved.
func (g *Git) treeWithForced(base string, paths []string) (string, error) {
	var tree string
	err := g.withTempIndex(func(env []string) error {
		if _, err := g.run(env, nil, "read-tree", base); err != nil {
			return err
		}
		env = append(env, "GIT_LITERAL_PATHSPECS=1")
		stdin := strings.NewReader(strings.Join(paths, "\x00") + "\x00")
		if _, err := g.run(env, stdin, "add", "-f", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
		out, err := g.run(env, nil, "write-tree")
		tree = strings.TrimSpace(out)
		return err
	})
	if err != nil {
		return "", err
	}
	saved, err := g.TreeFiles(tree, paths)
	if err != nil {
		return "", err
	}
	// The check: everything on disk under these paths is in the tree.
	for _, rel := range paths {
		root := filepath.Join(g.Dir, filepath.FromSlash(rel))
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fmt.Errorf("%s contains a nested git repository", rel)
				}
				return nil
			}
			r, err := filepath.Rel(g.Dir, path)
			if err != nil {
				return err
			}
			if !saved[filepath.ToSlash(r)] {
				return fmt.Errorf("could not save %s", filepath.ToSlash(r))
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return tree, nil
}

// samePath compares paths with symlinks taken into account (/tmp → /private/tmp).
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// Rollback returns the working tree to the snapshot: it deletes the files that
// appeared after it and restores the changed and deleted ones. Before that the
// current state (including everything that will be overwritten) is saved under
// refs/holdfast-backup/, so the rollback can be undone by rolling back to that backup.
func (g *Git) Rollback(snap Snapshot, now time.Time) (Changes, Snapshot, error) {
	plan, err := g.PlanRollback(snap)
	if err != nil {
		return Changes{}, Snapshot{}, err
	}
	backup, err := g.snapshotFromTree(g.backupPrefix(), now, plan.Tree,
		SnapshotMeta{Kind: KindBackup, Prompt: "before rollback to " + snap.Name()})
	if err != nil {
		return Changes{}, Snapshot{}, fmt.Errorf("backup before rollback: %w", err)
	}
	ch := plan.Changes

	for _, rel := range ch.Added {
		p := filepath.Join(g.Dir, filepath.FromSlash(rel))
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ch, backup, err
		}
		removeEmptyParents(g.Dir, filepath.Dir(p))
	}

	// checkout-index through a temporary index writes the snapshot's files into
	// the working tree without changing HEAD or the real index. Only the
	// changed paths are written: the other files are not touched at all.
	if len(ch.Modified) > 0 {
		err = g.withTempIndex(func(env []string) error {
			if _, err := g.run(env, nil, "read-tree", snap.Commit+"^{tree}"); err != nil {
				return err
			}
			stdin := strings.NewReader(strings.Join(ch.Modified, "\x00") + "\x00")
			_, err := g.run(env, stdin, "checkout-index", "-f", "-z", "--stdin")
			return err
		})
		if err != nil {
			return ch, backup, err
		}
	}
	if backups, err := g.Backups(); err == nil {
		_ = g.deleteOld(backups, KeepBackups)
	}
	return ch, backup, nil
}

// removeEmptyParents removes directories that became empty, up to the root.
func removeEmptyParents(root, dir string) {
	for dir != root && isWithin(root, dir) {
		if err := os.Remove(dir); err != nil { // Remove does not delete a non-empty directory
			return
		}
		dir = filepath.Dir(dir)
	}
}

// AllRefs is every Holdfast ref in all working trees of the repository:
// restore points and before-rollback backups.
func (g *Git) AllRefs() ([]string, error) {
	out, err := g.run(nil, nil, "for-each-ref", "--format=%(refname)", SnapshotRefPrefix, BackupRefPrefix)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// PurgeRefs deletes all Holdfast refs (holdfast guard uninstall --purge).
// The git objects themselves are removed later by regular garbage collection (git gc).
func (g *Git) PurgeRefs() (int, error) {
	refs, err := g.AllRefs()
	if err != nil || len(refs) == 0 {
		return 0, err
	}
	var b strings.Builder
	for _, r := range refs {
		fmt.Fprintf(&b, "delete %s\n", r)
	}
	if _, err := g.run(nil, strings.NewReader(b.String()), "update-ref", "--stdin"); err != nil {
		return 0, err
	}
	return len(refs), nil
}
