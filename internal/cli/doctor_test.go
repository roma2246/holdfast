package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roma2246/holdfast/internal/guard"
)

// newGitRepo — git-репозиторий с одним коммитом.
func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git не установлен")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	return dir
}

// fakeBin — исполняемый файл, который будет «бинарником holdfast» для хуков.
func fakeBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "holdfast")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDoctor(t *testing.T) {
	dir := newGitRepo(t)

	// Ничего не установлено: хуков и точек нет — код 2 и подсказки.
	code, out, _ := run(t, "", "-C", dir, "doctor")
	if code != ExitFailure {
		t.Fatalf("код %d:\n%s", code, out)
	}
	for _, want := range []string{"✓ git установлен", "✗ хуки Claude Code", "holdfast guard install",
		"✓ holdfast.toml читается", "✗ точки восстановления", "holdfast guard start", "Проблем:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}

	// Хуки с явным путём к бинарнику и одна точка — всё в порядке.
	bin := fakeBin(t)
	if code, _, errOut := run(t, "", "-C", dir, "guard", "install", "--bin", bin); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := run(t, "", "-C", dir, "guard", "start"); code != 0 {
		t.Fatal(errOut)
	}
	code, out, _ = run(t, "", "-C", dir, "doctor")
	if code != 0 || strings.Contains(out, "✗") || !strings.Contains(out, "Всё в порядке") ||
		!strings.Contains(out, "бинарник для хуков: "+bin) || !strings.Contains(out, "1, последняя") {
		t.Fatalf("код %d:\n%s", code, out)
	}

	// Бинарник пропал.
	os.Remove(bin)
	if code, out, _ := run(t, "", "-C", dir, "doctor"); code != ExitFailure || !strings.Contains(out, "файла "+bin+" нет") {
		t.Fatalf("%d\n%s", code, out)
	}
	if code, _, _ := run(t, "", "-C", dir, "guard", "uninstall"); code != 0 {
		t.Fatal(code)
	}
	if _, _, err := guard.InstallHook(dir, false, fakeBin(t)); err != nil {
		t.Fatal(err)
	}

	// Битый holdfast.toml и свежая config-error в журнале.
	toml := filepath.Join(dir, "holdfast.toml")
	os.WriteFile(toml, []byte("[guard\n"), 0o644)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(toml, past, past)
	guard.AppendLog(dir, guard.LogEntry{Time: time.Now().UTC(), Tool: "Bash", Decision: "ask", Rule: "config-error", Reason: "holdfast.toml не читается"})
	guard.AppendLog(dir, guard.LogEntry{Time: past.Add(-time.Minute).UTC(), Tool: "Bash", Decision: "ask", Rule: "config-error", Reason: "до правки"})
	code, out, _ = run(t, "", "-C", dir, "doctor")
	if code != ExitFailure || !strings.Contains(out, "✗ holdfast.toml читается") ||
		!strings.Contains(out, "✗ в журнале нет свежих ошибок config-error — 1 после последнего изменения") {
		t.Fatalf("%d\n%s", code, out)
	}
	// Конфиг исправлен (файл изменён позже ошибки) — ошибка в журнале уже не свежая.
	os.WriteFile(toml, []byte("[guard]\n"), 0o644)
	future := time.Now().Add(time.Minute)
	os.Chtimes(toml, future, future)
	if code, out, _ := run(t, "", "-C", dir, "doctor"); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}

	// Неудачная точка восстановления.
	guard.AppendLog(dir, guard.LogEntry{Time: time.Now().UTC(), Tool: "UserPromptSubmit", Decision: "snapshot", Rule: "error", Reason: "disk full"})
	os.WriteFile(filepath.Join(dir, guard.StateDir, "checkpoint-error"), []byte("2026-11-15T10:00:00Z disk full\n"), 0o644)
	code, out, _ = run(t, "", "-C", dir, "doctor")
	if code != ExitFailure || !strings.Contains(out, "✗ точки восстановления создаются без ошибок — 2026-11-15T10:00:00Z disk full") {
		t.Fatalf("%d\n%s", code, out)
	}
	os.Remove(filepath.Join(dir, guard.StateDir, "checkpoint-error"))
	if _, out, _ := run(t, "", "-C", dir, "doctor"); !strings.Contains(out, "неудачных точек: 1") {
		t.Fatal(out)
	}
}

func TestDoctorBrokenSettingsAndNoGit(t *testing.T) {
	dir := t.TempDir() // не git-репозиторий
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte("{oops"), 0o644)
	os.MkdirAll(filepath.Join(dir, guard.StateDir, "log.jsonl"), 0o755) // журнал не читается
	code, out, _ := run(t, "", "-C", dir, "doctor")
	for _, want := range []string{"некорректный JSON", "✗ журнал", "папка не в git-репозитории", "git init"} {
		if code != ExitFailure || !strings.Contains(out, want) {
			t.Fatalf("нет %q (код %d):\n%s", want, code, out)
		}
	}

	// git не найден: проверка точек тоже честно не проходит.
	t.Setenv("PATH", t.TempDir())
	_, out, _ = run(t, "", "-C", dir, "doctor")
	for _, want := range []string{"✗ git установлен", "без git точек восстановления нет", "holdfast не найден в PATH", "guard install --bin"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}
}

func TestResolveBin(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin", "dir"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", "holdfast"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", "plain"), []byte("x"), 0o644)
	tests := []struct {
		bin  string
		want string // подстрока результата или ошибки
		ok   bool
	}{
		{"./bin/holdfast", filepath.Join(root, "bin", "holdfast"), true},
		{filepath.Join(root, "bin", "holdfast"), filepath.Join(root, "bin", "holdfast"), true},
		{"./bin/plain", "не исполняемый", false},
		{"./bin/dir", "не исполняемый", false},
		{"./bin/none", "нет", false},
		{"sh", "/sh", true},
		{"holdfast-no-such-binary", "не найден в PATH", false},
	}
	for _, tt := range tests {
		got, err := resolveBin(root, tt.bin)
		msg := got
		if err != nil {
			msg = err.Error()
		}
		if (err == nil) != tt.ok || !strings.Contains(msg, tt.want) {
			t.Errorf("%q = %q, %v; want %q ok=%v", tt.bin, got, err, tt.want, tt.ok)
		}
	}
	if h := binHint("/x/holdfast"); !strings.Contains(h, "--bin /путь") {
		t.Error(h)
	}
}

func TestGuardUninstall(t *testing.T) {
	dir := newGitRepo(t)
	settings := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	os.WriteFile(settings, []byte(`{"model":"opus"}`), 0o644)

	run(t, "", "-C", dir, "guard", "install")
	run(t, "", "-C", dir, "guard", "install", "--local")
	code, out, _ := run(t, "", "-C", dir, "guard", "uninstall")
	if code != 0 || !strings.Contains(out, "удалены из") || !strings.Contains(out, "uninstall --local") ||
		!strings.Contains(out, "--purge") {
		t.Fatalf("%d\n%s", code, out)
	}
	data, _ := os.ReadFile(settings)
	var s map[string]any
	if json.Unmarshal(data, &s) != nil || len(s) != 1 || s["model"] != "opus" {
		t.Fatalf("%s", data)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "uninstall"); !strings.Contains(out, "хуков holdfast нет") {
		t.Fatal(out)
	}

	// --purge: отказ — ничего не удалено.
	run(t, "", "-C", dir, "guard", "start")
	_, out, _ = run(t, "n\n", "-C", dir, "guard", "uninstall", "--local", "--purge")
	if !strings.Contains(out, "Отменено") || !strings.Contains(out, "1 точек") || strings.Contains(out, "остались") {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(dir, guard.StateDir)); err != nil {
		t.Fatal(".holdfast удалён без подтверждения")
	}
	// Подтверждение — удалены журнал и ссылки.
	code, out, _ = run(t, "да\n", "-C", dir, "guard", "uninstall", "--purge")
	if code != 0 || !strings.Contains(out, "Удалено: журнал и 1 ссылок") {
		t.Fatalf("%d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, guard.StateDir)); err == nil {
		t.Fatal(".holdfast не удалён")
	}
	if g, _ := guard.OpenGit(dir); g != nil {
		if refs, _ := g.AllRefs(); len(refs) != 0 {
			t.Fatalf("остались ссылки %v", refs)
		}
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "uninstall", "--purge", "--yes"); !strings.Contains(out, "удалять нечего") {
		t.Fatal(out)
	}

	// Не git-репозиторий: --purge удаляет только .holdfast/.
	plain := t.TempDir()
	run(t, "", "-C", plain, "guard", "install")
	code, out, _ = run(t, "", "-C", plain, "guard", "uninstall", "--purge", "-y")
	if code != 0 || !strings.Contains(out, "журнал и 0 ссылок") {
		t.Fatalf("%d\n%s", code, out)
	}
}
