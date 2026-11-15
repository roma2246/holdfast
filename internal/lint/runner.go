package lint

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sync/errgroup"
)

// skipDirs is the directories the walk does not enter.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true,
	"dist": true, "build": true, "target": true,
}

// Discover finds instruction files (CLAUDE.md, AGENTS.md, ...) in the root and
// in nested directories. Hidden directories (.git, .holdfast) are skipped.
func Discover(root string, names []string) ([]string, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []string
	// WalkDir calls the function for every file and directory in the tree.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable directories are simply skipped
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (skipDirs[name] || (len(name) > 1 && name[0] == '.')) {
				return filepath.SkipDir
			}
			return nil
		}
		if want[d.Name()] {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// Run checks files with all of rules and returns the findings sorted.
func Run(ctx context.Context, repo *Repo, files []string, rules []Rule) ([]Finding, error) {
	results := make([][]Finding, len(files))

	// errgroup is a group of goroutines: Wait() waits for all of them and
	// returns the first error. SetLimit caps how many run at the same time.
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, path := range files {
		// Each goroutine writes only to its own slot results[i], so no
		// mutex is needed.
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(repo.Root, path)
			if err != nil {
				rel = path
			}
			doc, err := ParseFile(path, filepath.ToSlash(rel))
			if err != nil {
				return err
			}
			results[i] = CheckDoc(doc, repo, rules)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var all []Finding
	for _, r := range results {
		all = append(all, r...)
	}
	SortFindings(all)
	return all, nil
}

// CheckDoc applies the rules to one document, honoring the disabling comments.
func CheckDoc(doc *Doc, repo *Repo, rules []Rule) []Finding {
	var out []Finding
	for _, rule := range rules {
		if repo.Config.IsDisabled(rule.ID()) {
			continue
		}
		for _, f := range rule.Check(doc, repo) {
			if !doc.IsDisabled(f.RuleID, f.Line) {
				out = append(out, f)
			}
		}
	}
	return out
}

// ResolveTarget turns the PATH argument of the lint command into a repository
// root and a list of files. If PATH is a file, only that file is checked.
func ResolveTarget(path string) (root string, single string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return abs, "", nil
	}
	return FindRoot(filepath.Dir(abs)), abs, nil
}

// FindRoot walks up from dir to a directory with .git or holdfast.toml.
// If there is none, it returns dir itself.
func FindRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		for _, marker := range []string{".git", "holdfast.toml"} {
			if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
				return d
			}
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}
