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

// Слой «какие файлы на самом деле затрагивает команда». Запреты путей,
// защищённые папки и секреты применяются к итоговым файлам, а не к тому,
// как путь написан:
//
//	rm *.sum, cat .en[v], cat .e{n,x}v — раскрываем, как это сделает shell;
//	cat alias.txt (ссылка на .env)      — идём по символическим ссылкам;
//	cp go.sum .                         — итоговый путь: ./go.sum.

// maxBraceForms — сколько вариантов {a,b}{c,d} раскрываем (дальше — бессмысленно).
const maxBraceForms = 256

// targets — абсолютные пути, которые обозначает слово-аргумент: после
// раскрытия {…} и glob, плюс файлы, на которые указывают ссылки.
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

// expansions — пути, как их увидит команда после раскрытия {…} и glob
// (без перехода по ссылкам).
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
		out = append(out, abs) // glob без совпадений shell передаёт как есть
	}
	return out
}

// hasGlob — есть ли в слове *, ? или [ вне кавычек: 'a*' — это просто имя.
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

// braceForms раскрывает {a,b} и {1..3} вне кавычек: .e{n,x}v → .env, .exv.
func braceForms(w word) []string {
	if w.node == nil || !strings.Contains(w.s, "{") {
		return []string{w.s}
	}
	// SplitBraces меняет слово, поэтому работаем с копией его частей.
	cp := &syntax.Word{}
	for _, part := range w.node.Parts {
		if lit, ok := part.(*syntax.Lit); ok {
			c := *lit // копия структуры, а не указателя
			part = &c
		}
		cp.Parts = append(cp.Parts, part)
	}
	if !syntax.SplitBraces(cp) {
		return []string{w.s}
	}
	var out []string
	// range по функции-итератору (Go 1.23+): цикл получает пары (слово, ошибка).
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

// shellGlob раскрывает шаблон как bash и zsh по умолчанию: * и ? не
// совпадают с именами на точку (.env), если точка не написана явно.
// filepath.Glob этого правила не знает — отфильтровываем сами.
func shellGlob(pattern string) []string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	pat := strings.Split(pattern, string(filepath.Separator))
	out := matches[:0] // фильтр «на месте»: тот же массив, новая длина
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

// realPath — куда на самом деле ведёт путь после всех символических ссылок.
// Путь внутри проекта возвращается в «проектном» написании (Root/…), даже
// если сам Root лежит за ссылкой (/tmp → /private/tmp на macOS).
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

// resolveLinks идёт по символическим ссылкам. В отличие от
// filepath.EvalSymlinks, работает и для «висячей» ссылки на ещё не
// созданный файл: alias.txt → .env, даже если .env пока нет.
func (p *Policy) resolveLinks(abs string) string {
	path := abs
	for range 40 { // как в ядре: не больше 40 переходов, защита от циклов
		path = filepath.Join(p.realDir(filepath.Dir(path)), filepath.Base(path))
		target, err := os.Readlink(path)
		if err != nil {
			return path // не ссылка или файла нет
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = filepath.Clean(target)
	}
	return path
}

// realDir — папка после раскрытия ссылок. Аргументы одной команды обычно
// лежат в одних и тех же папках, поэтому ответ кэшируется: хук должен
// отвечать за доли миллисекунды.
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

// copyTargets — куда запишут cp/mv/ln/install/rsync: место назначения, а
// если это папка — файлы внутри неё (cp a.txt dir/ → dir/a.txt), включая
// содержимое копируемых папок. tooMany — файлов больше maxScanFiles.
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
			// rsync src/ dst и cp -r src/. dst копируют содержимое src, а не саму папку.
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

// mayHoldRules — может ли в папке оказаться файл под правилами: она внутри
// проекта или содержит служебные файлы (~/.claude).
func (p *Policy) mayHoldRules(dir string) bool {
	return p.inRepo(dir) || p.isService(dir) || p.containsService(dir)
}

// --- Служебные файлы Holdfast ---

// Настройки защиты и точки восстановления — страховка пользователя.
// Агент не должен менять их ни правкой, ни командой.
type serviceSet struct {
	files []string // конкретные файлы
	dirs  []string // папки целиком
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
	// Снимки лежат в общей папке репозитория (для worktree — не в .git проекта).
	if gd := gitDir(p.Root); gd != "" {
		cd := commonDir(gd)
		s.files = append(s.files, filepath.Join(cd, "packed-refs"))
		s.dirs = append(s.dirs, filepath.Join(cd, "refs", "holdfast"), filepath.Join(cd, "holdfast"),
			filepath.Join(cd, "logs", "refs", "holdfast"))
	}
	p.services = s
	return s
}

// isService — путь является служебным файлом Holdfast или лежит в его папке.
func (p *Policy) isService(abs string) bool {
	s := p.serviceSet()
	for _, f := range s.files {
		if abs == f {
			return true
		}
	}
	for _, d := range s.dirs {
		// refs/holdfast-old и т.п. — тоже снимки
		if isWithin(d, abs) || strings.HasPrefix(abs, d+"-") {
			return true
		}
	}
	// .claude/settings*.json проекта и пользователя
	dir := filepath.Dir(abs)
	if (dir == filepath.Join(p.Root, ".claude") || dir == filepath.Join(p.Home, ".claude")) && claudeSettings.MatchString(filepath.Base(abs)) {
		return true
	}
	return false
}

// containsService — внутри папки abs есть служебные файлы (mv .claude x).
// Сам проект и папки над ним не считаются: для них есть свои правила.
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

// noteGitConfigWrite отмечает запись в файл конфигурации git: после неё
// назначение git push в этой же команде по файлам уже не определить.
func (p *Policy) noteGitConfigWrite(abs string) {
	base := filepath.Base(abs)
	if (base == "config" || base == "config.worktree") && filepath.Base(filepath.Dir(abs)) == ".git" ||
		base == ".gitconfig" || strings.HasSuffix(abs, filepath.Join(".config", "git", "config")) {
		p.gitConfigChanged = true
	}
}
