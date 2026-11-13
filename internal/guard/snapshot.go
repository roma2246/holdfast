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
	Time   time.Time
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

// CreateSnapshot делает снапшот и сохраняет его в prefix<время>.
func (g *Git) CreateSnapshot(prefix string, now time.Time) (Snapshot, error) {
	tree, err := g.WriteTree()
	if err != nil {
		return Snapshot{}, err
	}
	args := []string{"commit-tree", tree, "-m", "holdfast snapshot " + now.Format(time.RFC3339)}
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
	return Snapshot{Ref: ref, Commit: commit, Time: now}, nil
}

func (g *Git) refExists(ref string) bool {
	_, err := g.run(nil, nil, "show-ref", "--verify", "-q", ref)
	return err == nil
}

// Snapshots возвращает снапшоты, новые первыми.
func (g *Git) Snapshots(prefix string) ([]Snapshot, error) {
	out, err := g.run(nil, nil, "for-each-ref", "--sort=-refname",
		"--format=%(refname) %(objectname) %(committerdate:unix)", prefix)
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		unix, _ := strconv.ParseInt(f[2], 10, 64) // ошибка → время 0, не критично
		snaps = append(snaps, Snapshot{Ref: f[0], Commit: f[1], Time: time.Unix(unix, 0)})
	}
	return snaps, nil
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
