package guard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRepo создаёт временный git-репозиторий с одним коммитом.
func gitRepo(t *testing.T, commit bool) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git не установлен")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write(t, dir, "a.txt", "original a\n")
	write(t, dir, "b.txt", "original b\n")
	write(t, dir, ".gitignore", "*.log\n")
	if commit {
		git(t, dir, "add", "-A")
		git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSnapshotRollback(t *testing.T) {
	for _, withCommit := range []bool{true, false} {
		name := "with-commit"
		if !withCommit {
			name = "no-commits"
		}
		t.Run(name, func(t *testing.T) {
			dir := gitRepo(t, withCommit)
			// Состояние до сессии: правка, staged-правка и неотслеживаемый файл.
			write(t, dir, "a.txt", "edited before session\n")
			write(t, dir, "untracked.txt", "untracked before\n")
			write(t, dir, "debug.log", "ignored\n")
			if withCommit {
				write(t, dir, "b.txt", "staged change\n")
				git(t, dir, "add", "b.txt")
			}
			stagedBefore := git(t, dir, "diff", "--cached", "--name-only")
			branchesBefore := git(t, dir, "branch", "-a")

			g, err := OpenGit(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := g.FindSnapshot(""); !errors.Is(err, ErrNoSnapshot) {
				t.Fatalf("ожидалась ErrNoSnapshot, получено %v", err)
			}
			snap, err := g.CreateSnapshot(time.Now(), SnapshotMeta{})
			if err != nil {
				t.Fatal(err)
			}

			// Снапшот не трогает индекс, ветки и stash пользователя.
			if got := git(t, dir, "diff", "--cached", "--name-only"); got != stagedBefore {
				t.Errorf("индекс изменился: %q → %q", stagedBefore, got)
			}
			if got := git(t, dir, "branch", "-a"); got != branchesBefore {
				t.Errorf("ветки изменились")
			}
			if got := git(t, dir, "stash", "list"); got != "" {
				t.Errorf("stash изменился: %q", got)
			}

			// «Сессия агента».
			write(t, dir, "a.txt", "agent rewrote a\n")
			os.Remove(filepath.Join(dir, "untracked.txt"))
			write(t, dir, "new/deep/file.go", "package x\n")
			write(t, dir, "c.txt", "new file\n")
			write(t, dir, "debug.log", "agent log\n")

			ch, err := g.ChangesSince(snap)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(ch.Added, ",") != "c.txt,new/deep/file.go" || strings.Join(ch.Modified, ",") != "a.txt,untracked.txt" {
				t.Fatalf("изменения: %+v", ch)
			}
			var diff bytes.Buffer
			if err := g.Diff(&diff, snap, DiffOptions{}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(diff.String(), "+agent rewrote a") || !strings.Contains(diff.String(), "new/deep/file.go") {
				t.Errorf("diff:\n%s", diff.String())
			}
			diff.Reset()
			if err := g.Diff(&diff, snap, DiffOptions{Stat: true, NameStatus: true, Color: true}); err != nil {
				t.Fatal(err)
			}

			found, err := g.FindSnapshot(snap.Name())
			if err != nil || found.Commit != snap.Commit {
				t.Fatalf("FindSnapshot: %+v %v", found, err)
			}
			if _, err := g.FindSnapshot("nope"); err == nil {
				t.Error("ожидалась ошибка для неизвестного снапшота")
			}

			done, backup, err := g.Rollback(snap, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(done.Added) != 2 || len(done.Modified) != 2 {
				t.Errorf("rollback: %+v", done)
			}
			if got := read(t, dir, "a.txt"); got != "edited before session\n" {
				t.Errorf("a.txt = %q", got)
			}
			if got := read(t, dir, "untracked.txt"); got != "untracked before\n" {
				t.Errorf("untracked.txt = %q", got)
			}
			for _, gone := range []string{"c.txt", "new"} {
				if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
					t.Errorf("%s должен быть удалён", gone)
				}
			}
			// Игнорируемые файлы не трогаем.
			if got := read(t, dir, "debug.log"); got != "agent log\n" {
				t.Errorf("debug.log = %q", got)
			}
			if got := git(t, dir, "diff", "--cached", "--name-only"); got != stagedBefore {
				t.Errorf("индекс изменился после отката")
			}
			if ch, _ := g.ChangesSince(snap); !ch.Empty() {
				t.Errorf("после отката остались изменения: %+v", ch)
			}

			// Резервная копия позволяет отменить откат.
			backups, err := g.Backups()
			if err != nil || len(backups) != 1 || backups[0].Commit != backup.Commit {
				t.Fatalf("backups: %+v %v", backups, err)
			}
		})
	}
}

func TestSnapshotUniqueRefs(t *testing.T) {
	dir := gitRepo(t, true)
	g, err := OpenGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s1, err1 := g.CreateSnapshot(now, SnapshotMeta{})
	s2, err2 := g.CreateSnapshot(now, SnapshotMeta{})
	if err1 != nil || err2 != nil || s1.Ref == s2.Ref {
		t.Fatalf("refs %s %s: %v %v", s1.Ref, s2.Ref, err1, err2)
	}
	snaps, _ := g.Snapshots()
	if len(snaps) != 2 {
		t.Fatalf("снапшотов %d", len(snaps))
	}
}

func TestOpenGitNotRepo(t *testing.T) {
	if _, err := OpenGit(t.TempDir()); err == nil {
		t.Fatal("ожидалась ошибка вне git-репозитория")
	}
}

func TestCheckpointDedupAndPrune(t *testing.T) {
	dir := gitRepo(t, true)
	g, err := OpenGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	meta := SnapshotMeta{Kind: KindCheckpoint, Session: "s1", Prompt: "добавь\nлогирование   в app.py"}

	s1, created, err := g.Checkpoint(now, meta, 3)
	if err != nil || !created {
		t.Fatalf("первая точка: %v %v", created, err)
	}
	// Ничего не изменилось — новая точка не нужна.
	s2, created, err := g.Checkpoint(now.Add(time.Minute), meta, 3)
	if err != nil || created || s2.Ref != s1.Ref {
		t.Fatalf("без изменений не должно быть новой точки: %v %+v", created, s2)
	}
	// Изменения → новые точки; старше трёх — удаляются.
	for i := 0; i < 4; i++ {
		write(t, dir, "a.txt", fmt.Sprintf("v%d\n", i))
		if _, created, err := g.Checkpoint(now.Add(time.Duration(i+2)*time.Minute), meta, 3); err != nil || !created {
			t.Fatalf("точка %d: %v %v", i, created, err)
		}
	}
	snaps, _ := g.Snapshots()
	if len(snaps) != 3 {
		t.Fatalf("должно остаться 3 точки, осталось %d", len(snaps))
	}
	m := snaps[0].Meta
	if m.Kind != KindCheckpoint || m.Session != "s1" || m.Prompt != "добавь логирование в app.py" || snaps[0].Tree == "" {
		t.Fatalf("метаданные: %+v tree=%q", m, snaps[0].Tree)
	}
	if at, err := g.SnapshotAt(2); err != nil || at.Ref != snaps[1].Ref {
		t.Fatalf("SnapshotAt(2): %+v %v", at, err)
	}
	if _, err := g.SnapshotAt(9); err == nil {
		t.Fatal("несуществующая точка")
	}
	// Ручной снимок старого формата читается как manual.
	manual, err := g.CreateSnapshot(now.Add(time.Hour), SnapshotMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := g.FindSnapshot(manual.Name()); got.Meta.Kind != KindManual {
		t.Fatalf("manual: %+v", got.Meta)
	}
	if parseMeta("holdfast snapshot 2026").Kind != KindManual {
		t.Fatal("старый формат")
	}
	if g.Prune(0) != nil {
		t.Fatal("Prune(0) ничего не делает")
	}
}

func TestRunCheckpointHook(t *testing.T) {
	dir := gitRepo(t, true)
	env := testEnv(dir)
	in := `{"session_id":"sess-42","cwd":"` + dir + `","hook_event_name":"UserPromptSubmit","prompt":"почини тесты"}`

	RunCheckpoint(strings.NewReader(in), io.Discard, env)
	write(t, dir, "a.txt", "agent edit\n")
	RunCheckpoint(strings.NewReader(in), io.Discard, env)
	RunCheckpoint(strings.NewReader(in), io.Discard, env) // без изменений

	g, _ := OpenGit(dir)
	snaps, _ := g.Snapshots()
	if len(snaps) != 2 || snaps[0].Meta.Prompt != "почини тесты" {
		t.Fatalf("точки: %+v", snaps)
	}
	if snaps[0].Meta.Session != "sess-42" || !samePath(snaps[0].Meta.Worktree, dir) {
		t.Fatalf("метаданные точки: %+v", snaps[0].Meta)
	}
	entries, _ := ReadLog(dir)
	rules := []string{}
	for _, e := range entries {
		rules = append(rules, e.Rule)
	}
	if strings.Join(rules, ",") != "checkpoint,checkpoint,unchanged" {
		t.Fatalf("журнал: %v", rules)
	}
	// Вне git и с мусором на входе — молча ничего не делает.
	RunCheckpoint(strings.NewReader("not json"), io.Discard, testEnv(t.TempDir()))
}

// Быстрый путь (постоянный индекс) обязан давать то же дерево, что и честный.
func TestWriteTreeCachedMatchesFresh(t *testing.T) {
	dir := gitRepo(t, true)
	g, err := OpenGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	same := func(step string) {
		t.Helper()
		cached, err1 := g.writeTreeCached()
		fresh, err2 := g.writeTreeFresh()
		if err1 != nil || err2 != nil || cached != fresh {
			t.Fatalf("%s: cached=%s (%v) fresh=%s (%v)", step, cached, err1, fresh, err2)
		}
	}
	same("чистое дерево")
	write(t, dir, "a.txt", "изменён\n")
	write(t, dir, "new/untracked.txt", "новый\n")
	write(t, dir, "debug.log", "игнорируется\n")
	same("правка, новый файл, игнорируемый файл")
	os.Remove(filepath.Join(dir, "b.txt"))
	same("удаление отслеживаемого файла")
	// Файл был в прошлых снимках, а потом попал в .gitignore.
	write(t, dir, ".gitignore", "*.log\nnew/\n")
	same("новый файл стал игнорируемым")
	// Новый коммит и принудительно добавленный игнорируемый файл.
	git(t, dir, "add", "-A")
	git(t, dir, "add", "-f", "debug.log")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "second")
	write(t, dir, "debug.log", "изменён после коммита\n")
	same("после коммита, отслеживаемый игнорируемый файл")
	os.Remove(filepath.Join(dir, "new", "untracked.txt"))
	same("удаление игнорируемого неотслеживаемого файла")

	// Без коммитов быстрый путь не используется.
	empty := gitRepo(t, false)
	ge, _ := OpenGit(empty)
	if _, err := ge.writeTreeCached(); err == nil {
		t.Fatal("без коммитов ожидалась ошибка быстрого пути")
	}
	if _, err := ge.WriteTree(); err != nil {
		t.Fatal(err)
	}
}
