package guard

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// maxDepth — сколько уровней bash -c "bash -c '...'" мы разбираем.
const maxDepth = 5

// CheckCommand разбирает shell-команду в AST и проверяет каждую простую
// команду: цепочки (&&, ;, |), подстановки $(...), bash -c "...".
// Поиск подстроки не используется: "echo 'rm -rf /'" — это безопасный echo.
//
// `cd` внутри команды учитывается: после `cd ~` путь `projects` означает
// ~/projects. Policy поэтому не потокобезопасна для CheckCommand.
func (p *Policy) CheckCommand(src string) Verdict {
	saved := p.Cwd
	p.touched = nil
	defer func() { p.Cwd, p.cwdUnknown = saved, false }()
	return p.checkScript(src, 0)
}

// trackCd обновляет текущую папку после `cd X`.
func (p *Policy) trackCd(args []word) {
	switch {
	case len(args) < 2:
		p.Cwd = p.Home
	case args[1].dynamic || args[1].s == "-":
		p.cwdUnknown = true
	default:
		p.Cwd = p.abs(args[1].s)
	}
}

func (p *Policy) checkScript(src string, depth int) Verdict {
	if depth > maxDepth {
		return p.soft("too-deep", "слишком глубокая вложенность bash -c", "Разбей команду на несколько простых шагов")
	}
	file, err := parseShell(src)
	if err != nil {
		return p.soft("parse-error", fmt.Sprintf("не удалось разобрать команду (%v)", err), "Перепиши команду в простом синтаксисе bash")
	}
	v := allowVerdict
	// syntax.Walk обходит всё дерево, включая $(...), <(...), тела
	// функций, циклов и условий. Функция возвращает true — «идти глубже».
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			v = worst(v, p.checkCall(n, depth))
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				v = worst(v, p.checkPipe(n))
			}
		case *syntax.Redirect:
			v = worst(v, p.checkRedirect(n))
		}
		return true
	})
	return v
}

// parseShell разбирает команду как bash, а если не вышло — как zsh: на macOS
// команды агента выполняет zsh, и агент пишет, например, ${(P)var}.
// Поддержка zsh в mvdan.cc/sh экспериментальная, поэтому она — запасной путь.
func parseShell(src string) (*syntax.File, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err == nil {
		return file, nil
	}
	if zfile, zerr := syntax.NewParser(syntax.Variant(syntax.LangZsh)).Parse(strings.NewReader(src), ""); zerr == nil {
		return zfile, nil
	}
	return nil, err // показываем ошибку bash — она понятнее агенту
}

// word — аргумент команды после разбора кавычек.
type word struct {
	s       string // значение; $VAR и $(...) остаются как текст
	dynamic bool   // содержит переменные/подстановки — значение неизвестно
	node    *syntax.Word
}

// wordValue собирает значение слова из частей: 'a'"b"c → abc.
func wordValue(w *syntax.Word) word {
	var b strings.Builder
	dyn := false
	var walk func(parts []syntax.WordPart, quoted bool)
	walk = func(parts []syntax.WordPart, quoted bool) {
		for _, part := range parts {
			// «type switch»: проверяем конкретный тип значения интерфейса.
			switch x := part.(type) {
			case *syntax.Lit:
				b.WriteString(unescape(x.Value, quoted))
			case *syntax.SglQuoted:
				b.WriteString(x.Value)
			case *syntax.DblQuoted:
				walk(x.Parts, true)
			case *syntax.ParamExp:
				if x.Param != nil && x.Param.Value == "HOME" && x.Exp == nil && x.Slice == nil && x.Repl == nil {
					b.WriteString("$HOME") // раскроем как домашнюю папку
				} else {
					dyn = true
					name := "VAR"
					if x.Param != nil {
						name = x.Param.Value
					}
					b.WriteString("$" + name)
				}
			default: // $(...), <(...), $((...)), {a,b} и т.п.
				dyn = true
				b.WriteString("$(...)")
			}
		}
	}
	walk(w.Parts, false)
	return word{s: b.String(), dynamic: dyn, node: w}
}

// unescape убирает обратные слэши: rm\ -rf → "rm -rf" как в shell.
func unescape(s string, quoted bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			next := s[i+1]
			if !quoted || strings.IndexByte("\"\\$`", next) >= 0 {
				i++
				b.WriteByte(next)
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func callWords(c *syntax.CallExpr) []word {
	out := make([]word, 0, len(c.Args))
	for _, a := range c.Args {
		out = append(out, wordValue(a))
	}
	return out
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}
var interpreters = map[string]bool{"python": true, "python3": true, "perl": true, "ruby": true, "node": true}
var downloaders = map[string]bool{"curl": true, "wget": true}

// stripWrappers убирает sudo, env, nohup, time, timeout... и возвращает
// «настоящую» команду. viaXargs=true, если аргументы придут из xargs.
func stripWrappers(args []word) (rest []word, viaXargs bool) {
	for len(args) > 0 {
		name := filepath.Base(args[0].s)
		switch name {
		case "sudo", "doas":
			args = skipFlags(args[1:], map[string]bool{"-u": true, "-g": true, "-C": true, "-D": true})
		case "env":
			args = stripEnv(args[1:])
		case "command", "builtin", "exec", "nohup", "time", "stdbuf":
			args = skipFlags(args[1:], nil)
		case "nice":
			args = skipFlags(args[1:], map[string]bool{"-n": true})
		case "timeout":
			args = skipFlags(args[1:], map[string]bool{"-s": true, "-k": true})
			if len(args) > 0 {
				args = args[1:] // длительность
			}
		case "xargs":
			viaXargs = true
			args = skipFlags(args[1:], map[string]bool{"-I": true, "-n": true, "-P": true, "-L": true, "-d": true, "-s": true, "-E": true})
		default:
			return args, viaXargs
		}
	}
	return args, viaXargs
}

// stripEnv пропускает опции и присваивания команды env и возвращает
// саму запускаемую команду: env -i -u NAME -C dir VAR=1 cmd args…
func stripEnv(args []word) []word {
	for len(args) > 0 {
		a := args[0].s
		switch {
		case a == "--":
			return args[1:]
		case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir":
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		case a == "-S" || a == "--split-string":
			// env -S "cmd arg…": команда записана одной строкой.
			if len(args) < 2 {
				return nil
			}
			var split []word
			for _, f := range strings.Fields(args[1].s) {
				split = append(split, word{s: f, dynamic: args[1].dynamic})
			}
			return stripEnv(append(split, args[2:]...))
		case strings.HasPrefix(a, "-") && a != "-":
			args = args[1:] // -i, --ignore-environment, --unset=NAME, -uNAME …
		case strings.Contains(a, "="):
			args = args[1:] // VAR=value
		default:
			return args
		}
	}
	return args
}

func skipFlags(args []word, withValue map[string]bool) []word {
	for len(args) > 0 && strings.HasPrefix(args[0].s, "-") && args[0].s != "-" {
		if args[0].s == "--" {
			return args[1:]
		}
		takesValue := withValue[args[0].s]
		args = args[1:]
		if takesValue && len(args) > 0 {
			args = args[1:]
		}
	}
	return args
}

func strs(ws []word) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.s
	}
	return out
}

func (p *Policy) checkCall(c *syntax.CallExpr, depth int) Verdict {
	args, viaXargs := stripWrappers(callWords(c))
	if len(args) == 0 {
		return allowVerdict // только присваивания VAR=x
	}
	name := filepath.Base(args[0].s)
	v := allowVerdict

	// Любое упоминание файла с секретами в аргументах — кроме команд,
	// которые смотрят только на сам файл, а не на его содержимое.
	if !metadataOnly(name, args[1:]) {
		for _, a := range args[1:] {
			if !a.dynamic {
				v = worst(v, p.checkSensitive(p.abs(a.s)))
			}
		}
	}

	switch {
	case name == "cd" || name == "pushd":
		p.trackCd(args)
	case shells[name]:
		v = worst(v, p.checkShellCall(args, depth))
	case name == "eval":
		v = worst(v, p.checkScript(strings.Join(strs(args[1:]), " "), depth+1))
	case name == "rm":
		v = worst(v, p.checkRm(args[1:], viaXargs))
	case name == "git":
		v = worst(v, p.checkGit(args[1:]))
	case name == "holdfast" && isHoldfastRollback(args[1:]):
		// Откат перезаписывает файлы проекта, в том числе правки самого
		// пользователя, — это решает человек, а не агент.
		v = worst(v, Verdict{Ask, "holdfast-rollback", "откат перезапишет файлы проекта — нужно подтверждение пользователя"})
	case name == "npm" || name == "pnpm" || name == "yarn" || name == "bun":
		if hasArg(args[1:], "publish") {
			v = worst(v, p.soft("npm-publish", name+" publish необратимо публикует пакет",
				"Подготовь версию и changelog, а публикацию оставь человеку или CI"))
		}
	case name == "find":
		v = worst(v, p.checkFind(args[1:]))
	}

	v = worst(v, p.checkMutatingPaths(name, args[1:]))
	v = worst(v, p.checkConfigured(args))
	return v
}

// isHoldfastRollback распознаёт `holdfast [-C dir] guard rollback …`.
func isHoldfastRollback(args []word) bool {
	for len(args) > 0 && strings.HasPrefix(args[0].s, "-") {
		if (args[0].s == "-C" || args[0].s == "--dir") && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return len(args) >= 2 && args[0].s == "guard" && args[1].s == "rollback"
}

// metadataCommands не читают содержимое файла: существует ли он, размер,
// права, игнорируется ли git. Их можно применять и к .env.
var metadataCommands = map[string]bool{
	"ls": true, "stat": true, "test": true, "[": true, "file": true, "du": true,
	"realpath": true, "readlink": true, "basename": true, "dirname": true, "chmod": true,
}

var metadataGitCommands = map[string]bool{"check-ignore": true, "ls-files": true, "status": true}

func metadataOnly(name string, args []word) bool {
	if name == "git" {
		sub, _, _, _ := gitSubcommand(args, "", false)
		return metadataGitCommands[sub]
	}
	return metadataCommands[name]
}

// gitSubcommand пропускает глобальные опции git (-C dir, -c key=val, ...)
// и возвращает подкоманду, её аргументы и папку, в которой выполнится git.
func gitSubcommand(args []word, dir string, unknown bool) (sub string, rest []word, outDir string, dirUnknown bool) {
	for len(args) > 0 && strings.HasPrefix(args[0].s, "-") {
		opt := args[0].s
		if (opt == "-C" || opt == "-c" || opt == "--git-dir" || opt == "--work-tree") && len(args) > 1 {
			if opt == "-C" {
				switch val := args[1]; {
				case val.dynamic:
					unknown = true
				case filepath.IsAbs(val.s):
					dir, unknown = val.s, false
				default:
					dir = filepath.Join(dir, val.s)
				}
			}
			args = args[2:]
			continue
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return "", nil, dir, unknown
	}
	return args[0].s, args[1:], dir, unknown
}

func hasArg(args []word, s string) bool {
	for _, a := range args {
		if a.s == s {
			return true
		}
	}
	return false
}

func execsRm(args []word) bool {
	for i, a := range args {
		if (a.s == "-exec" || a.s == "-execdir") && i+1 < len(args) && filepath.Base(args[i+1].s) == "rm" {
			return true
		}
	}
	return false
}

// checkShellCall: bash -c "..." разбираем рекурсивно; bash <(curl ...) — запрет.
func (p *Policy) checkShellCall(args []word, depth int) Verdict {
	v := allowVerdict
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a.s, "-") && !strings.HasPrefix(a.s, "--") && strings.Contains(a.s, "c") && i+1 < len(args) {
			v = worst(v, p.checkScript(args[i+1].s, depth+1))
			i++
		}
	}
	// Код для shell получается из сети: bash <(curl ...), sh -c "$(wget ...)".
	for _, a := range args[1:] {
		if containsDownloader(a.node) {
			v = worst(v, curlPipeVerdict())
		}
	}
	return v
}

func curlPipeVerdict() Verdict {
	return Verdict{Deny, "curl-pipe-shell",
		"выполнение скрипта, скачанного из сети (curl | sh), запрещено. Скачай файл (curl -o), покажи его пользователю и запускай только после проверки"}
}

// containsDownloader — есть ли внутри узла вызов curl/wget.
func containsDownloader(n syntax.Node) bool {
	found := false
	syntax.Walk(n, func(node syntax.Node) bool {
		if c, ok := node.(*syntax.CallExpr); ok {
			args, _ := stripWrappers(callWords(c))
			if len(args) > 0 && downloaders[filepath.Base(args[0].s)] {
				found = true
			}
		}
		return !found
	})
	return found
}

// readsProgramFromStdin — выполнит ли интерпретатор код из stdin.
// `curl … | sh` и `curl … | python3` — да (опасно); а в `curl … | python3 -c '…'`
// или `curl … | node script.js` скачанное — только данные, код свой.
func readsProgramFromStdin(name string, args []word) bool {
	// Флаги, после которых идёт значение, а не имя скрипта.
	withValue := map[string]bool{"-W": true, "-X": true, "-o": true, "-O": true, "-r": true, "--require": true}
	for i := 0; i < len(args); i++ {
		s := args[i].s
		switch {
		case s == "-":
			return true // явное «программа из stdin»
		case s == "--":
			return i+1 >= len(args)
		case strings.HasPrefix(s, "--"):
			if s == "--eval" || s == "--print" || s == "--command" {
				return false
			}
		case strings.HasPrefix(s, "-"):
			flags := s[1:]
			switch {
			case shells[name] && strings.Contains(flags, "c"):
				return false // sh -c '…': код в аргументе
			case shells[name] && strings.Contains(flags, "s"):
				return true // bash -s: код из stdin
			case (name == "python" || name == "python3") && (strings.HasPrefix(flags, "c") || strings.HasPrefix(flags, "m")):
				return false // python -c '…' / python -m модуль
			case !shells[name] && name != "python" && name != "python3" && strings.ContainsAny(flags, "eEp"):
				return false // perl -e / -ne, ruby -e, node -e / -p
			}
			if withValue[s] {
				i++
			}
		default:
			return false // первый позиционный аргумент — файл скрипта
		}
	}
	return true // скрипта нет — код читается из stdin
}

// checkPipe ищет «скачать | выполнить»: curl ... | sh, wget -O- | sudo bash.
func (p *Policy) checkPipe(b *syntax.BinaryCmd) Verdict {
	var stmts []*syntax.Stmt
	var flatten func(s *syntax.Stmt)
	flatten = func(s *syntax.Stmt) {
		if bc, ok := s.Cmd.(*syntax.BinaryCmd); ok && (bc.Op == syntax.Pipe || bc.Op == syntax.PipeAll) {
			flatten(bc.X)
			flatten(bc.Y)
			return
		}
		stmts = append(stmts, s)
	}
	flatten(b.X)
	flatten(b.Y)

	downloaded := false
	for _, s := range stmts {
		if downloaded {
			if c, ok := s.Cmd.(*syntax.CallExpr); ok {
				args, _ := stripWrappers(callWords(c))
				if len(args) > 0 {
					name := filepath.Base(args[0].s)
					if (shells[name] || interpreters[name]) && readsProgramFromStdin(name, args[1:]) {
						return curlPipeVerdict()
					}
				}
			}
		}
		if containsDownloader(s) {
			downloaded = true
		}
	}
	return allowVerdict
}

func (p *Policy) checkRedirect(r *syntax.Redirect) Verdict {
	if r.Word == nil {
		return allowVerdict
	}
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc, syntax.DplIn, syntax.DplOut:
		return allowVerdict
	}
	w := wordValue(r.Word)
	if w.dynamic || w.s == "/dev/null" {
		return allowVerdict
	}
	abs := p.abs(w.s)
	v := p.checkSensitive(abs)
	if r.Op != syntax.RdrIn {
		v = worst(v, p.checkWritePath(abs))
	}
	return v
}

// Команды, аргументы которых — изменяемые пути.
var mutating = map[string]bool{
	"rm": true, "mv": true, "touch": true, "tee": true, "truncate": true, "mkdir": true,
	"rmdir": true, "chmod": true, "chown": true, "ln": true, "unlink": true, "shred": true,
}

func (p *Policy) checkMutatingPaths(name string, args []word) Verdict {
	var paths []word
	switch {
	case mutating[name]:
		paths = nonFlags(args)
	case name == "cp" || name == "rsync" || name == "install":
		if nf := nonFlags(args); len(nf) > 0 {
			paths = nf[len(nf)-1:] // меняется только место назначения
		}
	case name == "sed" || name == "perl":
		for _, a := range args {
			if strings.HasPrefix(a.s, "-i") || a.s == "--in-place" || (name == "perl" && strings.HasPrefix(a.s, "-") && strings.Contains(a.s, "i")) {
				paths = nonFlags(args)
				break
			}
		}
	case name == "dd":
		for _, a := range args {
			if strings.HasPrefix(a.s, "of=") {
				paths = append(paths, word{s: strings.TrimPrefix(a.s, "of="), dynamic: a.dynamic})
			}
		}
	}
	v := allowVerdict
	for _, a := range paths {
		if !a.dynamic {
			v = worst(v, p.checkWritePath(p.abs(a.s)))
		}
	}
	return v
}

func nonFlags(args []word) []word {
	var out []word
	afterDD := false
	for _, a := range args {
		if !afterDD && a.s == "--" {
			afterDD = true
			continue
		}
		if !afterDD && strings.HasPrefix(a.s, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// checkRm — rm -rf на корень, домашнюю папку и пути вне репозитория.
func (p *Policy) checkRm(args []word, viaXargs bool) Verdict {
	recursive := false
	var targets []word
	afterDD := false
	for _, a := range args {
		switch {
		case afterDD || !strings.HasPrefix(a.s, "-") || a.s == "-":
			targets = append(targets, a)
		case a.s == "--":
			afterDD = true
		case a.s == "--recursive":
			recursive = true
		case !strings.HasPrefix(a.s, "--"):
			// Слитые короткие флаги: -rf, -fR, -rvf.
			if strings.ContainsAny(a.s[1:], "rR") {
				recursive = true
			}
		}
	}

	if viaXargs && recursive {
		return p.soft("rm-dynamic", "rm -r с путями из xargs: цели неизвестны заранее",
			"Перечисли пути явно (сначала посмотри список без rm)")
	}

	v := allowVerdict
	var inProject []string // цели внутри проекта
	otherCount := 0        // файлы в других репозиториях
	for _, t := range targets {
		relativeAfterUnknownCd := p.cwdUnknown && !filepath.IsAbs(p.expandHome(t.s))
		if (t.dynamic && !strings.HasPrefix(t.s, "$HOME")) || relativeAfterUnknownCd {
			if recursive {
				v = worst(v, p.soft("rm-dynamic", fmt.Sprintf("rm -r с вычисляемым путём (%s): значение неизвестно", t.s),
					"Подставь путь явно, без переменных и cd на переменную"))
			}
			continue
		}
		abs := p.abs(t.s)
		// Для glob (/*, ~/*, build/*.o) проверяем папку до первого '*'.
		base := abs
		if i := strings.IndexAny(abs, "*?["); i >= 0 {
			base = filepath.Dir(abs[:i] + "x")
		}

		switch {
		case recursive && (base == "/" || base == filepath.Dir(base)):
			return Verdict{Deny, "rm-root", "rm -r на корень файловой системы запрещён"}
		case recursive && (base == p.Home || isWithin(base, p.Home)):
			return Verdict{Deny, "rm-home", "rm -r на домашнюю папку запрещён. Удаляй конкретные файлы внутри проекта"}
		case recursive && (base == p.Root || isWithin(base, p.Root)):
			// и сам репозиторий, и любая папка, которая его содержит
			return Verdict{Deny, "rm-repo-root", "rm -r на весь репозиторий запрещён. Удаляй конкретные файлы или папки"}
		case !p.cfg.Strict() && !p.inRepo(base) && p.isTempPath(base):
			// временные файлы вне проекта: удаляем без вопросов.
			// Проверка идёт после repo-root: проект сам может лежать в /tmp.
			continue
		case recursive && !p.inRepo(base):
			return Verdict{Deny, "rm-outside-repo", fmt.Sprintf("rm -r вне репозитория (%s) запрещён. Работай только с файлами проекта %s", base, p.Root)}
		case !p.cfg.Strict() && !p.inRepo(base) && p.inOtherRepo(base):
			// Отдельные файлы в другом git-репозитории (агент работает и там):
			// можно, но массовое удаление — нет, снапшот проекта их не защищает.
			otherCount += p.countTargets(abs, false, p.cfg.MaxDeletes+1-otherCount)
			continue
		case !p.inRepo(base):
			v = worst(v, p.soft("rm-outside-repo", fmt.Sprintf("удаление файла вне репозитория (%s)", abs),
				"Удаляй только файлы проекта, отдельные файлы в git-репозиториях или временные файлы в /tmp"))
			continue
		}
		inProject = append(inProject, abs)
	}
	v = worst(v, p.deleteInProject("удаление (rm)", inProject, recursive, nil))
	if otherCount > p.cfg.MaxDeletes {
		v = worst(v, p.soft("mass-delete", fmt.Sprintf("удаление более %d файлов вне проекта", p.cfg.MaxDeletes),
			"Снапшот проекта эти файлы не защищает — удаляй по несколько файлов за раз"))
	}
	return v
}

// inOtherRepo — путь внутри какого-то git-репозитория (не проекта). Репозиторий
// во всю домашнюю папку (~/.git для dotfiles) или выше не считается.
func (p *Policy) inOtherRepo(path string) bool {
	root := findGitRoot(path)
	if root == "" || root == "/" || root == p.Home || isWithin(root, p.Home) {
		return false
	}
	return true
}

// countTargets считает файлы, которые удалит rm, но не больше limit —
// чтобы не обходить огромные папки ради ответа «больше лимита».
func (p *Policy) countTargets(abs string, recursive bool, limit int) int {
	if limit <= 0 {
		return 0
	}
	matches := []string{abs}
	if strings.ContainsAny(abs, "*?[") {
		m, err := filepath.Glob(abs)
		if err != nil || len(m) == 0 {
			return 1
		}
		matches = m
	}
	n := 0
	for _, m := range matches {
		if n >= limit {
			break
		}
		if !recursive {
			n++
			continue
		}
		_ = filepath.WalkDir(m, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				n++
			}
			if n >= limit {
				return filepath.SkipAll // досрочно прекращаем обход
			}
			return nil
		})
		if n == 0 {
			n = 1 // несуществующий или пустой путь — считаем как один
		}
	}
	return n
}

// checkGit: push --force и reset --hard — запрет, обычный push — спросить.
func (p *Policy) checkGit(args []word) Verdict {
	// Папка, где выполнится git: текущая (с учётом cd) и -C из команды.
	sub, rest, dir, dirUnknown := gitSubcommand(args, p.Cwd, p.cwdUnknown)
	if sub == "" {
		return allowVerdict
	}
	switch sub {
	case "push":
		return p.checkPush(rest, p.abs(dir), dirUnknown)
	case "update-ref":
		for _, w := range rest {
			if strings.HasPrefix(w.s, "refs/holdfast") {
				return Verdict{Deny, "holdfast-snapshots", "точки восстановления — страховка пользователя, менять и удалять их агенту нельзя"}
			}
		}
		if hasArg(rest, "--stdin") {
			// Имена ссылок придут из stdin — что изменится, неизвестно.
			return p.soft("git-update-ref", "git update-ref --stdin: изменяемые ссылки неизвестны заранее",
				"Используй явную форму: git update-ref <ссылка> <значение>")
		}
	case "reset":
		if hasArg(rest, "--hard") {
			return Verdict{Deny, "git-reset-hard", "git reset --hard безвозвратно стирает незакоммиченные изменения. Используй git stash или git restore <файл> для конкретных файлов"}
		}
	case "clean":
		force, ignored := false, false
		for _, a := range rest {
			short := strings.HasPrefix(a.s, "-") && !strings.HasPrefix(a.s, "--")
			if a.s == "--force" || (short && strings.Contains(a.s, "f")) {
				force = true
			}
			if short && strings.ContainsAny(a.s, "xX") {
				ignored = true
			}
		}
		switch {
		case force && ignored:
			return p.soft("git-clean", "git clean -x удаляет и игнорируемые файлы (.env, локальные настройки) — их нет в точках восстановления",
				"Удаляй конкретные файлы или используй git clean -f без -x")
		case force:
			return p.checkGitClean(p.abs(dir), dirUnknown)
		}
	}
	return allowVerdict
}

// checkPush: force-push, удаление веток и теги запрещены в любом репозитории.
// Защищённые ветки (main) — настройка проекта: она действует только для
// репозитория проекта, а не для любого репозитория на диске, где агент
// выполнил cd. Ветку читаем у того репозитория, где реально идёт push.
func (p *Policy) checkPush(args []word, dir string, dirUnknown bool) Verdict {
	withValue := map[string]bool{"--repo": true, "-o": true, "--push-option": true, "--receive-pack": true, "--exec": true}
	var positional []string
	deleting, tags, all := false, false, false
	for i := 0; i < len(args); i++ {
		s := args[i].s
		short := strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "--")
		switch {
		case s == "--force" || strings.HasPrefix(s, "--force-with-lease") || s == "--force-if-includes" || s == "--mirror" ||
			(short && strings.Contains(s, "f")) || (strings.HasPrefix(s, "+") && len(s) > 1):
			return Verdict{Deny, "git-push-force", "git push --force переписывает историю на сервере. Сделай обычный push; если история разошлась — создай новую ветку и запушь её"}
		case s == "--delete" || s == "-d":
			deleting = true
		case s == "--tags" || s == "--follow-tags":
			tags = true
		case s == "--all" || s == "--branches":
			all = true
		case withValue[s]:
			i++
		case strings.HasPrefix(s, "-"):
		default:
			positional = append(positional, s)
		}
	}
	if p.cfg.Strict() {
		return Verdict{Ask, "git-push", "git push отправляет код на сервер — нужно подтверждение пользователя"}
	}

	var refspecs []string
	if len(positional) > 1 {
		refspecs = positional[1:]
	}
	for _, r := range refspecs {
		if strings.HasPrefix(r, ":") {
			deleting = true
		}
	}
	switch {
	case deleting:
		return Verdict{Deny, "git-push-delete", "удаление веток на сервере необратимо. Сообщи пользователю, какую ветку нужно удалить"}
	case tags:
		return Verdict{Deny, "git-push-tags", "push тегов обычно запускает релиз. Создай тег локально и предложи пользователю его запушить"}
	case all:
		return Verdict{Deny, "git-push-all", "push всех веток сразу запрещён. Запушь только свою рабочую ветку: git push origin <ветка>"}
	}

	if dirUnknown {
		return p.soft("git-push-branch", "не удалось определить, из какого репозитория push (cd на переменную)",
			"Укажи путь явно: git -C <путь> push origin <ветка>")
	}
	repo := findGitRoot(dir)
	if repo == "" {
		return allowVerdict // не git-репозиторий: git сам сообщит об ошибке
	}
	projectRepo := isWithin(p.Root, repo) || isWithin(repo, p.Root)

	cur := currentBranch(repo)
	var targets []string
	if len(refspecs) == 0 {
		targets = []string{cur}
	}
	for _, r := range refspecs {
		dst := r
		if _, after, ok := strings.Cut(r, ":"); ok {
			dst = after
		}
		if dst == "HEAD" {
			dst = cur
		}
		if strings.HasPrefix(dst, "refs/tags/") || (dst != "" && refExists(repo, "refs/tags/"+dst)) {
			return Verdict{Deny, "git-push-tags", "push тегов обычно запускает релиз. Создай тег локально и предложи пользователю его запушить"}
		}
		targets = append(targets, strings.TrimPrefix(dst, "refs/heads/"))
	}
	if !projectRepo {
		return allowVerdict // правила веток проекта к чужим репозиториям не применяем
	}
	for _, t := range targets {
		if t == "" {
			return Verdict{Deny, "git-push-branch", "не удалось определить ветку (detached HEAD?). Укажи ветку явно: git push origin <ветка>"}
		}
		for _, protected := range p.cfg.ProtectedBranches {
			if t == protected {
				return Verdict{Deny, "git-push-protected", fmt.Sprintf(
					"push в защищённую ветку %s запрещён. Создай рабочую ветку (git switch -c <имя>), запушь её (git push -u origin <имя>) и предложи pull request", t)}
			}
		}
	}
	return allowVerdict
}

// checkConfigured применяет deny_commands / ask_commands и
// синхронизированные правила-команды из конфига.
func (p *Policy) checkConfigured(args []word) Verdict {
	argv := strs(args)
	argv[0] = filepath.Base(argv[0])
	v := allowVerdict
	for _, r := range p.cfg.Synced.Rules {
		switch r.Kind {
		case "deny-command":
			if MatchCommand(argv, r.Pattern) {
				v = worst(v, Verdict{Deny, "synced-deny-command", fmt.Sprintf("команда «%s» запрещена правилом из %s", r.Pattern, r.Source)})
			}
		case "ask-command":
			if MatchCommand(argv, r.Pattern) {
				v = worst(v, Verdict{Ask, "synced-ask-command", fmt.Sprintf("команда «%s» требует подтверждения (правило из %s)", r.Pattern, r.Source)})
			}
		}
	}
	for _, pat := range p.cfg.DenyCommands {
		if MatchCommand(argv, pat) {
			v = worst(v, Verdict{Deny, "deny-command", fmt.Sprintf("команда «%s» запрещена в holdfast.toml", pat)})
		}
	}
	for _, pat := range p.cfg.AskCommands {
		if MatchCommand(argv, pat) {
			v = worst(v, Verdict{Ask, "ask-command", fmt.Sprintf("команда «%s» требует подтверждения пользователя", pat)})
		}
	}
	return v
}

// MatchCommand: первое слово шаблона совпадает с именем команды, а
// остальные слова встречаются в аргументах в том же порядке.
// "git push --force" совпадает с "git push origin main --force".
func MatchCommand(argv []string, pattern string) bool {
	fields := strings.Fields(pattern)
	if len(fields) == 0 || len(argv) == 0 || argv[0] != fields[0] {
		return false
	}
	j := 1
	for _, a := range argv[1:] {
		if j < len(fields) && a == fields[j] {
			j++
		}
	}
	return j == len(fields)
}
