package guard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// The "which files does the command really touch" layer. Path restrictions,
// protected paths and secrets apply to the resulting files, not to the way
// the path is spelled:
//
//	rm *.sum, cat .en[v], cat .e{n,x}v  are expanded the way the shell would;
//	cat alias.txt (a symlink to .env)   symlinks are followed;
//	cp go.sum .                         the resulting path is ./go.sum.

// maxBraceForms is how many {a,b}{c,d} variants we expand (more is pointless).
const maxBraceForms = 256

// targets is the absolute paths a word argument stands for: after {…} and
// glob expansion, plus the files that symlinks point to.
func (p *Policy) targets(w word) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, abs := range p.expansions(w) {
		add(abs)
		add(p.realPath(abs))
	}
	return out
}

// expansions is the paths as the command will see them after {…} and glob
// expansion (without following symlinks).
func (p *Policy) expansions(w word) []string {
	glob := hasGlob(w)
	var out []string
	for _, s := range braceForms(w) {
		abs := p.abs(s)
		if glob && strings.ContainsAny(s, "*?[") {
			if m := shellGlob(abs); len(m) > 0 {
				out = append(out, m...)
				continue
			}
		}
		out = append(out, abs) // the shell passes a glob with no matches through as is
	}
	return out
}

// hasGlob reports whether the word has *, ? or [ outside quotes: 'a*' is just a name.
func hasGlob(w word) bool {
	if w.node == nil {
		return strings.ContainsAny(w.s, "*?[")
	}
	for _, part := range w.node.Parts {
		if lit, ok := part.(*syntax.Lit); ok && strings.ContainsAny(lit.Value, "*?[") {
			return true
		}
	}
	return false
}

// braceForms expands {a,b} and {1..3} outside quotes: .e{n,x}v → .env, .exv.
func braceForms(w word) []string {
	if w.node == nil || !strings.Contains(w.s, "{") {
		return []string{w.s}
	}
	// SplitBraces modifies the word, so we work on a copy of its parts.
	cp := &syntax.Word{}
	for _, part := range w.node.Parts {
		if lit, ok := part.(*syntax.Lit); ok {
			c := *lit // a copy of the struct, not of the pointer
			part = &c
		}
		cp.Parts = append(cp.Parts, part)
	}
	if !syntax.SplitBraces(cp) {
		return []string{w.s}
	}
	var out []string
	// range over an iterator function (Go 1.23+): the loop receives (word, error) pairs.
	for bw, err := range expand.BracesSeq(nil, cp) {
		if err != nil || len(out) >= maxBraceForms {
			break
		}
		out = append(out, wordValue(bw).s)
	}
	if len(out) == 0 {
		return []string{w.s}
	}
	return out
}

// shellGlob expands a pattern the way bash and zsh do by default: * and ? do
// not match dot names (.env) unless the dot is written explicitly.
// filepath.Glob does not know this rule, so we filter the results ourselves.
func shellGlob(pattern string) []string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	pat := strings.Split(pattern, string(filepath.Separator))
	out := matches[:0] // in-place filter: the same backing array, a new length
	for _, m := range matches {
		parts := strings.Split(m, string(filepath.Separator))
		hidden := false
		for i := 0; i < len(parts) && i < len(pat); i++ {
			if strings.HasPrefix(parts[i], ".") && !strings.HasPrefix(pat[i], ".") && strings.ContainsAny(pat[i], "*?[") {
				hidden = true
				break
			}
		}
		if !hidden {
			out = append(out, m)
		}
	}
	return out
}

// realPath is where a path really leads after all symlinks. A path inside the
// project is returned in its "project" spelling (Root/…), even if Root
// itself sits behind a symlink (/tmp → /private/tmp on macOS).
func (p *Policy) realPath(abs string) string {
	real := p.resolveLinks(abs)
	if real == abs {
		return abs
	}
	if p.realRoot == "" {
		p.realRoot = p.Root
		if r, err := filepath.EvalSymlinks(p.Root); err == nil {
			p.realRoot = r
		}
	}
	if p.realRoot != p.Root && isWithin(p.realRoot, real) {
		rel, _ := filepath.Rel(p.realRoot, real)
		return filepath.Join(p.Root, rel)
	}
	return real
}

// resolveLinks follows symlinks. Unlike filepath.EvalSymlinks, it also works
// for a dangling symlink to a file that does not exist yet:
// alias.txt → .env, even when .env is not there so far.
func (p *Policy) resolveLinks(abs string) string {
	path := abs
	for range 40 { // like the kernel: at most 40 hops, protection against loops
		path = filepath.Join(p.realDir(filepath.Dir(path)), filepath.Base(path))
		target, err := os.Readlink(path)
		if err != nil {
			return path // not a symlink, or the file does not exist
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = filepath.Clean(target)
	}
	return path
}

// realDir is a directory with symlinks resolved. The arguments of one command
// usually live in the same directories, so the answer is cached: the hook has
// to respond within fractions of a millisecond.
func (p *Policy) realDir(dir string) string {
	if r, ok := p.dirCache[dir]; ok {
		return r
	}
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		r = dir
	}
	if p.dirCache == nil {
		p.dirCache = map[string]string{}
	}
	p.dirCache[dir] = r
	return r
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// copyTargets is where cp/mv/ln/install/rsync will write: the destination and,
// if it is a directory, the files inside it (cp a.txt dir/ → dir/a.txt),
// including the contents of copied directories. tooMany means more files than maxScanFiles.
func (p *Policy) copyTargets(name string, args []word) (paths []string, tooMany bool) {
	var dest *word
	var pos []word
	afterDD := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case afterDD || !strings.HasPrefix(a.s, "-") || a.s == "-":
			pos = append(pos, a)
		case a.s == "--":
			afterDD = true
		case a.s == "-t" || a.s == "--target-directory":
			if i+1 < len(args) {
				d := args[i+1]
				dest = &d
				i++
			}
		case strings.HasPrefix(a.s, "--target-directory="):
			dest = &word{s: strings.TrimPrefix(a.s, "--target-directory="), dynamic: a.dynamic}
		}
	}
	intoDir := dest != nil
	if dest == nil {
		if len(pos) == 0 {
			return nil, false
		}
		d := pos[len(pos)-1]
		dest, pos = &d, pos[:len(pos)-1]
	}
	if dest.dynamic {
		return nil, false
	}
	destAbs := p.abs(dest.s)
	paths = p.targets(*dest)
	if !intoDir {
		intoDir = len(pos) > 1 || strings.HasSuffix(dest.s, "/") || isDir(destAbs)
	}
	count := 0
	for _, src := range pos {
		if src.dynamic {
			continue
		}
		for _, s := range p.expansions(src) {
			landing := destAbs
			// rsync src/ dst and cp -r src/. dst copy the contents of src, not the directory itself.
			contents := (name == "rsync" && strings.HasSuffix(src.s, "/")) || strings.HasSuffix(src.s, "/.")
			if intoDir && !contents {
				landing = filepath.Join(destAbs, filepath.Base(s))
			}
			paths = append(paths, landing, p.realPath(landing))
			if name == "ln" || !isDir(s) || !p.mayHoldRules(landing) {
				continue
			}
			_ = filepath.WalkDir(s, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if count++; count > maxScanFiles {
					tooMany = true
					return filepath.SkipAll
				}
				rel, _ := filepath.Rel(s, path)
				paths = append(paths, filepath.Join(landing, rel))
				return nil
			})
		}
	}
	return paths, tooMany
}

// mayHoldRules reports whether a directory may end up holding a file covered
// by rules: it is inside the project or contains service files (~/.claude).
func (p *Policy) mayHoldRules(dir string) bool {
	return p.inRepo(dir) || p.isService(dir) || p.containsService(dir)
}

// --- Holdfast service files ---

// The protection settings and the restore points are the user's safety net.
// The agent must not change them, neither by an edit nor by a command.
type serviceSet struct {
	files []string // specific files
	dirs  []string // whole directories
}

var claudeSettings = regexp.MustCompile(`^settings.*\.json$`)

func (p *Policy) serviceSet() *serviceSet {
	if p.services != nil {
		return p.services
	}
	s := &serviceSet{
		files: []string{filepath.Join(p.Root, "holdfast.toml")},
		dirs:  []string{filepath.Join(p.Root, ".holdfast")},
	}
	for _, base := range []string{p.Root, p.Home} {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			s.files = append(s.files, filepath.Join(base, ".claude", name))
		}
	}
	// Snapshots live in the repository's common directory (for a worktree, not in the project's .git).
	if gd := gitDir(p.Root); gd != "" {
		cd := commonDir(gd)
		s.files = append(s.files, filepath.Join(cd, "packed-refs"))
		s.dirs = append(s.dirs, filepath.Join(cd, "refs", "holdfast"), filepath.Join(cd, "holdfast"),
			filepath.Join(cd, "logs", "refs", "holdfast"))
	}
	p.services = s
	return s
}

// isService reports whether a path is a Holdfast service file or lies in one of its directories.
func (p *Policy) isService(abs string) bool {
	s := p.serviceSet()
	for _, f := range s.files {
		if abs == f {
			return true
		}
	}
	for _, d := range s.dirs {
		// refs/holdfast-old and the like are snapshots too
		if isWithin(d, abs) || strings.HasPrefix(abs, d+"-") {
			return true
		}
	}
	// .claude/settings*.json of the project and of the user
	dir := filepath.Dir(abs)
	if (dir == filepath.Join(p.Root, ".claude") || dir == filepath.Join(p.Home, ".claude")) && claudeSettings.MatchString(filepath.Base(abs)) {
		return true
	}
	return false
}

// containsService reports whether the directory abs has service files inside (mv .claude x).
// The project itself and the directories above it don't count: they have their own rules.
func (p *Policy) containsService(abs string) bool {
	if isWithin(abs, p.Root) {
		return false
	}
	s := p.serviceSet()
	for _, f := range append(s.files, s.dirs...) {
		if isWithin(abs, f) {
			return true
		}
	}
	return false
}

func (p *Policy) serviceVerdict(abs string) Verdict {
	name := abs
	if p.inRepo(abs) {
		name = p.rel(abs)
	}
	return p.soft("holdfast-config",
		fmt.Sprintf("%s is a Holdfast service file: protection settings or restore points", name),
		"The agent does not change these itself. Describe the edit to the user and let them make it by hand")
}

// noteGitConfigWrite records a write to a git configuration file: after it,
// the destination of a git push in this same command can no longer be read from files.
func (p *Policy) noteGitConfigWrite(abs string) {
	base := filepath.Base(abs)
	if (base == "config" || base == "config.worktree") && filepath.Base(filepath.Dir(abs)) == ".git" ||
		base == ".gitconfig" || strings.HasSuffix(abs, filepath.Join(".config", "git", "config")) {
		p.gitConfigChanged = true
	}
}
