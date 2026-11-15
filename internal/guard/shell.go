package guard

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// maxDepth is how many levels of bash -c "bash -c '...'" we parse.
const maxDepth = 5

// CheckCommand parses a shell command into an AST and checks every simple
// command: chains (&&, ;, |), substitutions $(...), bash -c "...".
// No substring search is used: "echo 'rm -rf /'" is a harmless echo.
//
// A `cd` inside the command is tracked: after `cd ~` the path `projects`
// means ~/projects. That makes Policy unsafe for concurrent CheckCommand calls.
func (p *Policy) CheckCommand(src string) Verdict {
	saved := p.Cwd
	p.touched = nil
	p.dirStack, p.dirFuncs = nil, nil
	p.branches, p.gitConfigChanged, p.gitEnv = nil, false, false
	p.dirCache = nil // the command may create a symlink, so the old cache can't be trusted
	defer func() { p.Cwd, p.cwdUnknown = saved, false }()
	return p.checkScript(src, 0)
}

// dirEntry is a directory on the pushd stack.
type dirEntry struct {
	dir     string
	unknown bool
}

// trackCd updates the current directory after cd, pushd and popd.
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

// isolated runs f in a copy of the shell context: a cd inside f does not
// change the directory for the commands that follow. This is how ( … ),
// $( … ), a pipeline, a background command and a child shell (bash -c) behave.
func (p *Policy) isolated(f func()) {
	cwd, unknown := p.Cwd, p.cwdUnknown
	stack := slices.Clone(p.dirStack) // a copy: f may append into the shared backing array
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

// checkNode walks the command tree in execution order. Parts that the shell
// runs in a separate process are checked in a copy of the context
// (isolated): their cd does not affect the commands after them.
func (p *Policy) checkNode(node syntax.Node, depth int) Verdict {
	v := allowVerdict
	child := func(nodes ...syntax.Node) {
		p.isolated(func() {
			for _, n := range nodes {
				v = worst(v, p.checkNode(n, depth))
			}
		})
	}
	// syntax.Walk visits the whole tree, including $(...), <(...) and the
	// bodies of functions, loops and conditionals. The callback returns true
	// for "go deeper" and false for "we have already checked this subtree".
	syntax.Walk(node, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			if n.Background || n.Coprocess {
				inner := *n // a copy without the "&" flag, otherwise we would land here again
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
			// The function body is checked, but it only runs when called.
			// If it contains a cd, calling the function makes the directory unknown.
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
				// In bash every part of a pipeline is a separate process.
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

// changesDir reports whether a function body contains cd, pushd or popd.
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

// pipeline flattens the pipeline a | b | c into separate commands.
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

// isGitEnvVar reports environment variables that change the repository or
// the git configuration: with them the push destination can't be read from files.
func isGitEnvVar(name string) bool {
	switch name {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_NAMESPACE":
		return true
	}
	return strings.HasPrefix(name, "GIT_CONFIG")
}

// scanGitEnv records that the command sets GIT_DIR, GIT_CONFIG_* and the
// like, either as an assignment (X=1 git …, export X=1) or through
// env X=1 git ….
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

// parseShell parses the command as bash and, if that fails, as zsh: on macOS
// the agent's commands run in zsh, and the agent writes things like ${(P)var}.
// zsh support in mvdan.cc/sh is experimental, so it is only a fallback.
func parseShell(src string) (*syntax.File, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err == nil {
		return file, nil
	}
	if zfile, zerr := syntax.NewParser(syntax.Variant(syntax.LangZsh)).Parse(strings.NewReader(src), ""); zerr == nil {
		return zfile, nil
	}
	return nil, err // report the bash error: it is clearer to the agent
}

// word is a command argument after quote processing.
type word struct {
	s       string // the value; $VAR and $(...) stay as text
	dynamic bool   // contains variables/substitutions, so the value is unknown
	node    *syntax.Word
}

// wordValue assembles a word's value from its parts: 'a'"b"c → abc.
func wordValue(w *syntax.Word) word {
	var b strings.Builder
	dyn := false
	var walk func(parts []syntax.WordPart, quoted bool)
	walk = func(parts []syntax.WordPart, quoted bool) {
		for _, part := range parts {
			// A "type switch": checks the concrete type behind an interface value.
			switch x := part.(type) {
			case *syntax.Lit:
				b.WriteString(unescape(x.Value, quoted))
			case *syntax.SglQuoted:
				b.WriteString(x.Value)
			case *syntax.DblQuoted:
				walk(x.Parts, true)
			case *syntax.ParamExp:
				if x.Param != nil && x.Param.Value == "HOME" && x.Exp == nil && x.Slice == nil && x.Repl == nil {
					b.WriteString("$HOME") // expanded later as the home directory
				} else {
					dyn = true
					name := "VAR"
					if x.Param != nil {
						name = x.Param.Value
					}
					b.WriteString("$" + name)
				}
			default: // $(...), <(...), $((...)), {a,b} and so on
				dyn = true
				b.WriteString("$(...)")
			}
		}
	}
	walk(w.Parts, false)
	return word{s: b.String(), dynamic: dyn, node: w}
}

// unescape removes backslashes: rm\ -rf → "rm -rf", as the shell does.
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

// stripWrappers removes sudo, env, nohup, time, timeout... and returns the
// "real" command. viaXargs is true when the arguments will come from xargs.
func stripWrappers(args []word) (rest []word, viaXargs bool) {
	rest, viaXargs, _ = stripWrappersDir(args)
	return rest, viaXargs
}

// stripWrappersDir is stripWrappers plus the directory in which the wrapper
// runs the command: env -C dir cmd, sudo -D dir cmd (nil means the current one).
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
					break // this break leaves the for loop, not the switch
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
				args = args[1:] // the duration
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

// stripEnv skips the options and assignments of the env command and returns
// the command it runs: env -i -u NAME -C dir VAR=1 cmd args…
// chdir is the directory from -C/--chdir (nil if there is none).
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
			// env -S "cmd arg…": the command is written as a single string.
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
		return allowVerdict // only assignments like VAR=x
	}
	if chdir == nil {
		return p.checkArgs(args, viaXargs, depth)
	}
	// env -C dir cmd: the command runs in dir while the shell's directory stays the same.
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

	// Any mention of a secrets file in the arguments counts, except for
	// commands that only look at the file itself and not at its contents.
	// Resolved paths are checked: cat .en[v] and a symlink to .env are .env too.
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
		p.cwdUnknown = true // a function from this same command does a cd
	case shells[name]:
		v = worst(v, p.checkShellCall(args, depth))
	case name == "eval":
		v = worst(v, p.checkScript(strings.Join(strs(args[1:]), " "), depth+1))
	case name == "rm":
		v = worst(v, p.checkRm(args[1:], viaXargs))
	case name == "git":
		v = worst(v, p.checkGit(args[1:]))
	case name == "holdfast" && isHoldfastRollback(args[1:]):
		// A rollback overwrites project files, including the user's own
		// edits, so a human decides that, not the agent.
		v = worst(v, Verdict{Ask, "holdfast-rollback", "a rollback overwrites project files; user confirmation required"})
	case name == "holdfast" && changesHoldfastSetup(args[1:]):
		// holdfast sync after an edit to CLAUDE.md can lift restrictions; like
		// an edit to holdfast.toml, that is the user's call.
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

// holdfastArgs skips holdfast's global options (-C dir, --dir dir).
func holdfastArgs(args []word) []word {
	for len(args) > 0 && strings.HasPrefix(args[0].s, "-") {
		if (args[0].s == "-C" || args[0].s == "--dir") && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return args
}

// isHoldfastRollback recognizes `holdfast [-C dir] guard rollback …`.
func isHoldfastRollback(args []word) bool {
	args = holdfastArgs(args)
	return len(args) >= 2 && args[0].s == "guard" && args[1].s == "rollback"
}

// changesHoldfastSetup reports a holdfast command that writes holdfast.toml or
// .claude/settings.json: init, sync (except --dry-run), guard install and
// guard uninstall (removes the protection and, with --purge, the restore points).
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

// metadataCommands do not read file contents: whether the file exists, its
// size, permissions, whether git ignores it. They may be applied to .env too.
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

// gitCall is a git invocation after its global options are parsed.
type gitCall struct {
	sub        string // subcommand: push, reset, …
	rest       []word // its arguments
	dir        string // the directory git will run in (cd and -C)
	dirUnknown bool   // the directory or repository can't be determined: cd $X, --git-dir
	config     []word // -c key=val values; --config-env ones are dynamic
}

// parseGit skips git's global options (-C dir, -c key=val, ...) and
// remembers the ones that affect the directory and the configuration.
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
			// The value comes from an environment variable, so it is unknown in advance.
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

// checkShellCall: bash -c "..." is parsed recursively; bash <(curl ...) is denied.
func (p *Policy) checkShellCall(args []word, depth int) Verdict {
	v := allowVerdict
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a.s, "-") && !strings.HasPrefix(a.s, "--") && strings.Contains(a.s, "c") && i+1 < len(args) {
			// A child shell is a separate process: its cd does not change our directory.
			p.isolated(func() { v = worst(v, p.checkScript(args[i+1].s, depth+1)) })
			i++
		}
	}
	// The shell's code comes from the network: bash <(curl ...), sh -c "$(wget ...)".
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

// containsDownloader reports whether the node contains a curl/wget call.
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

// readsProgramFromStdin reports whether the interpreter will execute code from stdin.
// `curl … | sh` and `curl … | python3` do (dangerous); in `curl … | python3 -c '…'`
// or `curl … | node script.js` the download is only data and the code is local.
func readsProgramFromStdin(name string, args []word) bool {
	// Flags that are followed by a value rather than a script name.
	withValue := map[string]bool{"-W": true, "-X": true, "-o": true, "-O": true, "-r": true, "--require": true}
	for i := 0; i < len(args); i++ {
		s := args[i].s
		switch {
		case s == "-":
			return true // an explicit "program from stdin"
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
				return false // sh -c '…': the code is in the argument
			case shells[name] && strings.Contains(flags, "s"):
				return true // bash -s: the code comes from stdin
			case (name == "python" || name == "python3") && (strings.HasPrefix(flags, "c") || strings.HasPrefix(flags, "m")):
				return false // python -c '…' / python -m module
			case !shells[name] && name != "python" && name != "python3" && strings.ContainsAny(flags, "eEp"):
				return false // perl -e / -ne, ruby -e, node -e / -p
			}
			if withValue[s] {
				i++
			}
		default:
			return false // the first positional argument is the script file
		}
	}
	return true // no script given, so the code is read from stdin
}

// checkPipe looks for "download | execute": curl ... | sh, wget -O- | sudo bash.
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

// Commands whose arguments are paths they modify.
var mutating = map[string]bool{
	"rm": true, "mv": true, "touch": true, "tee": true, "truncate": true, "mkdir": true,
	"rmdir": true, "chmod": true, "chown": true, "ln": true, "unlink": true, "shred": true,
}

// removers delete, move or change permissions of whole directories: for them
// a service file inside the directory is a target too (mv .claude x, chmod -R .holdfast).
var removers = map[string]bool{
	"rm": true, "mv": true, "rmdir": true, "chmod": true, "chown": true, "shred": true, "unlink": true,
}

func (p *Policy) checkMutatingPaths(name string, args []word) Verdict {
	v := allowVerdict
	var words []word
	var paths []string
	switch {
	case name == "cp" || name == "rsync" || name == "install" || name == "mv" || name == "ln":
		// Where the files actually end up: cp a.txt dir/ → dir/a.txt.
		dests, tooMany := p.copyTargets(name, args)
		paths = dests
		if tooMany {
			v = p.soft("copy-too-many", fmt.Sprintf("%s copies more than %d files, too many to check them all", name, maxScanFiles),
				"Copy in parts, one directory at a time")
		}
		if name == "mv" || name == "ln" {
			// mv removes the source; ln creates another name for the file, and
			// writing through a hard link changes the original.
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

// checkRm handles rm -rf on the root, the home directory and paths outside the repository.
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
			// Combined short flags: -rf, -fR, -rvf.
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
	var inProject []string // targets inside the project
	otherCount := 0        // files in other repositories
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

// rmBases is what rm will actually delete for one argument.
// "Everything in a directory" (/*, ~/*, dir/*) is checked as the directory
// itself: rm -rf * in the project root deletes the project. A pattern with part
// of a name (migr*, *.png) is expanded into concrete paths, as the shell would do.
func (p *Policy) rmBases(t word, recursive bool) []string {
	var out []string
	glob := hasGlob(t)
	for _, form := range braceForms(t) { // rm -rf {src,dist} is two targets
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

// checkRmTarget checks one rm target. It returns a denial when that is
// final; otherwise it accumulates the result in v, inProject and otherCount
// (pointers: the function modifies the caller's variables).
func (p *Policy) checkRmTarget(base string, recursive bool, v *Verdict, inProject *[]string, otherCount *int) Verdict {
	switch {
	case recursive && (base == "/" || base == filepath.Dir(base)):
		return Verdict{Deny, "rm-root", "rm -r on the filesystem root is not allowed. Delete specific files inside the project"}
	case recursive && (base == p.Home || isWithin(base, p.Home)):
		return Verdict{Deny, "rm-home", "rm -r on the home directory is not allowed. Delete specific files inside the project"}
	case recursive && (base == p.Root || isWithin(base, p.Root)):
		// both the repository itself and any directory that contains it
		return Verdict{Deny, "rm-repo-root", "rm -r on the whole repository is not allowed. Delete specific files or directories"}
	case !p.cfg.Strict() && !p.inRepo(base) && p.isTempPath(base):
		// temporary files outside the project: deleted without asking.
		// This check comes after repo-root: the project itself may live in /tmp.
	case recursive && !p.inRepo(base):
		return Verdict{Deny, "rm-outside-repo", fmt.Sprintf("rm -r outside the repository (%s) is not allowed. Work only with the files of the project %s", base, p.Root)}
	case !p.cfg.Strict() && !p.inRepo(base) && p.inOtherRepo(base):
		// Individual files in another git repository (the agent works there too)
		// are fine, but mass deletion is not: the project's restore points don't cover them.
		*otherCount += p.countTargets(base, false, p.cfg.MaxDeletes+1-*otherCount)
	case !p.inRepo(base):
		*v = worst(*v, p.soft("rm-outside-repo", fmt.Sprintf("deleting a file outside the repository (%s)", base),
			"Delete only project files, individual files in git repositories or temporary files in /tmp"))
	default:
		*inProject = append(*inProject, base)
	}
	return allowVerdict
}

// inOtherRepo reports a path inside some git repository other than the project.
// A repository spanning the whole home directory (~/.git for dotfiles) or higher does not count.
func (p *Policy) inOtherRepo(path string) bool {
	root := findGitRoot(path)
	if root == "" || root == "/" || root == p.Home || isWithin(root, p.Home) {
		return false
	}
	return true
}

// countTargets counts the files rm would delete, but no more than limit, so
// that huge directories are not walked just to answer "more than the limit".
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
				return filepath.SkipAll // stop the walk early
			}
			return nil
		})
		if n == 0 {
			n = 1 // a missing or empty path counts as one
		}
	}
	return n
}

// checkGit: push --force and reset --hard are denied, a plain push asks.
func (p *Policy) checkGit(args []word) Verdict {
	// The directory git will run in: the current one (with cd applied) plus -C from the command.
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
			// The ref names come from stdin, so what will change is unknown.
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

// checkConfigured applies deny_commands / ask_commands and the synced
// command rules from the config.
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

// MatchCommand: the first word of the pattern matches the command name, and
// the remaining words occur in the arguments in the same order.
// "git push --force" matches "git push origin main --force".
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
