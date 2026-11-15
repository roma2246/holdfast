// Package guard is a safety catch for agent actions: the policy, the
// Claude Code hook, the log and working-tree snapshots.
package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/roma2246/holdfast/internal/config"
)

// Decision is a policy decision. The order matters: the larger, the stricter,
// so the result for several commands is the maximum (see worst).
type Decision int

const (
	Allow Decision = iota
	Ask
	Deny
)

func (d Decision) String() string {
	switch d {
	case Deny:
		return "deny"
	case Ask:
		return "ask"
	default:
		return "allow"
	}
}

// Verdict is a decision with an explanation.
type Verdict struct {
	Decision Decision
	Rule     string // short rule identifier, for example "rm-outside-repo"
	Reason   string // an explanation the agent can understand
}

var allowVerdict = Verdict{Decision: Allow}

// worst returns the stricter of two verdicts.
func worst(a, b Verdict) Verdict {
	if b.Decision > a.Decision {
		return b
	}
	return a
}

// Policy applies the rules to commands and files.
type Policy struct {
	Root string // absolute repository root
	Cwd  string // the agent's current directory (for relative paths)
	Home string // the user's home directory
	cfg  *config.GuardConfig

	cwdUnknown bool       // there was a `cd $VAR`, so relative paths are unknown
	dirStack   []dirEntry // pushd/popd stack
	dirFuncs   map[string]bool

	// Git state changed by the command being checked: the hook sees the
	// files before the command runs, so `git switch main && git push` has
	// to be worked out from the command text.
	branches         map[string]branchState // repository → branch after checkout/switch
	gitConfigChanged bool                   // git config / git remote / a write to .git/config
	gitEnv           bool                   // GIT_DIR, GIT_CONFIG_* in the command's environment

	realRoot string            // Root with symlinks resolved
	dirCache map[string]string // directory → the same directory without symlinks (for one check)
	services *serviceSet       // Holdfast's own service files

	// touched is the files the action affects (for hints to the agent).
	touched []string

	// tempDirs is the temporary directories where deleting needs no questions.
	tempDirs []string

	// Session is the Claude Code session_id: the current task's snapshot is found by it.
	Session string
	// Now is the current time (replaced in tests).
	Now func() time.Time

	globMu    sync.Mutex
	globCache map[string]*regexp.Regexp
}

// NewPolicy creates a policy. cfg may be nil, which leaves only the built-in rules.
func NewPolicy(root, cwd string, cfg *config.GuardConfig) *Policy {
	if cfg == nil {
		cfg = &config.GuardConfig{}
	}
	if err := cfg.Normalize(); err != nil {
		cfg.Mode = config.ModeAutonomous
	}
	if cwd == "" {
		cwd = root
	}
	home, _ := os.UserHomeDir()
	return &Policy{
		Root: filepath.Clean(root), Cwd: filepath.Clean(cwd), Home: filepath.Clean(home),
		cfg: cfg, globCache: map[string]*regexp.Regexp{},
		tempDirs: []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"},
		Now:      time.Now,
	}
}

// FileOp is a kind of file operation.
type FileOp int

const (
	OpRead FileOp = iota
	OpWrite
)

// soft is the decision for a dangerous but not catastrophic action.
// In strict mode it asks the human; in autonomous mode it denies and tells
// the agent right away how to do it properly, so the agent corrects itself.
func (p *Policy) soft(rule, reason, hint string) Verdict {
	if p.cfg.Strict() {
		return Verdict{Ask, rule, reason + "; user confirmation required"}
	}
	return Verdict{Deny, rule, reason + ". " + hint}
}

// Touched returns the files (absolute paths) the checked action affected.
func (p *Policy) Touched() []string { return p.touched }

// CheckFile checks a file read or write made by the Read/Edit/Write tools.
// The rules apply both to the path and to the file it points to.
func (p *Policy) CheckFile(op FileOp, path string) Verdict {
	abs := p.abs(path)
	p.touched = []string{abs}
	v := allowVerdict
	for _, t := range []string{abs, p.realPath(abs)} {
		v = worst(v, p.checkSensitive(t))
		if op == OpWrite {
			v = worst(v, p.checkWritePath(t))
		}
	}
	return v
}

// abs makes the path absolute relative to Cwd, expanding ~ and $HOME.
func (p *Policy) abs(path string) string {
	path = p.expandHome(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.Cwd, path)
	}
	return filepath.Clean(path)
}

func (p *Policy) expandHome(path string) string {
	for _, prefix := range []string{"~", "$HOME", "${HOME}"} {
		if path == prefix {
			return p.Home
		}
		if strings.HasPrefix(path, prefix+"/") {
			return filepath.Join(p.Home, path[len(prefix)+1:])
		}
	}
	return path
}

// inRepo reports whether an absolute path lies inside the repository.
func (p *Policy) inRepo(abs string) bool {
	return isWithin(p.Root, abs)
}

func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rel is the path relative to the root in a/b/c form (for glob patterns).
func (p *Policy) rel(abs string) string {
	r, err := filepath.Rel(p.Root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(r)
}

// IsSensitive reports a secrets file: .env, *.pem, private SSH keys.
func IsSensitive(path string) bool {
	base := filepath.Base(path)
	switch base {
	case ".env", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519":
		return true
	case ".env.example", ".env.sample", ".env.template", ".env.dist":
		return false
	}
	return strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem")
}

func (p *Policy) checkSensitive(abs string) Verdict {
	if !IsSensitive(abs) {
		return allowVerdict
	}
	return Verdict{Deny, "secret-file", fmt.Sprintf(
		"access to the secrets file (%s) is not allowed. Look up variable names in .env.example or in the code (os.Getenv / process.env); you don't need the values, use environment variables", filepath.Base(abs))}
}

// isTempPath reports a path inside a temporary directory (but not the
// directory itself). Agents often work in temp directories, and deleting there needs no questions.
func (p *Policy) isTempPath(abs string) bool {
	for _, dir := range p.tempDirs {
		dir = filepath.Clean(dir)
		if abs != dir && isWithin(dir, abs) {
			// Someone's git repository in a temp directory (a clone, a CI checkout)
			// is real work, not scratch files: the exemption does not apply to it.
			return findGitRoot(abs) == ""
		}
	}
	return false
}

// checkWritePath handles writing/changing a file: Holdfast's service files,
// synced restrictions and protected paths from the config.
func (p *Policy) checkWritePath(abs string) Verdict {
	if p.isService(abs) {
		return p.serviceVerdict(abs)
	}
	if !p.inRepo(abs) {
		return allowVerdict
	}
	if len(p.touched) == 0 || p.touched[len(p.touched)-1] != abs {
		p.touched = append(p.touched, abs)
	}
	return p.pathRules(abs)
}

// pathRules is the config rules for a file inside the project.
func (p *Policy) pathRules(abs string) Verdict {
	rel := p.rel(abs)
	for _, r := range p.cfg.Synced.Rules {
		if r.Kind == config.KindDenyPath && p.match(r.Pattern, rel) {
			return Verdict{Deny, "synced-deny-path", fmt.Sprintf(
				"editing %s is forbidden by a rule from %s (%s). Don't change these files; if the edit is necessary, describe it to the user", rel, r.Source, r.Pattern)}
		}
	}
	for _, r := range p.cfg.Synced.Rules {
		if r.Kind == config.KindAskPath && p.match(r.Pattern, rel) {
			return Verdict{Ask, "synced-ask-path", fmt.Sprintf("editing %s needs confirmation (rule %s from %s)", rel, r.Pattern, r.Source)}
		}
	}
	for _, pat := range p.cfg.ProtectedPaths {
		if p.match(pat, rel) {
			return Verdict{Ask, "protected-path", fmt.Sprintf("%s is in a protected path (%s): user confirmation required", rel, pat)}
		}
	}
	return allowVerdict
}

// match matches a path (relative to the root) against a glob pattern.
//   - "**" is any number of directories, "*" is any name within one directory;
//   - a pattern without '*' matches the path itself and everything inside it;
//   - a pattern without '/' (for example "*.sql") is also tried against the file name.
func (p *Policy) match(pattern, rel string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	if pattern == "" {
		return false
	}
	if !strings.ContainsAny(pattern, "*?[") {
		pattern = strings.TrimSuffix(pattern, "/")
		return rel == pattern || strings.HasPrefix(rel, pattern+"/")
	}
	re := p.globRegexp(pattern)
	if re.MatchString(rel) {
		return true
	}
	if !strings.Contains(pattern, "/") {
		return re.MatchString(filepath.Base(rel))
	}
	return false
}

func (p *Policy) globRegexp(pattern string) *regexp.Regexp {
	p.globMu.Lock()
	defer p.globMu.Unlock()
	if re, ok := p.globCache[pattern]; ok {
		return re
	}
	re := regexp.MustCompile(GlobToRegexp(pattern))
	p.globCache[pattern] = re
	return re
}

// GlobToRegexp converts a glob into a regular expression.
func GlobToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			i++
			if i+1 < len(pattern) && pattern[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?") // "**/" is zero or more directories
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	// "migrations/**" also matches the "migrations" directory itself.
	s := b.String()
	if strings.HasSuffix(s, "/.*") {
		s = strings.TrimSuffix(s, "/.*") + "(?:/.*)?"
	}
	return s + "$"
}
