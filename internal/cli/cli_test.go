package cli

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roma2246/holdfast/internal/config"
)

const fixtures = "../../testdata/lint"

// run запускает CLI как из терминала и возвращает код и вывод.
func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Execute("test", args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

// copyDir копирует фикстуру во временную папку (тесты не портят testdata).
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestHelpAndVersion(t *testing.T) {
	code, out, _ := run(t, "", "--help")
	if code != 0 || !strings.Contains(out, "lint") || !strings.Contains(out, "guard") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out, _ := run(t, "", "--version"); code != 0 || !strings.Contains(out, "test") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _, errOut := run(t, "", "nope"); code != ExitFailure || errOut == "" {
		t.Fatalf("неизвестная команда: %d %q", code, errOut)
	}
}

func TestLintExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"ошибки → 1", []string{"lint", filepath.Join(fixtures, "HF002", "bad")}, ExitFindings},
		{"чисто → 0", []string{"lint", filepath.Join(fixtures, "HF008", "good")}, ExitOK},
		{"warning → 0", []string{"lint", filepath.Join(fixtures, "HF005", "bad")}, ExitOK},
		{"warning + --strict → 1", []string{"lint", "--strict", filepath.Join(fixtures, "HF005", "bad")}, ExitFindings},
		{"один файл", []string{"lint", filepath.Join(fixtures, "HF006", "bad", "CLAUDE.md")}, ExitFindings},
		{"нет пути → 2", []string{"lint", "/nonexistent/path"}, ExitFailure},
		{"плохой формат → 2", []string{"lint", "--format", "xml", fixtures}, ExitFailure},
		{"через -C", []string{"-C", filepath.Join(fixtures, "HF002", "bad"), "lint"}, ExitFindings},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, out, errOut := run(t, "", tt.args...); code != tt.want {
				t.Fatalf("код %d, ожидался %d\n%s%s", code, tt.want, out, errOut)
			}
		})
	}
}

func TestLintHumanOutput(t *testing.T) {
	_, out, _ := run(t, "", "lint", filepath.Join(fixtures, "HF002", "bad"))
	// Формат строки из ТЗ: CLAUDE.md:14  HF002 error  «npm run e2e» — ...
	if !strings.Contains(out, "CLAUDE.md:5  HF002 error    «npm run e2e» — скрипта e2e нет в package.json") {
		t.Fatalf("вывод:\n%s", out)
	}
	if !strings.Contains(out, "ошибок: 3") {
		t.Fatalf("нет сводки:\n%s", out)
	}
	_, out, _ = run(t, "", "lint", filepath.Join(fixtures, "HF008", "good"))
	if !strings.Contains(out, "проблем не найдено") {
		t.Fatal(out)
	}
	_, out, _ = run(t, "", "lint", t.TempDir())
	if !strings.Contains(out, "не найдены") {
		t.Fatal(out)
	}
	_, out, _ = run(t, "", "lint", "--list-rules")
	if strings.Count(out, "HF0") != 9 {
		t.Fatal(out)
	}
}

func TestLintJSON(t *testing.T) {
	code, out, _ := run(t, "", "lint", "--format", "json", filepath.Join(fixtures, "all"))
	if code != ExitFindings {
		t.Fatalf("код %d", code)
	}
	var report struct {
		Findings []struct {
			Rule     string `json:"rule"`
			Severity string `json:"severity"`
			File     string `json:"file"`
			Line     int    `json:"line"`
		} `json:"findings"`
		Summary struct{ Files, Errors, Warnings, Infos int } `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if report.Summary.Files != 1 || report.Summary.Errors == 0 || report.Findings[0].Severity == "" {
		t.Fatalf("%+v", report)
	}
	_, out, _ = run(t, "", "lint", "-f", "json", t.TempDir())
	if !strings.Contains(out, `"findings": []`) {
		t.Fatal(out)
	}
}

func TestInitAndSync(t *testing.T) {
	dir := copyDir(t, filepath.Join(fixtures, "HF009", "bad"))
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(`# P

<!-- holdfast
deny-paths: secrets/**
-->

- Never edit `+"`migrations/`"+` by hand.
- Never run `+"`docker compose down`"+`.
`), 0o644)

	if code, out, _ := run(t, "", "-C", dir, "init"); code != 0 || !strings.Contains(out, "holdfast.toml") {
		t.Fatalf("init: %d %s", code, out)
	}
	if code, _, _ := run(t, "", "-C", dir, "init"); code != ExitFailure {
		t.Fatal("повторный init должен падать")
	}

	// До sync lint предупреждает HF009 (с --strict — код 1).
	if code, out, _ := run(t, "", "lint", "--strict", dir); code != ExitFindings || !strings.Contains(out, "HF009") {
		t.Fatalf("lint до sync: %d\n%s", code, out)
	}

	// --dry-run ничего не пишет.
	run(t, "", "-C", dir, "sync", "--dry-run")
	cfg, _ := config.Load(dir)
	if len(cfg.Guard.Synced.Rules) != 0 {
		t.Fatal("dry-run изменил конфиг")
	}

	// Подтверждаем первое эвристическое правило и отклоняем второе.
	code, out, _ := run(t, "y\nn\n", "-C", dir, "sync")
	if code != 0 || !strings.Contains(out, "Добавить в политику?") {
		t.Fatalf("sync: %d\n%s", code, out)
	}
	cfg, _ = config.Load(dir)
	got := []string{}
	for _, r := range cfg.Guard.Synced.Rules {
		got = append(got, r.Kind+" "+r.Pattern+" "+r.Source)
	}
	want := "deny-path secrets/** CLAUDE.md:4|deny-path migrations/** CLAUDE.md:7"
	if strings.Join(got, "|") != want {
		t.Fatalf("synced:\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}

	// Повторный sync помнит подтверждённое правило; --yes принимает остальное.
	if code, _, _ := run(t, "", "-C", dir, "sync", "--yes"); code != 0 {
		t.Fatal(code)
	}
	cfg, _ = config.Load(dir)
	if len(cfg.Guard.Synced.Rules) != 3 {
		t.Fatalf("%+v", cfg.Guard.Synced.Rules)
	}
	// После sync предупреждений HF009 нет.
	if _, out, _ := run(t, "", "lint", dir); strings.Contains(out, "HF009") {
		t.Fatalf("HF009 после sync:\n%s", out)
	}
	// --no-heuristics оставляет только явный блок.
	run(t, "", "-C", dir, "sync", "--no-heuristics")
	cfg, _ = config.Load(dir)
	if len(cfg.Guard.Synced.Rules) != 1 {
		t.Fatalf("%+v", cfg.Guard.Synced.Rules)
	}

	empty := t.TempDir()
	if _, out, _ := run(t, "", "-C", empty, "sync"); !strings.Contains(out, "не найдено") {
		t.Fatal(out)
	}
}

func TestGuardInstallCheckLog(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := run(t, "", "-C", dir, "guard", "install")
	if code != 0 || !strings.Contains(out, "Хук добавлен") {
		t.Fatalf("%d %s", code, out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "install"); !strings.Contains(out, "уже установлен") {
		t.Fatal(out)
	}

	if _, out, _ := run(t, "", "-C", dir, "guard", "log"); !strings.Contains(out, "Журнал пуст") {
		t.Fatal(out)
	}

	t.Setenv("CLAUDE_PROJECT_DIR", dir) // t.Setenv восстановит переменную после теста
	for _, cmd := range []string{"rm -rf ~", "ls", "git push"} {
		in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]string{"command": cmd}})
		code, out, _ := run(t, string(in), "guard", "check")
		if code != 0 {
			t.Fatalf("check %q: код %d", cmd, code)
		}
		if cmd == "rm -rf ~" && !strings.Contains(out, `"deny"`) {
			t.Fatalf("check %q: %s", cmd, out)
		}
	}

	_, out, _ = run(t, "", "-C", dir, "guard", "log")
	// В autonomous-режиме вне git-репозитория ветку не определить → deny с подсказкой.
	for _, want := range []string{"deny", "rm-home", "git-push-branch", "allow", "ПРАВИЛО"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в журнале нет %q:\n%s", want, out)
		}
	}
	// Подсказка из CLAUDE.md видна в журнале.
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("- Never edit `db/` by hand.\n"), 0o644)
	in, _ := json.Marshal(map[string]any{"tool_name": "Read", "cwd": dir, "tool_input": map[string]string{"file_path": filepath.Join(dir, "db", "x.sql")}})
	if _, out, _ := run(t, string(in), "guard", "check"); !strings.Contains(out, "additionalContext") {
		t.Fatalf("нет подсказки: %s", out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "log"); !strings.Contains(out, "подсказка: CLAUDE.md:1") {
		t.Fatalf("подсказки нет в журнале:\n%s", out)
	}
	_, out, _ = run(t, "", "-C", dir, "guard", "log", "--decision", "allow", "--json")
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, `"ls"`) || !strings.Contains(out, `"context":["CLAUDE.md:1"]`) {
		t.Fatal(out)
	}
	_, out, _ = run(t, "", "-C", dir, "guard", "log", "-n", "1", "--json")
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "x.sql") {
		t.Fatal(out)
	}
}

func TestGuardSnapshotCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git не установлен")
	}
	dir := t.TempDir()
	gitCmd := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	gitCmd("init", "-q")
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	gitCmd("add", "-A")
	gitCmd("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")

	if code, _, errOut := run(t, "", "-C", dir, "guard", "diff"); code != ExitFailure || !strings.Contains(errOut, "guard start") {
		t.Fatalf("diff без снапшота: %d %s", code, errOut)
	}
	if code, out, _ := run(t, "", "-C", dir, "guard", "start"); code != 0 || !strings.Contains(out, "Снапшот") {
		t.Fatalf("start: %d %s", code, out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "diff"); !strings.Contains(out, "Изменений") {
		t.Fatal(out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "rollback"); !strings.Contains(out, "откатывать нечего") {
		t.Fatal(out)
	}

	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // broken by agent\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("junk\n"), 0o644)

	if _, out, _ := run(t, "", "-C", dir, "guard", "diff"); !strings.Contains(out, "broken by agent") || !strings.Contains(out, "junk.txt") {
		t.Fatal(out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "diff", "--stat"); !strings.Contains(out, "2 files changed") {
		t.Fatal(out)
	}
	// Отказ в подтверждении — ничего не меняется.
	if _, out, _ := run(t, "n\n", "-C", dir, "guard", "rollback"); !strings.Contains(out, "Отменено") {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); err != nil {
		t.Fatal("после отказа файл должен остаться")
	}
	code, out, _ := run(t, "y\n", "-C", dir, "guard", "rollback")
	if code != 0 || !strings.Contains(out, "восстановлено 1, удалено 1") {
		t.Fatalf("rollback: %d %s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(data) != "package main\n" {
		t.Fatalf("main.go = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Fatal("junk.txt должен быть удалён")
	}
	if code, _, _ := run(t, "", "-C", dir, "guard", "rollback", "--yes", "--snapshot", "missing"); code != ExitFailure {
		t.Fatal("неизвестный снапшот должен давать ошибку")
	}
	if code, _, _ := run(t, "", "-C", t.TempDir(), "guard", "start"); code != ExitFailure {
		t.Fatal("start вне git должен падать")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("привет\nмир", 20); got != "привет ⏎ мир" {
		t.Error(got)
	}
	if got := truncate("абвгдеёжз", 5); got != "абвг…" {
		t.Error(got)
	}
}
