package guard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Пространства имён git-ссылок. Ссылки в refs/holdfast/ не видны как
// ветки и не попадают в `git stash list`, поэтому не мешают пользователю.
const (
	SnapshotRefPrefix = "refs/holdfast/"
	BackupRefPrefix   = "refs/holdfast-backup/"
)

// ErrNoSnapshot — снапшотов ещё нет.
var ErrNoSnapshot = errors.New("снапшот не найден: сначала выполните holdfast guard start")

// Snapshot — сохранённое состояние рабочего дерева.
type Snapshot struct {
	Ref    string // refs/holdfast/20261113-204901-123
	Commit string
	Tree   string // хэш дерева: одинаковый у снимков одинакового состояния
	Time   time.Time
	Meta   SnapshotMeta
}

// Виды снимков.
const (
	KindManual     = "manual"     // holdfast guard start
	KindCheckpoint = "checkpoint" // автоматически перед запросом к агенту
)

// SnapshotMeta хранится в сообщении служебного коммита снимка.
type SnapshotMeta struct {
	Kind    string
	Session string // session_id Claude Code
	Prompt  string // начало запроса пользователя
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

// Name — короткое имя (без префикса).
func (s Snapshot) Name() string {
	return strings.TrimPrefix(strings.TrimPrefix(s.Ref, SnapshotRefPrefix), BackupRefPrefix)
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
	return g.writeTreeFresh()
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

// CreateSnapshot делает снапшот и сохраняет его в prefix<время>.
func (g *Git) CreateSnapshot(prefix string, now time.Time) (Snapshot, error) {
	return g.CreateSnapshotWith(prefix, now, SnapshotMeta{Kind: KindManual})
}

// CreateSnapshotWith — снапшот с метаданными (вид, сессия, запрос).
func (g *Git) CreateSnapshotWith(prefix string, now time.Time, meta SnapshotMeta) (Snapshot, error) {
	tree, err := g.WriteTree()
	if err != nil {
		return Snapshot{}, err
	}
	return g.snapshotFromTree(prefix, now, tree, meta)
}

func (g *Git) snapshotFromTree(prefix string, now time.Time, tree string, meta SnapshotMeta) (Snapshot, error) {
	args := []string{"commit-tree", tree, "-m", meta.message(now)}
	if head, ok := g.head(); ok {
		args = append(args, "-p", head)
	}
	out, err := g.run(nil, nil, args...)
	if err != nil {
		return Snapshot{}, err
	}
	commit := strings.TrimSpace(out)

	base := prefix + now.UTC().Format("20060102-150405-000")
	ref := base
	for i := 2; g.refExists(ref); i++ {
		ref = fmt.Sprintf("%s-%d", base, i)
	}
	if _, err := g.run(nil, nil, "update-ref", ref, commit); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Ref: ref, Commit: commit, Tree: tree, Time: now, Meta: meta}, nil
}

// Checkpoint — автоматическая точка восстановления перед запросом к агенту.
// Если рабочее дерево не изменилось с последнего снимка, новый не создаётся:
// возвращается последний (created=false). Старые снимки сверх keep удаляются.
func (g *Git) Checkpoint(now time.Time, meta SnapshotMeta, keep int) (snap Snapshot, created bool, err error) {
	tree, err := g.WriteTree()
	if err != nil {
		return Snapshot{}, false, err
	}
	snaps, err := g.Snapshots(SnapshotRefPrefix)
	if err != nil {
		return Snapshot{}, false, err
	}
	if len(snaps) > 0 && snaps[0].Tree == tree {
		return snaps[0], false, nil
	}
	snap, err = g.snapshotFromTree(SnapshotRefPrefix, now, tree, meta)
	if err != nil {
		return Snapshot{}, false, err
	}
	return snap, true, g.Prune(keep)
}

// Prune оставляет keep самых свежих снимков (keep <= 0 — не удалять ничего).
func (g *Git) Prune(keep int) error {
	if keep <= 0 {
		return nil
	}
	snaps, err := g.Snapshots(SnapshotRefPrefix)
	if err != nil || len(snaps) <= keep {
		return err
	}
	// update-ref --stdin удаляет все лишние ссылки одной командой git.
	var b strings.Builder
	for _, s := range snaps[keep:] {
		fmt.Fprintf(&b, "delete %s\n", s.Ref)
	}
	_, err = g.run(nil, strings.NewReader(b.String()), "update-ref", "--stdin")
	return err
}

func (g *Git) refExists(ref string) bool {
	_, err := g.run(nil, nil, "show-ref", "--verify", "-q", ref)
	return err == nil
}

// Snapshots возвращает снапшоты, новые первыми.
func (g *Git) Snapshots(prefix string) ([]Snapshot, error) {
	// %00 и %1e в формате for-each-ref — байты NUL и RS: разделители полей
	// и записей, которых не бывает ни в именах ссылок, ни в тексте запроса.
	out, err := g.run(nil, nil, "for-each-ref", "--sort=-refname",
		"--format=%(refname)%00%(objectname)%00%(committerdate:unix)%00%(tree)%00%(contents:body)%1e", prefix)
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.Split(strings.TrimLeft(rec, "\n"), "\x00")
		if len(f) != 5 {
			continue
		}
		unix, _ := strconv.ParseInt(f[2], 10, 64) // ошибка → время 0, не критично
		snaps = append(snaps, Snapshot{Ref: f[0], Commit: f[1], Tree: f[3], Time: time.Unix(unix, 0), Meta: parseMeta(f[4])})
	}
	return snaps, nil
}

// SnapshotAt возвращает n-й снимок с конца (1 — самый свежий).
func (g *Git) SnapshotAt(n int) (Snapshot, error) {
	snaps, err := g.Snapshots(SnapshotRefPrefix)
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

// FindSnapshot ищет снапшот по имени; пустое имя — самый свежий.
func (g *Git) FindSnapshot(name string) (Snapshot, error) {
	snaps, err := g.Snapshots(SnapshotRefPrefix)
	if err != nil {
		return Snapshot{}, err
	}
	for _, s := range snaps {
		if name == "" || s.Name() == name || s.Ref == name {
			return s, nil
		}
	}
	if name != "" {
		return Snapshot{}, fmt.Errorf("снапшот %q не найден", name)
	}
	return Snapshot{}, ErrNoSnapshot
}

// DiffOptions — параметры вывода diff.
type DiffOptions struct {
	Stat       bool
	NameStatus bool
	Color      bool
}

// Diff печатает изменения рабочего дерева с момента снапшота.
func (g *Git) Diff(w io.Writer, snap Snapshot, opt DiffOptions) error {
	cur, err := g.WriteTree()
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
	args = append(args, snap.Commit+"^{tree}", cur)
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

// ChangesSince считает, что изменилось с момента снапшота.
func (g *Git) ChangesSince(snap Snapshot) (Changes, error) {
	cur, err := g.WriteTree()
	if err != nil {
		return Changes{}, err
	}
	return g.changesBetween(snap.Commit+"^{tree}", cur)
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

// Rollback возвращает рабочее дерево к снапшоту: удаляет новые файлы и
// восстанавливает изменённые/удалённые. Перед этим сохраняет текущее
// состояние в refs/holdfast-backup/, чтобы откат можно было отменить.
func (g *Git) Rollback(snap Snapshot, now time.Time) (Changes, Snapshot, error) {
	backup, err := g.CreateSnapshot(BackupRefPrefix, now)
	if err != nil {
		return Changes{}, Snapshot{}, fmt.Errorf("резервная копия перед откатом: %w", err)
	}
	ch, err := g.changesBetween(snap.Commit+"^{tree}", backup.Commit+"^{tree}")
	if err != nil {
		return ch, backup, err
	}

	for _, rel := range ch.Added {
		p := filepath.Join(g.Dir, filepath.FromSlash(rel))
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ch, backup, err
		}
		removeEmptyParents(g.Dir, filepath.Dir(p))
	}

	// checkout-index через временный индекс записывает файлы снапшота в
	// рабочее дерево, не меняя ни HEAD, ни настоящий индекс.
	err = g.withTempIndex(func(env []string) error {
		if _, err := g.run(env, nil, "read-tree", snap.Commit+"^{tree}"); err != nil {
			return err
		}
		_, err := g.run(env, nil, "checkout-index", "-a", "-f")
		return err
	})
	return ch, backup, err
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
