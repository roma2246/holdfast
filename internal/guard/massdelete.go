package guard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mass deletion inside the project is allowed only when it is really
// reversible. "Some snapshot exists" does not prove that, so the hook:
//
//  1. lists the files the command would delete;
//  2. takes a restore point right now;
//  3. verifies that every file to be deleted made it into the restore point.
//
// Files that git ignores are not in a restore point, so a rollback won't bring
// them back. The exception is directories and files that are known to be
// regenerated (node_modules, dist, *.pyc …) from disposable_paths: those may
// be deleted without a restore point.

// maxScanFiles is the walk limit when sizing up a deletion.
const maxScanFiles = 20000

// deleteScan is what the command would delete.
type deleteScan struct {
	files      []string // absolute file paths, disposable ones excluded
	inGit      bool     // touches the .git directory
	tooMany    bool     // more files than maxScanFiles
	disposable bool     // disposable directories or files were encountered
}

// isDisposable reports whether a path (relative to the project root) is disposable:
// a pattern without '/' is compared with every path component, one with '/' with the whole path.
func (p *Policy) isDisposable(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, pat := range p.cfg.DisposablePaths {
		if strings.Contains(pat, "/") {
			if p.match(pat, rel) {
				return true
			}
			continue
		}
		re := p.globRegexp(pat)
		for _, part := range parts {
			if re.MatchString(part) {
				return true
			}
		}
	}
	return false
}

// scanDelete lists the files the command would delete.
// targets are absolute paths (globs allowed); recursive is like rm -r;
// match is a filter by name (find -name), nil means no filter.
func (p *Policy) scanDelete(targets []string, recursive bool, match func(name string) bool) deleteScan {
	var s deleteScan
	add := func(path string) {
		switch {
		case hasGitComponent(p.rel(path)):
			s.inGit = true
		case p.isDisposable(p.rel(path)):
			s.disposable = true
		case len(s.files) >= maxScanFiles:
			s.tooMany = true
		default:
			s.files = append(s.files, path)
		}
	}
	// walk traverses a directory; all=true means the name filter no longer applies
	// (the directory itself matched the filter and is deleted as a whole).
	var walk func(root string, all bool)
	walk = func(root string, all bool) {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || s.tooMany {
				return filepath.SkipAll
			}
			matched := all || match == nil || match(d.Name())
			if !d.IsDir() {
				if matched {
					add(path)
				}
				return nil
			}
			if path == root {
				return nil
			}
			switch {
			case d.Name() == ".git":
				// Without a filter the deletion takes .git as a whole. With a
				// filter, only if something inside matches it.
				if all || match == nil || match(d.Name()) || gitInternalsMatch(path, match) {
					s.inGit = true
				}
				return filepath.SkipDir
			case p.isDisposable(p.rel(path)):
				if matched || match == nil {
					s.disposable = true
				}
				return filepath.SkipDir
			case matched && !all && match != nil:
				walk(path, true)
				return filepath.SkipDir
			}
			return nil
		})
	}

	for _, t := range targets {
		paths := []string{t}
		if strings.ContainsAny(t, "*?[") {
			paths = shellGlob(t)
		}
		for _, path := range paths {
			fi, err := os.Lstat(path)
			if err != nil {
				continue
			}
			if !fi.IsDir() || !recursive {
				if match == nil || match(filepath.Base(path)) {
					add(path)
				}
				continue
			}
			switch {
			case filepath.Base(path) == ".git" || hasGitComponent(p.rel(path)):
				s.inGit = true
			case p.isDisposable(p.rel(path)):
				s.disposable = true
			default:
				walk(path, false)
			}
		}
	}
	return s
}

func hasGitComponent(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}

// gitInternalsMatch reports whether at least one file inside .git matches the filter.
func gitInternalsMatch(gitDir string, match func(string) bool) bool {
	found, seen := false, 0
	_ = filepath.WalkDir(gitDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > maxScanFiles {
			found = true // couldn't check everything, so assume it is affected
		}
		if match(d.Name()) {
			found = true
		}
		if found {
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// deleteInProject decides whether targets inside the project may be deleted.
func (p *Policy) deleteInProject(what string, targets []string, recursive bool, match func(string) bool) Verdict {
	if len(targets) == 0 {
		return allowVerdict
	}
	s := p.scanDelete(targets, recursive, match)
	if s.inGit {
		return Verdict{Deny, "delete-git-dir", what + " would touch the .git directory, which holds the project history and the restore points. Name specific project files or directories"}
	}
	// rm -r dir also deletes the files inside: path rules apply to each one.
	rules := allowVerdict
	for _, f := range s.files {
		if p.isService(f) {
			return p.serviceVerdict(f)
		}
		if rules = worst(rules, p.pathRules(f)); rules.Decision == Deny {
			return rules
		}
	}
	if rules.Decision != Allow {
		return rules
	}
	if len(s.files) <= p.cfg.MaxDeletes && !s.tooMany {
		if s.disposable && !p.cfg.Strict() {
			p.bestEffortSnapshot(what) // in case of unsaved edits in build/, dist/ and the like
		}
		return allowVerdict
	}
	if p.cfg.Strict() {
		return Verdict{Ask, "mass-delete", fmt.Sprintf("%s: more than %d files; user confirmation required", what, p.cfg.MaxDeletes)}
	}
	if s.tooMany {
		return Verdict{Deny, "mass-delete", fmt.Sprintf("%s: more than %d files, too many to check them all. Delete in parts, one directory at a time", what, maxScanFiles)}
	}
	g, err := OpenGit(p.Root)
	if err != nil {
		return Verdict{Deny, "mass-delete", fmt.Sprintf(
			"%s: more than %d files, and the project is not under git, so no restore point can be taken. Delete at most %d files at a time or ask the user",
			what, p.cfg.MaxDeletes, p.cfg.MaxDeletes)}
	}
	rel := make([]string, 0, len(s.files))
	for _, f := range s.files {
		rel = append(rel, p.repoRel(g, f))
	}
	return p.snapshotCovers(g, what, rel)
}

// snapshotCovers takes a restore point and verifies that all files
// (paths relative to the repository root) made it into it.
func (p *Policy) snapshotCovers(g *Git, what string, files []string) Verdict {
	_ = EnsureStateDir(p.Root)
	snap, _, err := g.Checkpoint(p.Now(), SnapshotMeta{Kind: KindPreDelete, Session: p.Session, Prompt: what}, p.cfg.KeepSnapshots)
	if err != nil {
		return Verdict{Deny, "mass-delete", fmt.Sprintf(
			"%s: could not save a restore point (%v), so the deletion was not performed. Tell the user about it", what, err)}
	}
	saved, err := g.TreeFiles(snap.Tree, nil)
	if err != nil {
		return Verdict{Deny, "mass-delete", fmt.Sprintf("%s: could not verify the restore point (%v). Tell the user about it", what, err)}
	}
	var lost []string
	for _, f := range files {
		if !saved[f] {
			lost = append(lost, f)
		}
	}
	if len(lost) == 0 {
		return allowVerdict // everything being deleted is in the restore point: holdfast guard rollback brings it back
	}
	sort.Strings(lost)
	sample := lost
	if len(sample) > 3 {
		sample = sample[:3]
	}
	return Verdict{Deny, "mass-delete", fmt.Sprintf(
		"%s: %d files are not covered by the restore point (git ignores them), a rollback won't bring them back: %s%s. "+
			"Delete only what git tracks, or ask the user whether they need these files (directories that get regenerated can be listed in disposable_paths)",
		what, len(lost), strings.Join(sample, ", "), map[bool]string{true: " …", false: ""}[len(lost) > 3])}
}

// bestEffortSnapshot takes a restore point without affecting the decision.
func (p *Policy) bestEffortSnapshot(what string) {
	g, err := OpenGit(p.Root)
	if err != nil {
		return
	}
	_ = EnsureStateDir(p.Root)
	_, _, _ = g.Checkpoint(p.Now(), SnapshotMeta{Kind: KindPreDelete, Session: p.Session, Prompt: what}, p.cfg.KeepSnapshots)
}

// repoRel converts an absolute path inside the project into a path relative
// to the git repository root. The project may be a subdirectory of the repository,
// and git returns the root with symlinks resolved (/private/var/…).
func (p *Policy) repoRel(g *Git, abs string) string {
	root, gitRoot := p.Root, g.Dir
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if r, err := filepath.EvalSymlinks(gitRoot); err == nil {
		gitRoot = r
	}
	prefix, err := filepath.Rel(gitRoot, root)
	if err != nil || strings.HasPrefix(prefix, "..") {
		prefix = "."
	}
	return filepath.ToSlash(filepath.Join(prefix, p.rel(abs)))
}

// checkFind handles find with deletion (-delete, -exec rm).
func (p *Policy) checkFind(args []word) Verdict {
	if !hasArg(args, "-delete") && !execsRm(args) {
		return allowVerdict
	}
	i := 0
	for i < len(args) && (args[i].s == "-H" || args[i].s == "-L" || args[i].s == "-P") {
		i++
	}
	var starts []word
	for ; i < len(args); i++ {
		if s := args[i].s; strings.HasPrefix(s, "-") || s == "(" || s == ")" || s == "!" {
			break
		}
		starts = append(starts, args[i])
	}
	expr := args[i:]
	if len(starts) == 0 {
		starts = []word{{s: "."}}
	}

	// The name filter is understood only in its simple form: -name/-iname without negations.
	var globs []string
	simple := true
	for j, a := range expr {
		switch a.s {
		case "-name", "-iname":
			if j+1 < len(expr) && !expr[j+1].dynamic {
				globs = append(globs, strings.ToLower(expr[j+1].s))
			} else {
				simple = false
			}
		case "-not", "!", "-prune", "-path", "-wholename", "-regex":
			simple = false
		}
	}
	var match func(string) bool
	if simple && len(globs) > 0 {
		match = func(name string) bool {
			for _, g := range globs {
				if ok, _ := filepath.Match(g, strings.ToLower(name)); ok {
					return true
				}
			}
			return false
		}
	}

	var targets []string
	for _, s := range starts {
		if s.dynamic || (p.cwdUnknown && !filepath.IsAbs(p.expandHome(s.s))) {
			return p.soft("rm-dynamic", fmt.Sprintf("find with deletion on a computed path (%s): its value is unknown", s.s),
				"Write the path out explicitly, without variables and without cd into a variable")
		}
		abs := p.abs(s.s)
		switch {
		case p.inRepo(abs):
			targets = append(targets, abs)
		case abs == "/" || abs == p.Home || isWithin(abs, p.Home) || isWithin(abs, p.Root):
			return Verdict{Deny, "rm-outside-repo", fmt.Sprintf("find with deletion on %s is not allowed: this directory contains the whole project or the whole home directory. Name a specific project directory", abs)}
		case !p.cfg.Strict() && p.isTempPath(abs):
			// temporary files outside the project
		default:
			return p.soft("rm-outside-repo", fmt.Sprintf("find with deletion outside the project (%s)", abs),
				"Delete only project files or temporary files in /tmp")
		}
	}
	return p.deleteInProject("deleting with find", targets, true, match)
}

// checkGitClean handles git clean -f in the directory dir.
func (p *Policy) checkGitClean(dir string, dirUnknown bool) Verdict {
	if dirUnknown {
		return p.soft("git-clean", "git clean in an unknown directory (cd into a variable)", "Give the path explicitly: git -C <path> clean …")
	}
	repo := findGitRoot(dir)
	if repo == "" {
		return allowVerdict // not a git repository: git will report the error itself
	}
	if !samePath(repo, findGitRoot(p.Root)) {
		return p.soft("git-clean", fmt.Sprintf("git clean in another repository (%s): the project's restore points do not cover it", repo),
			"Delete specific files there")
	}
	if p.cfg.Strict() {
		return Verdict{Ask, "git-clean", "git clean deletes untracked files; user confirmation required"}
	}
	g, err := OpenGit(p.Root)
	if err != nil {
		return allowVerdict
	}
	files, err := g.Untracked()
	if err != nil || len(files) <= p.cfg.MaxDeletes {
		return allowVerdict
	}
	return p.snapshotCovers(g, "git clean", files)
}
