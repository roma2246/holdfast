package guard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Массовое удаление в проекте разрешается, только если оно действительно
// обратимо. «Есть какой-то снимок» этого не доказывает, поэтому хук:
//
//  1. перечисляет файлы, которые команда удалит;
//  2. прямо сейчас делает точку восстановления;
//  3. проверяет, что каждый удаляемый файл в неё попал.
//
// Файлы, которые git игнорирует, в точку не попадают — откат их не вернёт.
// Исключение — заведомо пересоздаваемые папки и файлы (node_modules, dist,
// *.pyc …) из disposable_paths: их можно удалять без точки.

// maxScanFiles — предел обхода при оценке удаления.
const maxScanFiles = 20000

// deleteScan — что удалит команда.
type deleteScan struct {
	files      []string // абсолютные пути файлов, кроме пересоздаваемых
	inGit      bool     // затрагивает служебную папку .git
	tooMany    bool     // файлов больше maxScanFiles
	disposable bool     // встретились пересоздаваемые папки или файлы
}

// isDisposable — путь (относительно корня проекта) пересоздаваемый:
// шаблон без '/' сравнивается с каждой частью пути, с '/' — со всем путём.
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

// scanDelete перечисляет файлы, которые удалит команда.
// targets — абсолютные пути (можно с glob); recursive — как rm -r;
// match — фильтр по имени (find -name), nil — без фильтра.
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
	// walk обходит папку; all=true — фильтр по имени уже не применяется
	// (папка сама подошла под фильтр и удаляется целиком).
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
				// Без фильтра удаление затронет .git целиком. С фильтром —
				// только если под него подходит что-то внутри.
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

// gitInternalsMatch — подходит ли под фильтр хоть один файл внутри .git.
func gitInternalsMatch(gitDir string, match func(string) bool) bool {
	found, seen := false, 0
	_ = filepath.WalkDir(gitDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > maxScanFiles {
			found = true // не смогли проверить всё — считаем, что затронет
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

// deleteInProject решает, можно ли удалить targets внутри проекта.
func (p *Policy) deleteInProject(what string, targets []string, recursive bool, match func(string) bool) Verdict {
	if len(targets) == 0 {
		return allowVerdict
	}
	s := p.scanDelete(targets, recursive, match)
	if s.inGit {
		return Verdict{Deny, "delete-git-dir", what + " would touch the .git directory, which holds the project history and the restore points. Name specific project files or directories"}
	}
	// rm -r dir удаляет и файлы внутри: правила путей применяем к каждому.
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
			p.bestEffortSnapshot(what) // на случай несохранённых правок в build/, dist/ и т.п.
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

// snapshotCovers делает точку восстановления и проверяет, что все files
// (пути относительно корня репозитория) в неё попали.
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
		return allowVerdict // всё удаляемое есть в точке: holdfast guard rollback вернёт
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

// bestEffortSnapshot делает точку, не влияя на решение.
func (p *Policy) bestEffortSnapshot(what string) {
	g, err := OpenGit(p.Root)
	if err != nil {
		return
	}
	_ = EnsureStateDir(p.Root)
	_, _, _ = g.Checkpoint(p.Now(), SnapshotMeta{Kind: KindPreDelete, Session: p.Session, Prompt: what}, p.cfg.KeepSnapshots)
}

// repoRel переводит абсолютный путь внутри проекта в путь относительно
// корня git-репозитория. Проект может быть подпапкой репозитория, а git
// возвращает корень с раскрытыми символическими ссылками (/private/var/…).
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

// checkFind — find с удалением (-delete, -exec rm).
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

	// Фильтр по имени понимаем только в простом виде: -name/-iname без отрицаний.
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
			// временные файлы вне проекта
		default:
			return p.soft("rm-outside-repo", fmt.Sprintf("find with deletion outside the project (%s)", abs),
				"Delete only project files or temporary files in /tmp")
		}
	}
	return p.deleteInProject("deleting with find", targets, true, match)
}

// checkGitClean — git clean -f в папке dir.
func (p *Policy) checkGitClean(dir string, dirUnknown bool) Verdict {
	if dirUnknown {
		return p.soft("git-clean", "git clean in an unknown directory (cd into a variable)", "Give the path explicitly: git -C <path> clean …")
	}
	repo := findGitRoot(dir)
	if repo == "" {
		return allowVerdict // не git-репозиторий: git сам сообщит об ошибке
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
