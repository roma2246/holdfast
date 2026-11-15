package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// branchState — ветка репозитория после checkout/switch внутри команды.
type branchState struct {
	name    string // "" — detached HEAD
	unknown bool   // переключились, но куда — не понять (git checkout $X)
}

// anyRepo — ключ p.branches, когда неизвестно, в каком репозитории
// сменилась ветка (git -C $DIR switch main).
const anyRepo = "*"

// branchOf — текущая ветка repo с учётом переключений в этой же команде.
// known=false — ветку по тексту команды не определить.
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

// trackBranch запоминает смену текущей ветки: git switch X, git checkout -b X,
// git branch -m X. Хук видит репозиторий до выполнения команды, поэтому в
// `git switch main && git push` ветку берём из текста команды.
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
			p.gitConfigChanged = true // меняется upstream
		}
		if !hasArg(args, "-m") && !hasArg(args, "-M") && !hasArg(args, "--move") {
			return
		}
		pos := nonFlags(args)
		cur, known := p.branchOf(repo)
		switch {
		case len(pos) == 1:
			p.setBranch(repo, pos[0]) // переименовали текущую ветку
		case len(pos) == 2 && (!known || pos[0].dynamic || pos[0].s == cur):
			p.setBranch(repo, pos[1])
		}
		return
	case "checkout":
		if hasArg(args, "--") && !hasArg(args, "-b") && !hasArg(args, "-B") && !hasArg(args, "--orphan") {
			return // git checkout [ветка] -- файлы: ветка не меняется
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
					p.gitConfigChanged = true // ветка от origin/x получает upstream origin/x
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
			p.setBranch(repo, unknown) // предыдущая ветка
			return
		case strings.HasPrefix(a.s, "-"):
		case a.dynamic:
			p.setBranch(repo, unknown)
			return
		case track:
			// git checkout --track origin/main создаёт ветку main
			_, name, _ := strings.Cut(a.s, "/")
			p.setBranch(repo, word{s: name})
			return
		case g.sub == "switch" || refExists(repo, "refs/heads/"+a.s):
			// switch принимает только ветки (нет локальной — создаст из origin/<имя>)
			p.setBranch(repo, a)
			return
		case g.sub == "checkout" && fileExists(filepath.Join(g.dir, a.s)):
			return // git checkout файл
		default:
			// Тег, коммит или ветка, которой ещё нет (git создаст её из
			// origin/<имя>): куда переключимся — заранее неизвестно.
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

// changesGitConfig — git config / git remote, которые меняют настройки.
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
	return len(pos) != 1 // git config key — чтение, git config key value — запись
}

// pushTarget — куда ведёт один refspec.
type pushTarget struct {
	branch  string // ветка на сервере
	tag     bool   // это тег
	force   bool   // +src:dst
	unclear string // почему назначение не определить
}

// refspecTarget разбирает refspec вида [+]src[:dst].
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
		t.unclear = "refspec «:» отправляет все ветки с одинаковыми именами"
		return t
	case strings.Contains(dst, "*"):
		t.unclear = fmt.Sprintf("refspec %q отправляет сразу несколько веток", spec)
		return t
	case dst == "HEAD" || dst == "@":
		if !curKnown {
			t.unclear = "текущая ветка меняется в этой же команде"
			return t
		}
		dst = cur
	}
	t.tag = strings.HasPrefix(dst, "refs/tags/") || (dst != "" && refExists(repo, "refs/tags/"+dst))
	t.branch = strings.TrimPrefix(dst, "refs/heads/")
	return t
}

// mapRefspec — куда remote.<имя>.push отправит ветку src:
// refs/heads/*:refs/heads/team/* и src=feature → team/feature.
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

// checkPush: force-push, удаление веток и теги запрещены в любом репозитории.
// Защищённые ветки (main) — настройка проекта: она действует только для
// репозитория проекта, а не для любого репозитория на диске, где агент
// выполнил cd. Ветку читаем у того репозитория, где реально идёт push.
//
// Назначение без явной ветки берётся из конфигурации git (-c, remote.*.push,
// push.default, upstream). Если его не определить — отдельный запрет
// git-push-unclear с подсказкой указать ветку явно.
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
		return Verdict{Ask, "git-push", "git push отправляет код на сервер — нужно подтверждение пользователя"}
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
		return Verdict{Deny, "git-push-delete", "удаление веток на сервере необратимо. Сообщи пользователю, какую ветку нужно удалить"}
	case tags:
		return tagPushVerdict("")
	case all:
		return Verdict{Deny, "git-push-all", "push всех веток сразу запрещён. Запушь только свою рабочую ветку: git push origin <ветка>"}
	}

	if g.dirUnknown {
		return p.soft("git-push-branch", "не удалось определить, из какого репозитория push (cd на переменную, --git-dir)",
			"Укажи путь явно: git -C <путь> push origin <ветка>")
	}
	if p.gitEnv {
		return p.unclearPush("переменные GIT_DIR / GIT_CONFIG_* меняют репозиторий или настройки push")
	}
	repo := findGitRoot(g.dir)
	if repo == "" {
		return allowVerdict // не git-репозиторий: git сам сообщит об ошибке
	}
	projectRepo := isWithin(p.Root, repo) || isWithin(repo, p.Root)

	conf := readGitConfig(repo, p.Home)
	unclear := ""
	if conf.addArgs(g.config, p.Home) {
		unclear = "значение git -c / --config-env вычисляется при запуске"
	}
	cur, curKnown := p.branchOf(repo)

	var remotes []string
	switch {
	case remote == nil:
		remotes = conf.pushRemotes(cur)
	case remote.dynamic:
		unclear = "remote задан переменной"
	default:
		remotes = []string{remote.s}
	}
	for _, r := range remotes {
		if conf.isTrue("remote." + r + ".mirror") {
			return Verdict{Deny, "git-push-force", fmt.Sprintf(
				"remote.%s.mirror = true: push зеркалирует все ветки и перезаписывает историю на сервере. Сделай обычный push в свою ветку: git -c remote.%s.mirror=false push %s <ветка>", r, r, r)}
		}
	}
	if !noFollowTags && conf.isTrue("push.followtags") {
		return tagPushVerdict("в конфигурации git включён push.followTags. Добавь --no-follow-tags: git push --no-follow-tags origin <ветка>")
	}

	var targets []pushTarget
	specs := conf.pushSpecs(remotes)
	if len(refspecs) == 0 {
		switch {
		case unclear != "":
		case p.gitConfigChanged:
			unclear = "команда сама меняет настройки git перед push"
		case len(specs) > 0:
			// remote.<имя>.push заменяет push.default
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
					unclear = "push.default = matching отправит все ветки, которые есть и локально, и на сервере"
				case "upstream", "tracking":
					for _, m := range conf["branch."+cur+".merge"] {
						targets = append(targets, pushTarget{branch: strings.TrimPrefix(m, "refs/heads/")})
					}
					fallthrough // и проверка текущей ветки, если upstream не задан
				default: // simple, current
					if !curKnown {
						unclear = "текущая ветка меняется в этой же команде"
					}
					targets = append(targets, pushTarget{branch: cur})
				}
			}
		}
	}
	for _, r := range refspecs {
		if r.dynamic {
			unclear = "ветка задана переменной"
			continue
		}
		targets = append(targets, refspecTarget(r.s, cur, curKnown, repo))
		if !strings.Contains(r.s, ":") {
			// git push origin feature с remote.origin.push=feature:main уйдёт в main
			if p.gitConfigChanged {
				unclear = "команда сама меняет настройки git перед push"
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
		return allowVerdict // правила веток проекта к чужим репозиториям не применяем
	}
	if unclear != "" {
		return p.unclearPush(unclear)
	}
	for _, t := range targets {
		if t.branch == "" {
			return Verdict{Deny, "git-push-branch", "не удалось определить ветку (detached HEAD?). Укажи ветку явно: git push origin <ветка>"}
		}
		for _, protected := range p.cfg.ProtectedBranches {
			if t.branch == protected {
				return Verdict{Deny, "git-push-protected", fmt.Sprintf(
					"push в защищённую ветку %s запрещён. Создай рабочую ветку (git switch -c <имя>), запушь её (git push -u origin <имя>) и предложи pull request", t.branch)}
			}
		}
	}
	return allowVerdict
}

func forcePushVerdict() Verdict {
	return Verdict{Deny, "git-push-force", "git push --force переписывает историю на сервере. Сделай обычный push; если история разошлась — создай новую ветку и запушь её"}
}

func tagPushVerdict(reason string) Verdict {
	if reason == "" {
		reason = "Создай тег локально и предложи пользователю его запушить"
	}
	return Verdict{Deny, "git-push-tags", "push тегов обычно запускает релиз. " + reason}
}

func (p *Policy) unclearPush(why string) Verdict {
	return p.soft("git-push-unclear", "не удалось точно определить, в какую ветку уйдёт push: "+why,
		"Укажи ветку явно и без переменных: git push origin <ветка>")
}
