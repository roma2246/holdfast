package guard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Пространства имён git-ссылок. Ссылки в refs/holdfast/ не видны как
// ветки и не попадают в `git stash list`, поэтому не мешают пользователю.
//
// Ссылки общие для всех рабочих деревьев (git worktree) репозитория, поэтому
// снимки каждого дерева лежат в своей подпапке: refs/holdfast/main/… для
// основного и refs/holdfast/wt-<имя>/… для остальных. Иначе откат в одном
// дереве мог бы взять снимок другого.
const (
	SnapshotRefPrefix = "refs/holdfast/"
	BackupRefPrefix   = "refs/holdfast-backup/"
)

// KeepBackups — сколько резервных копий «перед откатом» хранить.
const KeepBackups = 20

// ErrNoSnapshot — снапшотов ещё нет.
var ErrNoSnapshot = errors.New("снапшот не найден: сначала выполните holdfast guard start")

// Snapshot — сохранённое состояние рабочего дерева.
type Snapshot struct {
	Ref    string // refs/holdfast/main/20261113-204901-123456789
	Commit string
	Tree   string // хэш дерева: одинаковый у снимков одинакового состояния
	Time   time.Time
	Meta   SnapshotMeta
}

// Виды снимков.
const (
	KindManual     = "manual"     // holdfast guard start
	KindCheckpoint = "checkpoint" // автоматически перед запросом к агенту
	KindPreDelete  = "pre-delete" // автоматически перед массовым удалением
	KindBackup     = "backup"     // состояние перед откатом
)

// SnapshotMeta хранится в сообщении служебного коммита снимка.
type SnapshotMeta struct {
	Kind     string
	Session  string // session_id Claude Code
	Prompt   string // начало запроса пользователя или описание действия
	Worktree string // корень рабочего дерева, в котором сделан снимок
}

func (m SnapshotMeta) message(now time.Time) string {
	kind := m.Kind
	if kind == "" {
		kind = KindManual
	}
	var b strings.Builder
	fmt.Fprintf(&b, "holdfast %s %s\n\nkind: %s\n", kind, now.Format(time.RFC3339), kind)
	if m.Session != "" {
		fmt.Fprintf(&b, "session: %s\n", m.Session)
	}
	if m.Worktree != "" {
		fmt.Fprintf(&b, "worktree: %s\n", m.Worktree)
	}
	if m.Prompt != "" {
		fmt.Fprintf(&b, "prompt: %s\n", oneLine(m.Prompt, 120))
	}
	return b.String()
}

func parseMeta(body string) SnapshotMeta {
	var m SnapshotMeta
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch k {
		case "kind":
			m.Kind = v
		case "session":
			m.Session = v
		case "prompt":
			m.Prompt = v
		case "worktree":
			m.Worktree = v
		}
	}
	if m.Kind == "" {
		m.Kind = KindManual // снимки старых версий
	}
	return m
}

// oneLine сжимает текст в одну строку не длиннее n символов.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Name — короткое имя снимка: последняя часть ссылки.
func (s Snapshot) Name() string {
	return s.Ref[strings.LastIndex(s.Ref, "/")+1:]
}

// Git — тонкая обёртка над командой git (через os/exec).
type Git struct {
	Dir string // корень рабочего дерева
}

// OpenGit находит корень git-репозитория, содержащего dir.
func OpenGit(dir string) (*Git, error) {
	out, err := (&Git{Dir: dir}).run(nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s не git-репозиторий: снапшоты работают через git", dir)
	}
	return &Git{Dir: strings.TrimSpace(out)}, nil
}

var unsafeRefRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// worktreeID — "main" для основного рабочего дерева и "wt-<имя>" для
// добавленного через `git worktree add` (имя — как его называет сам git).
func (g *Git) worktreeID() string {
	gd := gitDir(g.Dir)
	if gd == "" || filepath.Clean(commonDir(gd)) == filepath.Clean(gd) {
		return "main"
	}
	return "wt-" + unsafeRefRe.ReplaceAllString(filepath.Base(gd), "-")
}

func (g *Git) snapPrefix() string   { return SnapshotRefPrefix + g.worktreeID() + "/" }
func (g *Git) backupPrefix() string { return BackupRefPrefix + g.worktreeID() + "/" }

// run выполняет git с дополнительными переменными окружения и stdin.
func (g *Git) run(env []string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Dir
	cmd.Stdin = stdin
	// Снапшоты — служебные коммиты, автора задаём явно: так они работают
	// даже без настроенного user.name/user.email.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=holdfast", "GIT_AUTHOR_EMAIL=holdfast@localhost",
		"GIT_COMMITTER_NAME=holdfast", "GIT_COMMITTER_EMAIL=holdfast@localhost",
		"LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// withTempIndex вызывает fn с переменной GIT_INDEX_FILE, указывающей на
// временный индекс. Настоящий индекс пользователя (staged-изменения) не трогается.
func (g *Git) withTempIndex(fn func(env []string) error) error {
	f, err := os.CreateTemp("", "holdfast-index-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	os.Remove(name) // git нужен несуществующий файл или валидный индекс
	defer os.Remove(name)
	return fn([]string{"GIT_INDEX_FILE=" + name})
}

func (g *Git) head() (string, bool) {
	out, err := g.run(nil, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// WriteTree сохраняет текущее рабочее дерево (включая неотслеживаемые
// файлы, кроме игнорируемых .gitignore) как git-дерево и возвращает его хэш.
//
// Почему не `git stash create`: он не включает неотслеживаемые файлы.
// Временный индекс + `git add -A` + `git write-tree` делает то же, что
// stash, но с новыми файлами и без изменения stash, веток и индекса.
func (g *Git) WriteTree() (string, error) {
	// Быстрый путь: постоянный служебный индекс. Если он недоступен (нет
	// коммитов, параллельный запуск держит блокировку) — честный медленный.
	if tree, err := g.writeTreeCached(); err == nil {
		return tree, nil
	}
	tree, err := g.writeTreeFresh()
	if err != nil && strings.Contains(err.Error(), "does not have a commit checked out") {
		return "", fmt.Errorf("в проекте есть вложенный git-репозиторий без коммитов — git не может сохранить снимок. "+
			"Сделайте в нём коммит, удалите его или добавьте в .gitignore (%w)", err)
	}
	return tree, err
}

// writeTreeFresh строит дерево с нуля во временном индексе: git заново
// читает и хэширует каждый файл проекта.
func (g *Git) writeTreeFresh() (string, error) {
	var tree string
	err := g.withTempIndex(func(env []string) error {
		if _, ok := g.head(); ok {
			if _, err := g.run(env, nil, "read-tree", "HEAD"); err != nil {
				return err
			}
		}
		if _, err := g.run(env, nil, "add", "-A", "--", "."); err != nil {
			return err
		}
		out, err := g.run(env, nil, "write-tree")
		tree = strings.TrimSpace(out)
		return err
	})
	return tree, err
}

// writeTreeCached даёт тот же результат, но хранит служебный индекс в
// .git/holdfast/index между запусками. В индексе git запоминает размер и
// время изменения каждого файла, поэтому перечитывает только изменившиеся —
// на больших проектах это в разы быстрее. `read-tree -m HEAD` возвращает
// индекс к состоянию HEAD, сохраняя эти отметки у нетронутых файлов.
func (g *Git) writeTreeCached() (string, error) {
	gd := gitDir(g.Dir)
	if _, ok := g.head(); gd == "" || !ok {
		return "", errors.New("нет .git или коммитов")
	}
	dir := filepath.Join(gd, "holdfast")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	for _, args := range [][]string{{"read-tree", "-m", "HEAD"}, {"add", "-A", "--", "."}} {
		if _, err := g.run(env, nil, args...); err != nil {
			return "", err
		}
	}
	out, err := g.run(env, nil, "write-tree")
	return strings.TrimSpace(out), err
}

// CreateSnapshot сохраняет текущее рабочее дерево как снимок.
func (g *Git) CreateSnapshot(now time.Time, meta SnapshotMeta) (Snapshot, error) {
	tree, err := g.WriteTree()
	if err != nil {
		return Snapshot{}, err
	}
	return g.snapshotFromTree(g.snapPrefix(), now, tree, meta)
}

func (g *Git) snapshotFromTree(prefix string, now time.Time, tree string, meta SnapshotMeta) (Snapshot, error) {
	if meta.Kind == "" {
		meta.Kind = KindManual
	}
	meta.Worktree = g.Dir
	args := []string{"commit-tree", tree, "-m", meta.message(now)}
	if head, ok := g.head(); ok {
		args = append(args, "-p", head)
	}
	out, err := g.run(nil, nil, args...)
	if err != nil {
		return Snapshot{}, err
	}
	commit := strings.TrimSpace(out)

	// Имя — время с наносекундами фиксированной ширины: порядок имён совпадает
	// с порядком создания. Пустое «старое значение» у update-ref означает
	// «ссылки ещё нет»: создание атомарно, при совпадении имён берём следующее.
	t := now.UTC()
	for attempt := 0; ; attempt++ {
		ref := prefix + t.Format("20060102-150405") + fmt.Sprintf("-%09d", t.Nanosecond())
		_, err := g.run(nil, nil, "update-ref", ref, commit, "")
		if err == nil {
			return Snapshot{Ref: ref, Commit: commit, Tree: tree, Time: now, Meta: meta}, nil
		}
		if attempt >= 20 || !g.refExists(ref) {
			return Snapshot{}, err
		}
		t = t.Add(time.Nanosecond)
	}
}

// Checkpoint — автоматическая точка восстановления. Если рабочее дерево не
// изменилось с последнего снимка, новый не создаётся: возвращается последний
// (created=false). Старые снимки сверх keep удаляются.
func (g *Git) Checkpoint(now time.Time, meta SnapshotMeta, keep int) (snap Snapshot, created bool, err error) {
	tree, err := g.WriteTree()
	if err != nil {
		return Snapshot{}, false, err
	}
	snaps, err := g.Snapshots()
	if err != nil {
		return Snapshot{}, false, err
	}
	if len(snaps) > 0 && snaps[0].Tree == tree {
		return snaps[0], false, nil
	}
	snap, err = g.snapshotFromTree(g.snapPrefix(), now, tree, meta)
	if err != nil {
		return Snapshot{}, false, err
	}
	return snap, true, g.Prune(keep)
}

// Prune оставляет keep самых свежих снимков этого рабочего дерева
// (keep <= 0 — не удалять ничего).
func (g *Git) Prune(keep int) error {
	snaps, err := g.Snapshots()
	if err != nil {
		return err
	}
	return g.deleteOld(snaps, keep)
}

func (g *Git) deleteOld(snaps []Snapshot, keep int) error {
	if keep <= 0 || len(snaps) <= keep {
		return nil
	}
	// update-ref --stdin удаляет все лишние ссылки одной командой git.
	var b strings.Builder
	for _, s := range snaps[keep:] {
		fmt.Fprintf(&b, "delete %s\n", s.Ref)
	}
	_, err := g.run(nil, strings.NewReader(b.String()), "update-ref", "--stdin")
	return err
}

func (g *Git) refExists(ref string) bool {
	_, err := g.run(nil, nil, "show-ref", "--verify", "-q", ref)
	return err == nil
}

// Snapshots возвращает точки восстановления этого рабочего дерева, новые первыми.
func (g *Git) Snapshots() ([]Snapshot, error) { return g.list(SnapshotRefPrefix) }

// Backups возвращает резервные копии «перед откатом», новые первыми.
func (g *Git) Backups() ([]Snapshot, error) { return g.list(BackupRefPrefix) }

func (g *Git) list(root string) ([]Snapshot, error) {
	// %00 и %1e в формате for-each-ref — байты NUL и RS: разделители полей
	// и записей, которых не бывает ни в именах ссылок, ни в тексте запроса.
	out, err := g.run(nil, nil, "for-each-ref",
		"--format=%(refname)%00%(objectname)%00%(committerdate:unix)%00%(tree)%00%(contents:body)%1e", root)
	if err != nil {
		return nil, err
	}
	wt := g.worktreeID()
	var snaps []Snapshot
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.Split(strings.TrimLeft(rec, "\n"), "\x00")
		if len(f) != 5 {
			continue
		}
		// Свои снимки лежат в <root><wt>/<имя>. Снимки старых версий лежали
		// прямо в <root><имя>: их показываем только в основном дереве.
		rest := strings.TrimPrefix(f[0], root)
		own := strings.HasPrefix(rest, wt+"/") && strings.Count(rest, "/") == 1
		legacy := wt == "main" && !strings.Contains(rest, "/")
		if !own && !legacy {
			continue
		}
		unix, _ := strconv.ParseInt(f[2], 10, 64) // ошибка → время 0, не критично
		snaps = append(snaps, Snapshot{Ref: f[0], Commit: f[1], Tree: f[3], Time: time.Unix(unix, 0), Meta: parseMeta(f[4])})
	}
	sort.SliceStable(snaps, func(i, j int) bool { return nameKey(snaps[i].Name()) > nameKey(snaps[j].Name()) })
	return snaps, nil
}

var legacyNameRe = regexp.MustCompile(`^(\d{8}-\d{6})-000(?:-(\d+))?$`)

// nameKey превращает имя снимка в строку, порядок которой совпадает с
// порядком создания. У имён старого формата (…-000, …-000-2, …-000-12)
// номер дополняется нулями, иначе «-12» оказался бы раньше «-2».
func nameKey(name string) string {
	if m := legacyNameRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return fmt.Sprintf("%s-000000000-%06d", m[1], n)
	}
	return name + "-000000"
}

// SnapshotAt возвращает n-й снимок с конца (1 — самый свежий).
func (g *Git) SnapshotAt(n int) (Snapshot, error) {
	snaps, err := g.Snapshots()
	if err != nil {
		return Snapshot{}, err
	}
	if len(snaps) == 0 {
		return Snapshot{}, ErrNoSnapshot
	}
	if n < 1 || n > len(snaps) {
		return Snapshot{}, fmt.Errorf("точки %d нет: всего %d (см. holdfast guard list)", n, len(snaps))
	}
	return snaps[n-1], nil
}

// FindSnapshot ищет снимок по имени или полной ссылке; пустое имя — самый
// свежий. По имени находятся и резервные копии «перед откатом»
// (refs/holdfast-backup/…) — так откат можно отменить.
func (g *Git) FindSnapshot(name string) (Snapshot, error) {
	snaps, err := g.Snapshots()
	if err != nil {
		return Snapshot{}, err
	}
	if name == "" {
		if len(snaps) == 0 {
			return Snapshot{}, ErrNoSnapshot
		}
		return snaps[0], nil
	}
	backups, err := g.Backups()
	if err != nil {
		return Snapshot{}, err
	}
	for _, s := range append(snaps, backups...) {
		if s.Name() == name || s.Ref == name {
			return s, nil
		}
	}
	return Snapshot{}, fmt.Errorf("снапшот %q не найден в этом рабочем дереве", name)
}

// TreeFiles возвращает множество файлов дерева tree под путями paths
// (относительно корня репозитория; без paths — все файлы).
func (g *Git) TreeFiles(tree string, paths []string) (map[string]bool, error) {
	args := append([]string{"ls-tree", "-r", "-z", "--name-only", tree, "--"}, paths...)
	// GIT_LITERAL_PATHSPECS: пути — буквально, без glob-символов.
	out, err := g.run([]string{"GIT_LITERAL_PATHSPECS=1"}, nil, args...)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, f := range splitZ(out) {
		set[f] = true
	}
	return set, nil
}

// Untracked — неотслеживаемые и не игнорируемые файлы (то, что удалит git clean -f).
func (g *Git) Untracked() ([]string, error) {
	out, err := g.run(nil, nil, "ls-files", "--others", "--exclude-standard", "-z")
	return splitZ(out), err
}

// DiffOptions — параметры вывода diff.
type DiffOptions struct {
	Stat       bool
	NameStatus bool
	Color      bool
}

// Diff печатает изменения рабочего дерева с момента снапшота — ровно то,
// что отменит откат к нему.
func (g *Git) Diff(w io.Writer, snap Snapshot, opt DiffOptions) error {
	plan, err := g.PlanRollback(snap)
	if err != nil {
		return err
	}
	args := []string{"diff", "--no-ext-diff"}
	if opt.Color {
		args = append(args, "--color=always")
	} else {
		args = append(args, "--color=never")
	}
	if opt.Stat {
		args = append(args, "--stat")
	}
	if opt.NameStatus {
		args = append(args, "--name-status")
	}
	args = append(args, snap.Commit+"^{tree}", plan.Tree)
	out, err := g.run(nil, nil, args...)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, out)
	return err
}

// Changes — сводка различий между снапшотом и текущим деревом.
type Changes struct {
	Added    []string // появились после снапшота (будут удалены)
	Modified []string // изменены или удалены (будут восстановлены)
}

// Empty — изменений нет.
func (c Changes) Empty() bool { return len(c.Added)+len(c.Modified) == 0 }

// ChangesSince считает, что изменилось с момента снапшота (что отменит откат).
func (g *Git) ChangesSince(snap Snapshot) (Changes, error) {
	plan, err := g.PlanRollback(snap)
	return plan.Changes, err
}

// ChangesTo — что изменилось между снимком и деревом cur (из WriteTree).
func (g *Git) ChangesTo(snap Snapshot, cur string) (Changes, error) {
	return g.changesBetween(snap.Commit+"^{tree}", cur)
}

func (g *Git) changesBetween(from, to string) (Changes, error) {
	var c Changes
	// -z: имена разделены нулевым байтом — надёжно для путей с пробелами.
	// --no-renames: переименование = удаление + добавление.
	added, err := g.run(nil, nil, "diff", "--no-renames", "--name-only", "-z", "--diff-filter=A", from, to)
	if err != nil {
		return c, err
	}
	other, err := g.run(nil, nil, "diff", "--no-renames", "--name-only", "-z", "--diff-filter=MDT", from, to)
	if err != nil {
		return c, err
	}
	c.Added = splitZ(added)
	c.Modified = splitZ(other)
	return c, nil
}

func splitZ(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// RollbackPlan — что именно сделает откат. Строится до любых изменений.
type RollbackPlan struct {
	Snapshot Snapshot
	// Tree — текущее состояние, включая всё, что откат перезапишет или
	// удалит. Именно оно сохраняется как резервная копия.
	Tree    string
	Changes Changes
	// Hidden — пути, которые обычный снимок не видит (git их сейчас
	// игнорирует), но откат их затронет. Они сохранены в Tree принудительно.
	Hidden []string
	// Skipped — вложенные git-репозитории, которые откат оставит как есть.
	Skipped []string
}

// PlanRollback вычисляет план отката и гарантирует главное свойство: всё,
// что откат перезапишет или удалит, сохранено в plan.Tree. Если сохранить
// не удаётся — возвращает ошибку, и откат не начинается.
//
// Обычный снимок (WriteTree) учитывает .gitignore. Но файл мог попасть в
// .gitignore уже после точки восстановления: тогда он есть в точке, откат
// его перезапишет, а в обычном снимке текущей версии не будет. Такие пути
// добавляются в резервную копию принудительно (git add -f).
func (g *Git) PlanRollback(snap Snapshot) (RollbackPlan, error) {
	plan := RollbackPlan{Snapshot: snap}
	if wt := snap.Meta.Worktree; wt != "" && !samePath(wt, g.Dir) {
		return plan, fmt.Errorf("снимок %s сделан в другом рабочем дереве (%s) — откат отменён", snap.Name(), wt)
	}
	snapTree := snap.Commit + "^{tree}"
	base, err := g.WriteTree()
	if err != nil {
		return plan, err
	}
	// Что есть в точке, но чего нет в обычном снимке текущего состояния.
	out, err := g.run(nil, nil, "diff", "--no-renames", "--name-only", "-z", "--diff-filter=D", snapTree, base)
	if err != nil {
		return plan, err
	}
	hidden := map[string]bool{}
	for _, rel := range splitZ(out) {
		if _, err := os.Lstat(filepath.Join(g.Dir, filepath.FromSlash(rel))); err == nil {
			hidden[rel] = true // на этом месте что-то лежит: файл, ссылка или папка
			continue
		}
		// На месте одной из родительских папок может лежать файл.
		for parent := filepath.ToSlash(filepath.Dir(rel)); parent != "." && parent != "/"; parent = filepath.ToSlash(filepath.Dir(parent)) {
			fi, err := os.Lstat(filepath.Join(g.Dir, filepath.FromSlash(parent)))
			if err != nil {
				continue
			}
			if !fi.IsDir() {
				hidden[parent] = true
			}
			break
		}
	}
	plan.Tree = base
	if len(hidden) > 0 {
		for rel := range hidden {
			plan.Hidden = append(plan.Hidden, rel)
		}
		sort.Strings(plan.Hidden)
		if plan.Tree, err = g.treeWithForced(base, plan.Hidden); err != nil {
			return plan, fmt.Errorf("не удалось сохранить файлы, которые перезапишет откат (%s): %w — откат отменён",
				strings.Join(plan.Hidden, ", "), err)
		}
	}
	if plan.Changes, err = g.changesBetween(snapTree, plan.Tree); err != nil {
		return plan, err
	}
	return plan, g.guardNestedRepos(snapTree, &plan)
}

// guardNestedRepos не даёт откату разрушить вложенные git-репозитории. В
// дереве такой репозиторий — одна запись-ссылка без содержимого, поэтому
// резервная копия его файлы не хранит. Появившийся после точки вложенный
// репозиторий откат оставляет на месте; если же откату нужно записать файл
// на его место или внутрь него — откат отменяется.
func (g *Git) guardNestedRepos(snapTree string, plan *RollbackPlan) error {
	nested, err := g.gitlinks(plan.Tree)
	if err != nil || len(nested) == 0 {
		return err
	}
	inSnap, err := g.gitlinks(snapTree)
	if err != nil {
		return err
	}
	for _, m := range plan.Changes.Modified {
		for repo := range nested {
			if (m == repo && !inSnap[repo]) || strings.HasPrefix(m, repo+"/") {
				return fmt.Errorf("на месте %s сейчас вложенный git-репозиторий (%s): его содержимое нельзя сохранить в резервной копии — откат отменён. "+
					"Перенесите или удалите этот репозиторий и повторите", m, repo)
			}
		}
	}
	keep := func(paths []string) []string {
		var out []string
		for _, p := range paths {
			if nested[p] {
				plan.Skipped = append(plan.Skipped, p)
				continue
			}
			out = append(out, p)
		}
		return out
	}
	plan.Changes.Added = keep(plan.Changes.Added)
	plan.Changes.Modified = keep(plan.Changes.Modified)
	return nil
}

// gitlinks возвращает пути вложенных репозиториев (записи режима 160000) в дереве.
func (g *Git) gitlinks(tree string) (map[string]bool, error) {
	out, err := g.run(nil, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, err
	}
	links := map[string]bool{}
	for _, rec := range splitZ(out) {
		// "<режим> <тип> <хэш>\t<путь>"
		meta, path, ok := strings.Cut(rec, "\t")
		if ok && strings.HasPrefix(meta, "160000 ") {
			links[path] = true
		}
	}
	return links, nil
}

// treeWithForced строит дерево base + принудительно добавленные пути
// (в обход .gitignore) и проверяет, что каждый файл под ними сохранён.
func (g *Git) treeWithForced(base string, paths []string) (string, error) {
	var tree string
	err := g.withTempIndex(func(env []string) error {
		if _, err := g.run(env, nil, "read-tree", base); err != nil {
			return err
		}
		env = append(env, "GIT_LITERAL_PATHSPECS=1")
		stdin := strings.NewReader(strings.Join(paths, "\x00") + "\x00")
		if _, err := g.run(env, stdin, "add", "-f", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
		out, err := g.run(env, nil, "write-tree")
		tree = strings.TrimSpace(out)
		return err
	})
	if err != nil {
		return "", err
	}
	saved, err := g.TreeFiles(tree, paths)
	if err != nil {
		return "", err
	}
	// Проверка: всё, что лежит на диске под этими путями, есть в дереве.
	for _, rel := range paths {
		root := filepath.Join(g.Dir, filepath.FromSlash(rel))
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fmt.Errorf("%s содержит вложенный git-репозиторий", rel)
				}
				return nil
			}
			r, err := filepath.Rel(g.Dir, path)
			if err != nil {
				return err
			}
			if !saved[filepath.ToSlash(r)] {
				return fmt.Errorf("%s не удалось сохранить", filepath.ToSlash(r))
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return tree, nil
}

// samePath сравнивает пути с учётом символических ссылок (/tmp → /private/tmp).
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// Rollback возвращает рабочее дерево к снапшоту: удаляет появившиеся после
// него файлы и восстанавливает изменённые и удалённые. Перед этим текущее
// состояние (включая всё, что будет перезаписано) сохраняется в
// refs/holdfast-backup/ — откат можно отменить откатом к этой копии.
func (g *Git) Rollback(snap Snapshot, now time.Time) (Changes, Snapshot, error) {
	plan, err := g.PlanRollback(snap)
	if err != nil {
		return Changes{}, Snapshot{}, err
	}
	backup, err := g.snapshotFromTree(g.backupPrefix(), now, plan.Tree,
		SnapshotMeta{Kind: KindBackup, Prompt: "перед откатом к " + snap.Name()})
	if err != nil {
		return Changes{}, Snapshot{}, fmt.Errorf("резервная копия перед откатом: %w", err)
	}
	ch := plan.Changes

	for _, rel := range ch.Added {
		p := filepath.Join(g.Dir, filepath.FromSlash(rel))
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ch, backup, err
		}
		removeEmptyParents(g.Dir, filepath.Dir(p))
	}

	// checkout-index через временный индекс записывает файлы снапшота в
	// рабочее дерево, не меняя ни HEAD, ни настоящий индекс. Пишем только
	// изменившиеся пути: остальные файлы не трогаем вовсе.
	if len(ch.Modified) > 0 {
		err = g.withTempIndex(func(env []string) error {
			if _, err := g.run(env, nil, "read-tree", snap.Commit+"^{tree}"); err != nil {
				return err
			}
			stdin := strings.NewReader(strings.Join(ch.Modified, "\x00") + "\x00")
			_, err := g.run(env, stdin, "checkout-index", "-f", "-z", "--stdin")
			return err
		})
		if err != nil {
			return ch, backup, err
		}
	}
	if backups, err := g.Backups(); err == nil {
		_ = g.deleteOld(backups, KeepBackups)
	}
	return ch, backup, nil
}

// removeEmptyParents удаляет опустевшие папки вверх до корня.
func removeEmptyParents(root, dir string) {
	for dir != root && isWithin(root, dir) {
		if err := os.Remove(dir); err != nil { // Remove не удаляет непустую папку
			return
		}
		dir = filepath.Dir(dir)
	}
}

// AllRefs — все ссылки Holdfast во всех рабочих деревьях репозитория:
// точки восстановления и резервные копии перед откатом.
func (g *Git) AllRefs() ([]string, error) {
	out, err := g.run(nil, nil, "for-each-ref", "--format=%(refname)", SnapshotRefPrefix, BackupRefPrefix)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// PurgeRefs удаляет все ссылки Holdfast (holdfast guard uninstall --purge).
// Сами объекты git удалит позже обычная сборка мусора (git gc).
func (g *Git) PurgeRefs() (int, error) {
	refs, err := g.AllRefs()
	if err != nil || len(refs) == 0 {
		return 0, err
	}
	var b strings.Builder
	for _, r := range refs {
		fmt.Fprintf(&b, "delete %s\n", r)
	}
	if _, err := g.run(nil, strings.NewReader(b.String()), "update-ref", "--stdin"); err != nil {
		return 0, err
	}
	return len(refs), nil
}
