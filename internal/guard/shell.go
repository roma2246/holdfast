package guard

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
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
	p.dirStack, p.dirFuncs = nil, nil
	p.branches, p.gitConfigChanged, p.gitEnv = nil, false, false
	p.dirCache = nil // команда может создать ссылку — не верим старому кэшу
	defer func() { p.Cwd, p.cwdUnknown = saved, false }()
	return p.checkScript(src, 0)
}

// dirEntry — папка в стеке pushd.
type dirEntry struct {
	dir     string
	unknown bool
}

// trackCd обновляет текущую папку после cd, pushd и popd.
func (p *Policy) trackCd(name string, args []word) {
	switch name {
	case "popd":
		if n := len(p.dirStack); n > 0 {
			top := p.dirStack[n-1]
			p.Cwd, p.cwdUnknown, p.dirStack = top.dir, top.unknown, p.dirStack[:n-1]
		} else {
			p.cwdUnknown = true
		}
		return
	case "pushd":
		p.dirStack = append(p.dirStack, dirEntry{p.Cwd, p.cwdUnknown})
	}
	args = skipFlags(args[1:], nil) // cd -P dir, cd -L dir
	switch {
	case len(args) == 0 && name == "cd":
		p.Cwd, p.cwdUnknown = p.Home, false
	case len(args) == 0, args[0].dynamic, args[0].s == "-", strings.HasPrefix(args[0].s, "+"):
		p.cwdUnknown = true // cd -, pushd +1, cd $DIR
	default:
		p.Cwd = p.abs(args[0].s)
	}
}

// isolated выполняет f в копии контекста shell: cd внутри f не меняет
// папку для следующих команд. Так ведут себя ( … ), $( … ), конвейер,
// фоновая команда и дочерний shell (bash -c).
func (p *Policy) isolated(f func()) {
	cwd, unknown := p.Cwd, p.cwdUnknown
	stack := slices.Clone(p.dirStack) // копия: f может дописать в общий массив
	f()
	p.Cwd, p.cwdUnknown, p.dirStack = cwd, unknown, stack
}

func (p *Policy) checkScript(src string, depth int) Verdict {
	if depth > maxDepth {
		return p.soft("too-deep", "bash -c is nested too deeply", "Split the command into several simple steps")
	}
	file, err := parseShell(src)
	if err != nil {
		return p.soft("parse-error", fmt.Sprintf("could not parse the command (%v)", err), "Rewrite the command in plain bash syntax")
	}
	p.scanGitEnv(file)
	return p.checkNode(file, depth)
}

// checkNode обходит дерево команд по порядку выполнения. Части, которые
// shell выполняет в отдельном процессе, проверяются в копии контекста
// (isolated): их cd не влияет на команды после них.
func (p *Policy) checkNode(node syntax.Node, depth int) Verdict {
	v := allowVerdict
	child := func(nodes ...syntax.Node) {
		p.isolated(func() {
			for _, n := range nodes {
				v = worst(v, p.checkNode(n, depth))
			}
		})
	}
	// syntax.Walk обходит всё дерево, включая $(...), <(...), тела
	// функций, циклов и условий. Функция возвращает true — «идти глубже»,
	// false — «это поддерево мы уже проверили сами».
	syntax.Walk(node, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			if n.Background || n.Coprocess {
				inner := *n // копия без флага «&», иначе попадём сюда снова
				inner.Background, inner.Coprocess = false, false
				child(&inner)
				return false
			}
		case *syntax.Subshell:
			child(stmtNodes(n.Stmts)...)
			return false
		case *syntax.CmdSubst:
			child(stmtNodes(n.Stmts)...)
			return false
		case *syntax.ProcSubst:
			child(stmtNodes(n.Stmts)...)
			return false
		case *syntax.FuncDecl:
			// Тело функции проверяем, но выполнится оно только при вызове.
			// Если в нём есть cd, вызов функции делает папку неизвестной.
			child(n.Body)
			if changesDir(n.Body) && n.Name != nil {
				if p.dirFuncs == nil {
					p.dirFuncs = map[string]bool{}
				}
				p.dirFuncs[n.Name.Value] = true
			}
			return false
		case *syntax.CallExpr:
			v = worst(v, p.checkCall(n, depth))
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				v = worst(v, p.checkPipe(n))
				// В bash каждая часть конвейера — отдельный процесс.
				for _, s := range pipeline(n) {
					child(s)
				}
				return false
			}
		case *syntax.Redirect:
			v = worst(v, p.checkRedirect(n))
		}
		return true
	})
	return v
}

func stmtNodes(stmts []*syntax.Stmt) []syntax.Node {
	out := make([]syntax.Node, len(stmts))
	for i, s := range stmts {
		out[i] = s
	}
	return out
}

// changesDir — есть ли в теле функции cd, pushd или popd.
func changesDir(body syntax.Node) bool {
	found := false
	syntax.Walk(body, func(node syntax.Node) bool {
		if c, ok := node.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			switch wordValue(c.Args[0]).s {
			case "cd", "pushd", "popd":
				found = true
			}
		}
		return !found
	})
	return found
}

// pipeline раскладывает конвейер a | b | c на отдельные команды.
func pipeline(b *syntax.BinaryCmd) []*syntax.Stmt {
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
	return stmts
}

// gitEnvVars — переменные окружения, которые меняют репозиторий или
// конфигурацию git: с ними назначение push не определить по файлам.
func isGitEnvVar(name string) bool {
	switch name {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_NAMESPACE":
		return true
	}
	return strings.HasPrefix(name, "GIT_CONFIG")
}

// scanGitEnv отмечает, что в команде задаются переменные GIT_DIR,
// GIT_CONFIG_* и т.п. — как присваивание (X=1 git …, export X=1) или
// через env X=1 git ….
func (p *Policy) scanGitEnv(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name != nil && isGitEnvVar(n.Name.Value) {
				p.gitEnv = true
			}
		case *syntax.Word:
			if len(n.Parts) > 0 {
				if lit, ok := n.Parts[0].(*syntax.Lit); ok {
					if name, _, found := strings.Cut(lit.Value, "="); found && isGitEnvVar(name) {
						p.gitEnv = true
					}
				}
			}
		}
		return !p.gitEnv
	})
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
	rest, viaXargs, _ = stripWrappersDir(args)
	return rest, viaXargs
}

// stripWrappersDir — то же, что stripWrappers, и ещё папка, в которой
// обёртка запустит команду: env -C dir cmd, sudo -D dir cmd (nil — текущая).
func stripWrappersDir(args []word) (rest []word, viaXargs bool, chdir *word) {
	for len(args) > 0 {
		name := filepath.Base(args[0].s)
		switch name {
		case "sudo", "doas":
			withValue := map[string]bool{"-u": true, "-g": true, "-C": true, "-D": true, "--chdir": true}
			args = args[1:]
			for len(args) > 0 && strings.HasPrefix(args[0].s, "-") && args[0].s != "-" {
				flag := args[0].s
				args = args[1:]
				if flag == "--" {
					break // break выходит из for, а не из switch
				}
				if withValue[flag] && len(args) > 0 {
					if flag == "-D" || flag == "--chdir" {
						d := args[0]
						chdir = &d
					}
					args = args[1:]
				}
			}
		case "env":
			var d *word
			args, d = stripEnv(args[1:])
			if d != nil {
				chdir = d
			}
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
			return args, viaXargs, chdir
		}
	}
	return args, viaXargs, chdir
}

// stripEnv пропускает опции и присваивания команды env и возвращает
// саму запускаемую команду: env -i -u NAME -C dir VAR=1 cmd args…
// chdir — папка из -C/--chdir (nil, если её нет).
func stripEnv(args []word) (rest []word, chdir *word) {
	for len(args) > 0 {
		a := args[0].s
		switch {
		case a == "--":
			return args[1:], chdir
		case a == "-C" || a == "--chdir":
			if len(args) < 2 {
				return nil, chdir
			}
			d := args[1]
			chdir = &d
			args = args[2:]
		case strings.HasPrefix(a, "--chdir="):
			chdir = &word{s: strings.TrimPrefix(a, "--chdir="), dynamic: args[0].dynamic}
			args = args[1:]
		case a == "-u" || a == "--unset":
			if len(args) < 2 {
				return nil, chdir
			}
			args = args[2:]
		case a == "-S" || a == "--split-string":
			// env -S "cmd arg…": команда записана одной строкой.
			if len(args) < 2 {
				return nil, chdir
			}
			var split []word
			for _, f := range strings.Fields(args[1].s) {
				split = append(split, word{s: f, dynamic: args[1].dynamic})
			}
			rest, d := stripEnv(append(split, args[2:]...))
			if d != nil {
				chdir = d
			}
			return rest, chdir
		case strings.HasPrefix(a, "-") && a != "-":
			args = args[1:] // -i, --ignore-environment, --unset=NAME, -uNAME …
		case strings.Contains(a, "="):
			args = args[1:] // VAR=value
		default:
			return args, chdir
		}
	}
	return args, chdir
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
	args, viaXargs, chdir := stripWrappersDir(callWords(c))
	if len(args) == 0 {
		return allowVerdict // только присваивания VAR=x
	}
	if chdir == nil {
		return p.checkArgs(args, viaXargs, depth)
	}
	// env -C dir cmd: команда работает в dir, а папка shell не меняется.
	var v Verdict
	p.isolated(func() {
		p.trackCd("cd", []word{{s: "cd"}, *chdir})
		v = p.checkArgs(args, viaXargs, depth)
	})
	return v
}

func (p *Policy) checkArgs(args []word, viaXargs bool, depth int) Verdict {
	name := filepath.Base(args[0].s)
	v := allowVerdict

	// Любое упоминание файла с секретами в аргументах — кроме команд,
	// которые смотрят только на сам файл, а не на его содержимое.
	// Проверяем итоговые пути: cat .en[v] и ссылка на .env — тоже .env.
	if !metadataOnly(name, args[1:]) {
		for _, a := range args[1:] {
			switch {
			case a.dynamic:
			case strings.HasPrefix(a.s, "-"):
				v = worst(v, p.checkSensitive(p.abs(a.s))) // --env-file=.env
			default:
				for _, t := range p.targets(a) {
					v = worst(v, p.checkSensitive(t))
				}
			}
		}
	}

	switch {
	case name == "cd" || name == "pushd" || name == "popd":
		p.trackCd(name, args)
	case p.dirFuncs[name]:
		p.cwdUnknown = true // функция из этой же команды делает cd
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
		v = worst(v, Verdict{Ask, "holdfast-rollback", "a rollback overwrites project files; user confirmation required"})
	case name == "holdfast" && changesHoldfastSetup(args[1:]):
		// holdfast sync после правки CLAUDE.md может снять запреты — как и
		// правку holdfast.toml, это решает пользователь.
		v = worst(v, Verdict{Ask, "holdfast-config", "this command changes Holdfast's protection settings; user confirmation required"})
	case name == "npm" || name == "pnpm" || name == "yarn" || name == "bun":
		if hasArg(args[1:], "publish") {
			v = worst(v, p.soft("npm-publish", name+" publish releases the package irreversibly",
				"Prepare the version and the changelog, and leave publishing to a human or CI"))
		}
	case name == "find":
		v = worst(v, p.checkFind(args[1:]))
	}

	v = worst(v, p.checkMutatingPaths(name, args[1:]))
	v = worst(v, p.checkConfigured(args))
	return v
}

// holdfastArgs пропускает глобальные опции holdfast (-C dir, --dir dir).
func holdfastArgs(args []word) []word {
	for len(args) > 0 && strings.HasPrefix(args[0].s, "-") {
		if (args[0].s == "-C" || args[0].s == "--dir") && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return args
}

// isHoldfastRollback распознаёт `holdfast [-C dir] guard rollback …`.
func isHoldfastRollback(args []word) bool {
	args = holdfastArgs(args)
	return len(args) >= 2 && args[0].s == "guard" && args[1].s == "rollback"
}

// changesHoldfastSetup — команда holdfast, которая пишет holdfast.toml или
// .claude/settings.json: init, sync (кроме --dry-run), guard install и
// guard uninstall (снимает защиту, а с --purge удаляет точки восстановления).
func changesHoldfastSetup(args []word) bool {
	args = holdfastArgs(args)
	switch {
	case len(args) == 0:
		return false
	case args[0].s == "init":
		return true
	case args[0].s == "sync":
		return !hasArg(args, "--dry-run")
	}
	return len(args) >= 2 && args[0].s == "guard" && (args[1].s == "install" || args[1].s == "uninstall")
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
		return metadataGitCommands[parseGit(args, "", false).sub]
	}
	return metadataCommands[name]
}

// gitCall — вызов git после разбора глобальных опций.
type gitCall struct {
	sub        string // подкоманда: push, reset, …
	rest       []word // её аргументы
	dir        string // папка, где выполнится git (cd и -C)
	dirUnknown bool   // папку или репозиторий не определить: cd $X, --git-dir
	config     []word // значения -c key=val; --config-env — dynamic
}

// parseGit пропускает глобальные опции git (-C dir, -c key=val, ...)
// и запоминает те, что влияют на папку и конфигурацию.
func parseGit(args []word, dir string, unknown bool) gitCall {
	g := gitCall{dir: dir, dirUnknown: unknown}
	for len(args) > 0 && strings.HasPrefix(args[0].s, "-") {
		opt, val, hasVal := strings.Cut(args[0].s, "=")
		takesValue := opt == "-C" || opt == "-c" || opt == "--git-dir" || opt == "--work-tree" ||
			opt == "--namespace" || opt == "--config-env"
		if takesValue && !hasVal {
			if len(args) < 2 {
				break
			}
			args = args[1:]
			val = args[0].s
		}
		dynamic := args[0].dynamic
		args = args[1:]
		switch opt {
		case "-C":
			switch {
			case dynamic:
				g.dirUnknown = true
			case filepath.IsAbs(val):
				g.dir, g.dirUnknown = val, false
			default:
				g.dir = filepath.Join(g.dir, val)
			}
		case "-c":
			g.config = append(g.config, word{s: val, dynamic: dynamic})
		case "--config-env":
			// Значение берётся из переменной окружения — заранее неизвестно.
			g.config = append(g.config, word{s: val, dynamic: true})
		case "--git-dir", "--work-tree", "--namespace":
			g.dirUnknown = true
		}
	}
	if len(args) > 0 {
		g.sub, g.rest = args[0].s, args[1:]
	}
	return g
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
			// Дочерний shell — отдельный процесс: его cd не меняет нашу папку.
			p.isolated(func() { v = worst(v, p.checkScript(args[i+1].s, depth+1)) })
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
		"running a script downloaded from the network (curl | sh) is not allowed. Download the file (curl -o), show it to the user and run it only after it has been reviewed"}
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
	downloaded := false
	for _, s := range pipeline(b) {
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
	v := allowVerdict
	for _, abs := range p.targets(w) {
		v = worst(v, p.checkSensitive(abs))
		if r.Op != syntax.RdrIn {
			p.noteGitConfigWrite(abs)
			v = worst(v, p.checkWritePath(abs))
		}
	}
	return v
}

// Команды, аргументы которых — изменяемые пути.
var mutating = map[string]bool{
	"rm": true, "mv": true, "touch": true, "tee": true, "truncate": true, "mkdir": true,
	"rmdir": true, "chmod": true, "chown": true, "ln": true, "unlink": true, "shred": true,
}

// removers удаляют, переносят или меняют права целых папок: для них
// служебный файл внутри папки — тоже цель (mv .claude x, chmod -R .holdfast).
var removers = map[string]bool{
	"rm": true, "mv": true, "rmdir": true, "chmod": true, "chown": true, "shred": true, "unlink": true,
}

func (p *Policy) checkMutatingPaths(name string, args []word) Verdict {
	v := allowVerdict
	var words []word
	var paths []string
	switch {
	case name == "cp" || name == "rsync" || name == "install" || name == "mv" || name == "ln":
		// Куда на самом деле попадут файлы: cp a.txt dir/ → dir/a.txt.
		dests, tooMany := p.copyTargets(name, args)
		paths = dests
		if tooMany {
			v = p.soft("copy-too-many", fmt.Sprintf("%s copies more than %d files, too many to check them all", name, maxScanFiles),
				"Copy in parts, one directory at a time")
		}
		if name == "mv" || name == "ln" {
			// mv удаляет источник; ln создаёт ещё одно имя для файла, и запись
			// через жёсткую ссылку изменит оригинал.
			words = nonFlags(args)
		}
	case mutating[name]:
		words = nonFlags(args)
	case name == "sed" || name == "perl":
		for _, a := range args {
			if strings.HasPrefix(a.s, "-i") || a.s == "--in-place" || (name == "perl" && strings.HasPrefix(a.s, "-") && strings.Contains(a.s, "i")) {
				words = nonFlags(args)
				break
			}
		}
	case name == "dd":
		for _, a := range args {
			if strings.HasPrefix(a.s, "of=") {
				words = append(words, word{s: strings.TrimPrefix(a.s, "of="), dynamic: a.dynamic})
			}
		}
	}
	for _, a := range words {
		if !a.dynamic {
			paths = append(paths, p.targets(a)...)
		}
	}
	for _, abs := range paths {
		p.noteGitConfigWrite(abs)
		v = worst(v, p.checkWritePath(abs))
		if removers[name] && p.containsService(abs) {
			v = worst(v, p.serviceVerdict(abs))
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
		return p.soft("rm-dynamic", "rm -r with paths from xargs: the targets are not known in advance",
			"List the paths explicitly (look at the list without rm first)")
	}

	v := allowVerdict
	var inProject []string // цели внутри проекта
	otherCount := 0        // файлы в других репозиториях
	for _, t := range targets {
		relativeAfterUnknownCd := p.cwdUnknown && !filepath.IsAbs(p.expandHome(t.s))
		if (t.dynamic && !strings.HasPrefix(t.s, "$HOME")) || relativeAfterUnknownCd {
			if recursive {
				v = worst(v, p.soft("rm-dynamic", fmt.Sprintf("rm -r with a computed path (%s): its value is unknown", t.s),
					"Write the path out explicitly, without variables and without cd into a variable"))
			}
			continue
		}
		for _, base := range p.rmBases(t, recursive) {
			if stop := p.checkRmTarget(base, recursive, &v, &inProject, &otherCount); stop.Decision != Allow {
				return stop
			}
		}
	}
	v = worst(v, p.deleteInProject("deleting with rm", inProject, recursive, nil))
	if otherCount > p.cfg.MaxDeletes {
		v = worst(v, p.soft("mass-delete", fmt.Sprintf("deleting more than %d files outside the project", p.cfg.MaxDeletes),
			"The project's restore points do not cover those files, so delete a few files at a time"))
	}
	return v
}

// rmBases — что на самом деле удалит rm для одного аргумента.
// «Всё в папке» (/*, ~/*, dir/*) проверяется как сама папка: rm -rf * в
// корне проекта — это удаление проекта. Шаблон с частью имени (migr*,
// *.png) раскрывается в конкретные пути, как это сделает shell.
func (p *Policy) rmBases(t word, recursive bool) []string {
	var out []string
	glob := hasGlob(t)
	for _, form := range braceForms(t) { // rm -rf {src,dist} — две цели
		abs := p.abs(form)
		i := strings.IndexAny(abs, "*?[")
		if i < 0 || !glob {
			out = append(out, abs)
			continue
		}
		dir := filepath.Dir(abs[:i] + "x")
		comp, _, _ := strings.Cut(strings.TrimPrefix(abs[len(dir):], string(filepath.Separator)), string(filepath.Separator))
		switch m := shellGlob(abs); {
		case comp == "*" && recursive:
			out = append(out, dir)
		case len(m) > 0:
			out = append(out, m...)
		default:
			out = append(out, abs)
		}
	}
	return out
}

// checkRmTarget проверяет одну цель rm. Возвращает запрет, если он
// окончательный; иначе копит результат в v, inProject и otherCount
// (указатели: функция меняет переменные вызывающей стороны).
func (p *Policy) checkRmTarget(base string, recursive bool, v *Verdict, inProject *[]string, otherCount *int) Verdict {
	switch {
	case recursive && (base == "/" || base == filepath.Dir(base)):
		return Verdict{Deny, "rm-root", "rm -r on the filesystem root is not allowed. Delete specific files inside the project"}
	case recursive && (base == p.Home || isWithin(base, p.Home)):
		return Verdict{Deny, "rm-home", "rm -r on the home directory is not allowed. Delete specific files inside the project"}
	case recursive && (base == p.Root || isWithin(base, p.Root)):
		// и сам репозиторий, и любая папка, которая его содержит
		return Verdict{Deny, "rm-repo-root", "rm -r on the whole repository is not allowed. Delete specific files or directories"}
	case !p.cfg.Strict() && !p.inRepo(base) && p.isTempPath(base):
		// временные файлы вне проекта: удаляем без вопросов.
		// Проверка идёт после repo-root: проект сам может лежать в /tmp.
	case recursive && !p.inRepo(base):
		return Verdict{Deny, "rm-outside-repo", fmt.Sprintf("rm -r outside the repository (%s) is not allowed. Work only with the files of the project %s", base, p.Root)}
	case !p.cfg.Strict() && !p.inRepo(base) && p.inOtherRepo(base):
		// Отдельные файлы в другом git-репозитории (агент работает и там):
		// можно, но массовое удаление — нет, снапшот проекта их не защищает.
		*otherCount += p.countTargets(base, false, p.cfg.MaxDeletes+1-*otherCount)
	case !p.inRepo(base):
		*v = worst(*v, p.soft("rm-outside-repo", fmt.Sprintf("deleting a file outside the repository (%s)", base),
			"Delete only project files, individual files in git repositories or temporary files in /tmp"))
	default:
		*inProject = append(*inProject, base)
	}
	return allowVerdict
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
		m := shellGlob(abs)
		if len(m) == 0 {
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
	g := parseGit(args, p.Cwd, p.cwdUnknown)
	g.dir = p.abs(g.dir)
	sub, rest, dir, dirUnknown := g.sub, g.rest, g.dir, g.dirUnknown
	if sub == "" {
		return allowVerdict
	}
	switch sub {
	case "push":
		return p.checkPush(g)
	case "checkout", "switch", "branch":
		p.trackBranch(g)
	case "config", "remote":
		if changesGitConfig(sub, rest) {
			p.gitConfigChanged = true
		}
	case "update-ref":
		for _, w := range rest {
			if strings.HasPrefix(w.s, "refs/holdfast") {
				return Verdict{Deny, "holdfast-snapshots", "restore points are the user's safety net, the agent must not change or delete them. Leave refs/holdfast untouched"}
			}
		}
		if hasArg(rest, "--stdin") {
			// Имена ссылок придут из stdin — что изменится, неизвестно.
			return p.soft("git-update-ref", "git update-ref --stdin: the refs it changes are not known in advance",
				"Use the explicit form: git update-ref <ref> <value>")
		}
	case "reset":
		if hasArg(rest, "--hard") {
			return Verdict{Deny, "git-reset-hard", "git reset --hard irreversibly wipes uncommitted changes. Use git stash, or git restore <file> for specific files"}
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
			return p.soft("git-clean", "git clean -x also deletes ignored files (.env, local settings), and those are not in the restore points",
				"Delete specific files or use git clean -f without -x")
		case force:
			return p.checkGitClean(p.abs(dir), dirUnknown)
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
				v = worst(v, Verdict{Deny, "synced-deny-command", fmt.Sprintf("the command \"%s\" is forbidden by a rule from %s", r.Pattern, r.Source)})
			}
		case "ask-command":
			if MatchCommand(argv, r.Pattern) {
				v = worst(v, Verdict{Ask, "synced-ask-command", fmt.Sprintf("the command \"%s\" needs confirmation (rule from %s)", r.Pattern, r.Source)})
			}
		}
	}
	for _, pat := range p.cfg.DenyCommands {
		if MatchCommand(argv, pat) {
			v = worst(v, Verdict{Deny, "deny-command", fmt.Sprintf("the command \"%s\" is forbidden in holdfast.toml", pat)})
		}
	}
	for _, pat := range p.cfg.AskCommands {
		if MatchCommand(argv, pat) {
			v = worst(v, Verdict{Ask, "ask-command", fmt.Sprintf("the command \"%s\" needs the user's confirmation", pat)})
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
