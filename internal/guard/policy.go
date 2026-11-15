// Package guard — предохранитель для действий агента: политика, хук
// Claude Code, журнал и снапшоты рабочего дерева.
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

// Decision — решение политики. Порядок важен: чем больше, тем строже,
// поэтому итог по нескольким командам — максимум (см. worst).
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

// Verdict — решение с объяснением.
type Verdict struct {
	Decision Decision
	Rule     string // короткий идентификатор правила, например "rm-outside-repo"
	Reason   string // понятное агенту объяснение
}

var allowVerdict = Verdict{Decision: Allow}

// worst возвращает более строгий из двух вердиктов.
func worst(a, b Verdict) Verdict {
	if b.Decision > a.Decision {
		return b
	}
	return a
}

// Policy применяет правила к командам и файлам.
type Policy struct {
	Root string // абсолютный корень репозитория
	Cwd  string // текущая папка агента (для относительных путей)
	Home string // домашняя папка пользователя
	cfg  *config.GuardConfig

	cwdUnknown bool       // был `cd $VAR` — относительные пути неизвестны
	dirStack   []dirEntry // стек pushd/popd
	dirFuncs   map[string]bool

	// Состояние git, которое меняет сама проверяемая команда: хук видит
	// файлы до её выполнения, поэтому `git switch main && git push` надо
	// учитывать по тексту команды.
	branches         map[string]branchState // репозиторий → ветка после checkout/switch
	gitConfigChanged bool                   // git config / git remote / запись в .git/config
	gitEnv           bool                   // GIT_DIR, GIT_CONFIG_* в окружении команды

	realRoot string            // Root после раскрытия символических ссылок
	dirCache map[string]string // папка → она же без ссылок (на одну проверку)
	services *serviceSet       // служебные файлы Holdfast

	// touched — файлы, которые затрагивает действие (для подсказок агенту).
	touched []string

	// tempDirs — временные папки, где удалять можно без вопросов.
	tempDirs []string

	// Session — session_id Claude Code: по нему ищется снимок текущей задачи.
	Session string
	// Now — текущее время (в тестах подменяется).
	Now func() time.Time

	globMu    sync.Mutex
	globCache map[string]*regexp.Regexp
}

// NewPolicy создаёт политику. cfg может быть nil — тогда только встроенные правила.
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

// FileOp — вид файловой операции.
type FileOp int

const (
	OpRead FileOp = iota
	OpWrite
)

// soft — решение для опасного, но не катастрофического действия.
// В strict-режиме — спросить человека; в autonomous — запретить и сразу
// подсказать агенту, как сделать правильно, чтобы он исправился сам.
func (p *Policy) soft(rule, reason, hint string) Verdict {
	if p.cfg.Strict() {
		return Verdict{Ask, rule, reason + " — нужно подтверждение пользователя"}
	}
	return Verdict{Deny, rule, reason + ". " + hint}
}

// Touched — файлы (абсолютные пути), которые затронуло проверенное действие.
func (p *Policy) Touched() []string { return p.touched }

// CheckFile проверяет чтение или запись файла инструментами Read/Edit/Write.
// Правила применяются и к пути, и к файлу, на который он ссылается.
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

// abs превращает путь в абсолютный относительно Cwd, раскрывая ~ и $HOME.
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

// inRepo — лежит ли абсолютный путь внутри репозитория.
func (p *Policy) inRepo(abs string) bool {
	return isWithin(p.Root, abs)
}

func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rel — путь относительно корня в формате a/b/c (для glob-шаблонов).
func (p *Policy) rel(abs string) string {
	r, err := filepath.Rel(p.Root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(r)
}

// IsSensitive — файл с секретами: .env, *.pem, приватные SSH-ключи.
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
		"доступ к файлу с секретами (%s) запрещён. Имена переменных смотри в .env.example или в коде (os.Getenv / process.env); значения не нужны — используй переменные окружения", filepath.Base(abs))}
}

// isTempPath — путь внутри временной папки (но не она сама). Агенты часто
// работают во временных папках, удалять там можно без вопросов.
func (p *Policy) isTempPath(abs string) bool {
	for _, dir := range p.tempDirs {
		dir = filepath.Clean(dir)
		if abs != dir && isWithin(dir, abs) {
			// Чужой git-репозиторий во временной папке (клон, checkout на CI) —
			// это чья-то работа, а не черновики: послабление на него не действует.
			return findGitRoot(abs) == ""
		}
	}
	return false
}

// checkWritePath — запись/изменение файла: служебные файлы Holdfast,
// синхронизированные запреты и защищённые папки из конфига.
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

// pathRules — правила конфига для файла внутри проекта.
func (p *Policy) pathRules(abs string) Verdict {
	rel := p.rel(abs)
	for _, r := range p.cfg.Synced.Rules {
		if r.Kind == config.KindDenyPath && p.match(r.Pattern, rel) {
			return Verdict{Deny, "synced-deny-path", fmt.Sprintf(
				"правка %s запрещена правилом из %s (%s). Не меняй эти файлы; если правка необходима — опиши её пользователю", rel, r.Source, r.Pattern)}
		}
	}
	for _, r := range p.cfg.Synced.Rules {
		if r.Kind == config.KindAskPath && p.match(r.Pattern, rel) {
			return Verdict{Ask, "synced-ask-path", fmt.Sprintf("правка %s требует подтверждения (правило %s из %s)", rel, r.Pattern, r.Source)}
		}
	}
	for _, pat := range p.cfg.ProtectedPaths {
		if p.match(pat, rel) {
			return Verdict{Ask, "protected-path", fmt.Sprintf("%s в защищённой папке (%s): нужно подтверждение пользователя", rel, pat)}
		}
	}
	return allowVerdict
}

// match сопоставляет путь (относительно корня) с glob-шаблоном.
//   - "**" — любое число папок, "*" — любое имя в пределах одной папки;
//   - шаблон без '*' совпадает с самим путём и со всем, что внутри него;
//   - шаблон без '/' (например, "*.sql") проверяется и по имени файла.
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

// GlobToRegexp переводит glob в регулярное выражение.
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
				b.WriteString("(?:.*/)?") // "**/" — ноль или больше папок
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
	// "migrations/**" совпадает и с самой папкой "migrations".
	s := b.String()
	if strings.HasSuffix(s, "/.*") {
		s = strings.TrimSuffix(s, "/.*") + "(?:/.*)?"
	}
	return s + "$"
}
