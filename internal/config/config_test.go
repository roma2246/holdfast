package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsAndTemplate(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lint.MaxTokens != DefaultMaxTokens || cfg.Guard.MaxDeletes != DefaultMaxDeletes || len(cfg.Lint.Files) != 3 {
		t.Fatalf("defaults: %+v", cfg)
	}

	if _, err := WriteTemplate(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTemplate(dir, false); err == nil {
		t.Fatal("повторный init без --force должен падать")
	}
	if _, err := WriteTemplate(dir, true); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Guard.AskCommands) != 2 || cfg.Guard.DenyCommands[0] != "git push --force" {
		t.Fatalf("template: %+v", cfg.Guard)
	}
}

func TestLoadExampleFromSpec(t *testing.T) {
	dir := t.TempDir()
	spec := `[lint]
files = ["CLAUDE.md", "AGENTS.md"]
max_tokens = 2000
disable = ["HF007"]

[guard]
protected_paths = ["migrations/**"]
deny_commands = ["git push --force"]
ask_commands = ["git push", "npm publish"]
max_deletes = 5
`
	os.WriteFile(filepath.Join(dir, FileName), []byte(spec), 0o644)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsDisabled("HF007") || cfg.IsDisabled("HF001") || len(cfg.Lint.Files) != 2 || cfg.Guard.ProtectedPaths[0] != "migrations/**" {
		t.Fatalf("%+v", cfg)
	}
	g, err := LoadGuard(dir)
	if err != nil || g.MaxDeletes != 5 || len(g.AskCommands) != 2 {
		t.Fatalf("LoadGuard: %+v %v", g, err)
	}
}

func TestLoadInvalid(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("[lint\nbroken"), 0o644)
	if _, err := Load(dir); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if _, err := LoadGuard(dir); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	g, err := LoadGuard(t.TempDir()) // нет файла — не ошибка
	if err != nil || g.MaxDeletes != DefaultMaxDeletes {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestWriteSynced(t *testing.T) {
	dir := t.TempDir()
	orig := "# мой комментарий\n[lint]\nmax_tokens = 1000\n\n[guard]\nprotected_paths = [\"db/**\"]\n"
	os.WriteFile(filepath.Join(dir, FileName), []byte(orig), 0o644)

	rules := []SyncedRule{
		{Kind: KindDenyPath, Pattern: "migrations/**", Source: "CLAUDE.md:3", Origin: "explicit"},
		{Kind: KindAskCommand, Pattern: `say "hi"\now`, Source: "CLAUDE.md:4", Origin: "heuristic"},
	}
	for i := 0; i < 2; i++ { // повторный sync заменяет секцию, а не дублирует
		if err := WriteSynced(dir, rules); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, FileName))
	text := string(data)
	if !strings.Contains(text, "# мой комментарий") || strings.Count(text, "[[guard.synced.rules]]") != 2 {
		t.Fatalf("файл:\n%s", text)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lint.MaxTokens != 1000 || len(cfg.Guard.Synced.Rules) != 2 || cfg.Guard.Synced.Rules[1].Pattern != `say "hi"\now` {
		t.Fatalf("%+v", cfg.Guard.Synced)
	}

	// Пустой список удаляет секцию.
	if err := WriteSynced(dir, nil); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(dir)
	if len(cfg.Guard.Synced.Rules) != 0 {
		t.Fatal("секция должна исчезнуть")
	}
}

func TestWriteSyncedNoConfig(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSynced(dir, []SyncedRule{{Kind: KindDenyPath, Pattern: "x/**", Source: "A:1", Origin: "explicit"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil || len(cfg.Guard.Synced.Rules) != 1 || len(cfg.Guard.AskCommands) == 0 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if quote("a\tb\x01") != `"a\tb\u0001"` {
		t.Error(quote("a\tb\x01"))
	}
}
