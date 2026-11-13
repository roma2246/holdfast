package guard

import (
	"bytes"
	"errors"
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
			snap, err := g.CreateSnapshot(SnapshotRefPrefix, time.Now())
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
			backups, err := g.Snapshots(BackupRefPrefix)
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
	s1, err1 := g.CreateSnapshot(SnapshotRefPrefix, now)
	s2, err2 := g.CreateSnapshot(SnapshotRefPrefix, now)
	if err1 != nil || err2 != nil || s1.Ref == s2.Ref {
		t.Fatalf("refs %s %s: %v %v", s1.Ref, s2.Ref, err1, err2)
	}
	snaps, _ := g.Snapshots(SnapshotRefPrefix)
	if len(snaps) != 2 {
		t.Fatalf("снапшотов %d", len(snaps))
	}
}

func TestOpenGitNotRepo(t *testing.T) {
	if _, err := OpenGit(t.TempDir()); err == nil {
		t.Fatal("ожидалась ошибка вне git-репозитория")
	}
}
