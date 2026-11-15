package guard

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// The hook has to answer within milliseconds, and starting `git` takes 5–20 ms.
// So the facts the hook needs (the current branch, whether a snapshot exists,
// whether a tag exists) are read straight from the files in .git, which takes
// fractions of a millisecond.

// gitDir returns the .git directory of the repository root. In worktrees and
// submodules .git is a file with the line "gitdir: <path>".
func gitDir(root string) string {
	p := filepath.Join(root, ".git")
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return p
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	return dir
}

// findGitRoot walks up from dir to a directory that has .git.
// "" means dir is not inside a git repository.
func findGitRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// commonDir is the repository's common directory (for a worktree the refs live there).
func commonDir(gd string) string {
	data, err := os.ReadFile(filepath.Join(gd, "commondir"))
	if err != nil {
		return gd
	}
	dir := strings.TrimSpace(string(data))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(gd, dir)
	}
	return filepath.Clean(dir)
}

// currentBranch is the name of the current branch; "" means detached HEAD or not git.
func currentBranch(root string) string {
	gd := gitDir(root)
	if gd == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(gd, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if !strings.HasPrefix(head, "ref: refs/heads/") {
		return ""
	}
	return strings.TrimPrefix(head, "ref: refs/heads/")
}

// refExists looks for a ref both as a file and in packed-refs.
func refExists(root, ref string) bool {
	gd := gitDir(root)
	if gd == "" {
		return false
	}
	cd := commonDir(gd)
	if _, err := os.Stat(filepath.Join(cd, filepath.FromSlash(ref))); err == nil {
		return true
	}
	return packedRefsHas(cd, func(name string) bool { return name == ref })
}

func packedRefsHas(commonDir string, match func(string) bool) bool {
	f, err := os.Open(filepath.Join(commonDir, "packed-refs"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Line format: "<sha> <refname>".
		if _, name, ok := strings.Cut(sc.Text(), " "); ok && match(name) {
			return true
		}
	}
	return false
}
