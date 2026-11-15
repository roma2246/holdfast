package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// branchState is a repository's branch after a checkout/switch inside the command.
type branchState struct {
	name    string // "" means detached HEAD
	unknown bool   // switched, but where to can't be told (git checkout $X)
}

// anyRepo is the p.branches key used when it is unknown in which repository
// the branch changed (git -C $DIR switch main).
const anyRepo = "*"

// branchOf is the current branch of repo, taking switches in this same command
// into account. known=false means the branch can't be told from the command text.
func (p *Policy) branchOf(repo string) (name string, known bool) {
	if b, ok := p.branches[anyRepo]; ok && b.unknown {
		return "", false
	}
	if b, ok := p.branches[repo]; ok {
		return b.name, !b.unknown
	}
	return currentBranch(repo), true
}

func (p *Policy) setBranch(repo string, w word) {
	if p.branches == nil {
		p.branches = map[string]branchState{}
	}
	if w.dynamic {
		p.branches[repo] = branchState{unknown: true}
		return
	}
	p.branches[repo] = branchState{name: strings.TrimPrefix(w.s, "refs/heads/")}
}

// trackBranch remembers a change of the current branch: git switch X,
// git checkout -b X, git branch -m X. The hook sees the repository before the
// command runs, so in `git switch main && git push` the branch comes from the command text.
func (p *Policy) trackBranch(g gitCall) {
	repo := anyRepo
	if !g.dirUnknown {
		if repo = findGitRoot(g.dir); repo == "" {
			return
		}
	}
	unknown := word{dynamic: true}
	args := g.rest
	createFlags := map[string]bool{"-c": true, "-C": true, "--create": true, "--force-create": true, "--orphan": true, "-b": true, "-B": true}

	switch g.sub {
	case "branch":
		if hasArg(args, "-u") || hasArg(args, "--set-upstream-to") || hasArg(args, "--unset-upstream") || hasArg(args, "--track") ||
			hasPrefixArg(args, "--set-upstream-to=") {
			p.gitConfigChanged = true // the upstream changes
		}
		if !hasArg(args, "-m") && !hasArg(args, "-M") && !hasArg(args, "--move") {
			return
		}
		pos := nonFlags(args)
		cur, known := p.branchOf(repo)
		switch {
		case len(pos) == 1:
			p.setBranch(repo, pos[0]) // the current branch was renamed
		case len(pos) == 2 && (!known || pos[0].dynamic || pos[0].s == cur):
			p.setBranch(repo, pos[1])
		}
		return
	case "checkout":
		if hasArg(args, "--") && !hasArg(args, "-b") && !hasArg(args, "-B") && !hasArg(args, "--orphan") {
			return // git checkout [branch] -- files: the branch does not change
		}
	}

	track := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case createFlags[a.s]:
			if i+1 < len(args) {
				p.setBranch(repo, args[i+1])
				if i+2 < len(args) && !strings.HasPrefix(args[i+2].s, "-") {
					p.gitConfigChanged = true // a branch created from origin/x gets origin/x as its upstream
				}
			}
			return
		case strings.HasPrefix(a.s, "--create=") || strings.HasPrefix(a.s, "--orphan="):
			_, name, _ := strings.Cut(a.s, "=")
			p.setBranch(repo, word{s: name, dynamic: a.dynamic})
			return
		case a.s == "--detach" || (a.s == "-d" && g.sub == "switch"):
			p.setBranch(repo, word{})
			return
		case a.s == "-t" || a.s == "--track":
			track = true
			p.gitConfigChanged = true
		case a.s == "-":
			p.setBranch(repo, unknown) // the previous branch
			return
		case strings.HasPrefix(a.s, "-"):
		case a.dynamic:
			p.setBranch(repo, unknown)
			return
		case track:
			// git checkout --track origin/main creates the branch main
			_, name, _ := strings.Cut(a.s, "/")
			p.setBranch(repo, word{s: name})
			return
		case g.sub == "switch" || refExists(repo, "refs/heads/"+a.s):
			// switch accepts only branches (if there is no local one, it creates it from origin/<name>)
			p.setBranch(repo, a)
			return
		case g.sub == "checkout" && fileExists(filepath.Join(g.dir, a.s)):
			return // git checkout of a file
		default:
			// A tag, a commit or a branch that does not exist yet (git creates it
			// from origin/<name>): where we end up is unknown in advance.
			p.setBranch(repo, unknown)
			return
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func hasPrefixArg(args []word, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a.s, prefix) {
			return true
		}
	}
	return false
}

// changesGitConfig reports git config / git remote calls that change settings.
func changesGitConfig(sub string, args []word) bool {
	pos := nonFlags(args)
	if sub == "remote" {
		return len(pos) > 0 && pos[0].s != "show" && pos[0].s != "get-url"
	}
	for _, a := range args {
		switch a.s {
		case "--get", "--get-all", "--get-regexp", "--get-urlmatch", "--list", "-l", "--get-color", "--get-colorbool":
			return false
		}
	}
	if len(pos) > 0 && (pos[0].s == "get" || pos[0].s == "list") {
		return false
	}
	for _, a := range args {
		if strings.HasPrefix(a.s, "--unset") || strings.HasPrefix(a.s, "--add") || strings.HasPrefix(a.s, "--replace") ||
			strings.HasSuffix(a.s, "-section") || a.s == "--edit" || a.s == "-e" {
			return true
		}
	}
	return len(pos) != 1 // git config key is a read, git config key value is a write
}

// pushTarget is where one refspec leads.
type pushTarget struct {
	branch  string // the branch on the server
	tag     bool   // it is a tag
	force   bool   // +src:dst
	unclear string // why the destination can't be determined
}

// refspecTarget parses a refspec of the form [+]src[:dst].
func refspecTarget(spec, cur string, curKnown bool, repo string) pushTarget {
	var t pushTarget
	if strings.HasPrefix(spec, "+") {
		t.force = true
		spec = spec[1:]
	}
	src, dst, colon := strings.Cut(spec, ":")
	if !colon {
		dst = src
	}
	switch {
	case dst == "" && src == "":
		t.unclear = "the refspec \":\" pushes every branch with a matching name"
		return t
	case strings.Contains(dst, "*"):
		t.unclear = fmt.Sprintf("the refspec %q pushes several branches at once", spec)
		return t
	case dst == "HEAD" || dst == "@":
		if !curKnown {
			t.unclear = "the current branch changes within this same command"
			return t
		}
		dst = cur
	}
	t.tag = strings.HasPrefix(dst, "refs/tags/") || (dst != "" && refExists(repo, "refs/tags/"+dst))
	t.branch = strings.TrimPrefix(dst, "refs/heads/")
	return t
}

// mapRefspec is where remote.<name>.push sends the branch src:
// refs/heads/*:refs/heads/team/* and src=feature → team/feature.
func mapRefspec(spec, src, cur string) (string, bool) {
	spec = strings.TrimPrefix(spec, "+")
	from, to, ok := strings.Cut(spec, ":")
	if !ok {
		to = from
	}
	full := func(ref string) string {
		if ref == "HEAD" || ref == "@" {
			ref = cur
		}
		if !strings.HasPrefix(ref, "refs/") {
			ref = "refs/heads/" + ref
		}
		return ref
	}
	from, src = full(from), full(src)
	if prefix, suffix, wild := strings.Cut(from, "*"); wild {
		if !strings.HasPrefix(src, prefix) || !strings.HasSuffix(src, suffix) || len(src) < len(prefix)+len(suffix) {
			return "", false
		}
		middle := src[len(prefix) : len(src)-len(suffix)]
		return strings.Replace(to, "*", middle, 1), true
	}
	if from != src {
		return "", false
	}
	return to, true
}

// checkPush: force pushes, branch deletion and tags are denied in any repository.
// Protected branches (main) are a project setting: it applies only to the
// project's repository, not to every repository on disk the agent has cd'ed
// into. The branch is read from the repository the push actually runs in.
//
// A destination without an explicit branch is taken from the git configuration
// (-c, remote.*.push, push.default, upstream). If it can't be determined, a
// separate denial, git-push-unclear, tells the agent to name the branch explicitly.
func (p *Policy) checkPush(g gitCall) Verdict {
	args := g.rest
	withValue := map[string]bool{"-o": true, "--push-option": true, "--receive-pack": true, "--exec": true}
	var positional []word
	var repoOpt *word
	deleting, tags, all, noFollowTags := false, false, false, false
	for i := 0; i < len(args); i++ {
		s := args[i].s
		short := strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "--")
		switch {
		case s == "--force" || strings.HasPrefix(s, "--force-with-lease") || s == "--force-if-includes" || s == "--mirror" ||
			(short && strings.Contains(s, "f")) || (strings.HasPrefix(s, "+") && len(s) > 1):
			return forcePushVerdict()
		case s == "--delete" || s == "-d":
			deleting = true
		case s == "--tags" || s == "--follow-tags":
			tags = true
		case s == "--no-follow-tags":
			noFollowTags = true
		case s == "--all" || s == "--branches":
			all = true
		case s == "--repo":
			if i+1 < len(args) {
				r := args[i+1]
				repoOpt = &r
				i++
			}
		case strings.HasPrefix(s, "--repo="):
			repoOpt = &word{s: strings.TrimPrefix(s, "--repo="), dynamic: args[i].dynamic}
		case withValue[s]:
			i++
		case strings.HasPrefix(s, "-"):
		default:
			positional = append(positional, args[i])
		}
	}
	if p.cfg.Strict() {
		return Verdict{Ask, "git-push", "git push sends code to the server; user confirmation required"}
	}

	remote := repoOpt
	var refspecs []word
	if len(positional) > 0 {
		remote, refspecs = &positional[0], positional[1:]
	}
	for _, r := range refspecs {
		if strings.HasPrefix(r.s, ":") && r.s != ":" {
			deleting = true
		}
	}
	switch {
	case deleting:
		return Verdict{Deny, "git-push-delete", "deleting branches on the server is irreversible. Tell the user which branch needs to be deleted"}
	case tags:
		return tagPushVerdict("")
	case all:
		return Verdict{Deny, "git-push-all", "pushing all branches at once is not allowed. Push only your working branch: git push origin <branch>"}
	}

	if g.dirUnknown {
		return p.soft("git-push-branch", "could not determine which repository the push comes from (cd into a variable, --git-dir)",
			"Give the path explicitly: git -C <path> push origin <branch>")
	}
	if p.gitEnv {
		return p.unclearPush("the GIT_DIR / GIT_CONFIG_* variables change the repository or the push settings")
	}
	repo := findGitRoot(g.dir)
	if repo == "" {
		return allowVerdict // not a git repository: git will report the error itself
	}
	projectRepo := isWithin(p.Root, repo) || isWithin(repo, p.Root)

	conf := readGitConfig(repo, p.Home)
	unclear := ""
	if conf.addArgs(g.config, p.Home) {
		unclear = "the value of git -c / --config-env is computed at run time"
	}
	cur, curKnown := p.branchOf(repo)

	var remotes []string
	switch {
	case remote == nil:
		remotes = conf.pushRemotes(cur)
	case remote.dynamic:
		unclear = "the remote is given by a variable"
	default:
		remotes = []string{remote.s}
	}
	for _, r := range remotes {
		if conf.isTrue("remote." + r + ".mirror") {
			return Verdict{Deny, "git-push-force", fmt.Sprintf(
				"remote.%s.mirror = true: the push mirrors all branches and overwrites history on the server. Do a regular push to your own branch: git -c remote.%s.mirror=false push %s <branch>", r, r, r)}
		}
	}
	if !noFollowTags && conf.isTrue("push.followtags") {
		return tagPushVerdict("push.followTags is enabled in the git configuration. Add --no-follow-tags: git push --no-follow-tags origin <branch>")
	}

	var targets []pushTarget
	specs := conf.pushSpecs(remotes)
	if len(refspecs) == 0 {
		switch {
		case unclear != "":
		case p.gitConfigChanged:
			unclear = "the command itself changes git settings before the push"
		case len(specs) > 0:
			// remote.<name>.push replaces push.default
			for _, s := range specs {
				targets = append(targets, refspecTarget(s, cur, curKnown, repo))
			}
		default:
			modes := conf["push.default"]
			if len(modes) == 0 {
				modes = []string{"simple"}
			}
			for _, mode := range modes {
				switch strings.ToLower(mode) {
				case "nothing":
				case "matching":
					unclear = "push.default = matching pushes every branch that exists both locally and on the server"
				case "upstream", "tracking":
					for _, m := range conf["branch."+cur+".merge"] {
						targets = append(targets, pushTarget{branch: strings.TrimPrefix(m, "refs/heads/")})
					}
					fallthrough // and check the current branch too, in case no upstream is set
				default: // simple, current
					if !curKnown {
						unclear = "the current branch changes within this same command"
					}
					targets = append(targets, pushTarget{branch: cur})
				}
			}
		}
	}
	for _, r := range refspecs {
		if r.dynamic {
			unclear = "the branch is given by a variable"
			continue
		}
		targets = append(targets, refspecTarget(r.s, cur, curKnown, repo))
		if !strings.Contains(r.s, ":") {
			// git push origin feature with remote.origin.push=feature:main goes to main
			if p.gitConfigChanged {
				unclear = "the command itself changes git settings before the push"
			}
			for _, s := range specs {
				if dst, ok := mapRefspec(s, r.s, cur); ok {
					targets = append(targets, refspecTarget(dst, cur, curKnown, repo))
				}
			}
		}
	}

	for _, t := range targets {
		switch {
		case t.force:
			return forcePushVerdict()
		case t.tag:
			return tagPushVerdict("")
		case t.unclear != "" && unclear == "":
			unclear = t.unclear
		}
	}
	if !projectRepo {
		return allowVerdict // the project's branch rules are not applied to other repositories
	}
	if unclear != "" {
		return p.unclearPush(unclear)
	}
	for _, t := range targets {
		if t.branch == "" {
			return Verdict{Deny, "git-push-branch", "could not determine the branch (detached HEAD?). Name the branch explicitly: git push origin <branch>"}
		}
		for _, protected := range p.cfg.ProtectedBranches {
			if t.branch == protected {
				return Verdict{Deny, "git-push-protected", fmt.Sprintf(
					"pushing to the protected branch %s is not allowed. Create a working branch (git switch -c <name>), push it (git push -u origin <name>) and propose a pull request", t.branch)}
			}
		}
	}
	return allowVerdict
}

func forcePushVerdict() Verdict {
	return Verdict{Deny, "git-push-force", "git push --force rewrites history on the server. Do a regular push; if the histories have diverged, create a new branch and push that"}
}

func tagPushVerdict(reason string) Verdict {
	if reason == "" {
		reason = "Create the tag locally and suggest that the user push it"
	}
	return Verdict{Deny, "git-push-tags", "pushing tags usually triggers a release. " + reason}
}

func (p *Policy) unclearPush(why string) Verdict {
	return p.soft("git-push-unclear", "could not work out exactly which branch the push would go to: "+why,
		"Name the branch explicitly and without variables: git push origin <branch>")
}
